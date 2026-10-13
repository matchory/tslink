package netutil

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"os/exec"
	"slices"
	"strings"
	"sync"

	"github.com/coreos/go-iptables/iptables"
)

// hostState is what tslink keeps of the host's iptables between calls: an
// iptables of each address family, the chains it found in place, the
// WireGuard port mappings, and the configuration of those and of the host
// endpoints. The exported functions use defaultHost; tests may make their
// own.
type hostState struct {
	familyMu sync.Mutex
	families map[iptables.Protocol]*ipFamily

	checkedMu sync.Mutex
	checked   map[chainKey]checkedChain

	// forwardMu serializes changes to TSLINK-FORWARD: the watchdog's sync
	// must not remove the rules of a veth SetupNAT adds meanwhile.
	forwardMu sync.Mutex

	// portMapMu makes listing portMapChain, choosing a port and adding its
	// rule one step, and guards portMap and ports.
	portMapMu sync.Mutex
	portMap   portMapping        // the mapping SetupNAT installs
	ports     map[netip.Addr]int // portMapChain's mappings, as last written or listed

	hostEndpointsMu sync.Mutex
	hostEndpoints   HostEndpointsConfig

	// forgetFlows deletes the conntrack entries of UDP flows from each of
	// srcs on port sport, and returns how many it deleted.
	forgetFlows func(srcs []netip.Addr, sport int) (uint, error)
}

// defaultHost is the state of the host tslink runs on.
var defaultHost = newHostState()

func newHostState() *hostState {
	return &hostState{
		families:    map[iptables.Protocol]*ipFamily{},
		checked:     map[chainKey]checkedChain{},
		portMap:     portMapping{sport: 41641, ports: DefaultPortRange},
		ports:       map[netip.Addr]int{},
		forgetFlows: forgetUDPFlows,
	}
}

// family returns the iptables of proto, which it sets up on first use: the
// backend does not change while tslink runs.
func (s *hostState) family(proto iptables.Protocol) (*ipFamily, error) {
	s.familyMu.Lock()
	defer s.familyMu.Unlock()
	if f, ok := s.families[proto]; ok {
		return f, nil
	}
	f, err := newIPFamily(proto)
	if err != nil {
		return nil, err
	}
	s.families[proto] = f
	return f, nil
}

// chainHolds reports whether chain in table, whose rules are lines as
// iptables lists them, holds rules and no others. Lines it found to hold
// them before do; others are checked with -C, as iptables lists rules in its
// own normal form, and remembered if they hold them.
func (s *hostState) chainHolds(
	f *ipFamily, table, chain string, lines []string, rules [][]string,
) (bool, error) {
	key := chainKey{f.Proto(), table, chain}
	s.checkedMu.Lock()
	c, ok := s.checked[key]
	s.checkedMu.Unlock()
	if ok && c.holds(lines, rules) {
		return true, nil
	}
	if len(lines) != len(rules) {
		return false, nil
	}
	for _, rule := range rules {
		ok, err := f.Exists(table, chain, rule...)
		if err != nil || !ok {
			return false, err
		}
	}
	s.remember(f, table, chain, lines, rules)
	return true, nil
}

// remember records that chain in table, whose rules are lines, holds rules.
func (s *hostState) remember(f *ipFamily, table, chain string, lines []string, rules [][]string) {
	s.checkedMu.Lock()
	defer s.checkedMu.Unlock()
	s.checked[chainKey{f.Proto(), table, chain}] = checkedChain{
		lines: slices.Clone(lines),
		rules: slices.Clone(rules),
	}
}

// ipFamily runs the iptables of one address family, and its save and restore
// binaries of the same backend.
type ipFamily struct {
	*iptables.IPTables

	save, restore string
}

// newIPFamily finds the iptables of proto and the save and restore binaries
// that use its backend: the plugin image links iptables to iptables-nft, but
// not iptables-save and iptables-restore.
func newIPFamily(proto iptables.Protocol) (*ipFamily, error) {
	ipt, err := iptables.NewWithProtocol(proto)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize iptables: %w", err)
	}
	name := "iptables"
	if proto == iptables.ProtocolIPv6 {
		name = "ip6tables"
	}
	out, err := exec.CommandContext( // #nosec G204 -- a fixed binary
		context.Background(), name, "--version",
	).Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get the version of %s: %w", name, err)
	}
	return &ipFamily{
		IPTables: ipt,
		save:     backendTool(name, string(out), "save"),
		restore:  backendTool(name, string(out), "restore"),
	}, nil
}

// list returns table as iptables-save lists it: one process for all of its
// chains, where iptables would take one for each chain or rule.
func (f *ipFamily) list(table string) (tableRules, error) {
	ctx, cancel := context.WithTimeout(context.Background(), restoreTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, f.save, "-t", table) // #nosec G204 -- a fixed binary
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return tableRules{}, fmt.Errorf(
			"%s -t %s: %w: %s", f.save, table, err, strings.TrimSpace(stderr.String()),
		)
	}
	return parseSave(string(out)), nil
}

// backendTool returns name's tool ("save" or "restore") of the backend the
// version output of name names, as in "iptables v1.8.10 (nf_tables)", if it
// is installed, and otherwise the plain one.
func backendTool(name, version, tool string) string {
	backend := ""
	switch {
	case strings.Contains(version, "(nf_tables)"):
		backend = "-nft"
	case strings.Contains(version, "(legacy)"):
		backend = "-legacy"
	}
	if backend != "" {
		if path, err := exec.LookPath(name + backend + "-" + tool); err == nil {
			return path
		}
	}
	return name + "-" + tool
}

// tableRules is a table as iptables-save lists it: its chains, and the rules
// of each as "-A <chain> ..." lines, in order.
type tableRules struct {
	chains map[string]bool
	rules  map[string][]string
}

// parseSave reads the output of iptables-save for one table.
func parseSave(out string) tableRules {
	t := tableRules{chains: map[string]bool{}, rules: map[string][]string{}}
	for line := range strings.Lines(out) {
		line = strings.TrimSpace(line)
		if name, ok := strings.CutPrefix(line, ":"); ok {
			name, _, _ = strings.Cut(name, " ")
			t.chains[name] = true
			continue
		}
		if f := strings.Fields(line); len(f) > 1 && f[0] == "-A" {
			t.rules[f[1]] = append(t.rules[f[1]], line)
		}
	}
	return t
}

// chainKey names a chain of one address family.
type chainKey struct {
	proto        iptables.Protocol
	table, chain string
}

// checkedChain is a chain's rules as iptables listed them, and the rules
// they were found to be.
type checkedChain struct {
	lines []string
	rules [][]string
}

// holds reports whether lines are the lines c found to be rules.
func (c checkedChain) holds(lines []string, rules [][]string) bool {
	return slices.Equal(c.lines, lines) &&
		slices.EqualFunc(c.rules, rules, slices.Equal)
}

// hasRule reports whether chain in table, whose rules are lines, holds rule:
// a line that reads as rule does, and otherwise iptables' -C decides.
func hasRule(f *ipFamily, table, chain string, lines, rule []string) (bool, error) {
	if slices.Contains(lines, "-A "+chain+" "+strings.Join(rule, " ")) {
		return true, nil
	}
	ok, err := f.Exists(table, chain, rule...)
	if err != nil {
		return false, fmt.Errorf("failed to check %s %s: %w", chain, strings.Join(rule, " "), err)
	}
	return ok, nil
}
