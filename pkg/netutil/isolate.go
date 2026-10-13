package netutil

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/coreos/go-iptables/iptables"

	"github.com/matchory/tslink/pkg/logger"
)

// Host tailnet isolation keeps containers from reaching the tailnet through
// the host's own tailscaled. The host's tailscaled accepts all forwarded
// traffic to its interface (its ts-forward chain), and Docker masquerades
// container traffic, so without it any container reaches the tailnet with the
// host's identity: one on Docker's bridges with an ordinary socket, and one on
// tslink with raw frames (CAP_NET_RAW, which Docker grants by default) past
// the unreachable routes in its namespace. The rules live in the host's
// namespace, where no container can change them, NET_ADMIN or not.
//
// They are in the mangle table, whose FORWARD and INPUT hooks run before the
// filter table's, so no ACCEPT in filter, the host tailscaled's or Docker's,
// gets to the traffic first. Only traffic arriving on container interfaces is
// dropped: a subnet router or exit node on the host still forwards from the
// LAN.
const (
	IsolateForwardChain = "TSLINK-ISOLATE-FWD"
	IsolateInputChain   = "TSLINK-ISOLATE-IN"
)

// VethIsolateChain holds the veth isolation, which keeps containers from
// reaching each other through tslink's veths. The host forwards between the veths' /30s in vethRange, so without
// it a container reaches another tslink container's veth address, past
// Docker's network isolation and past the tailnet's ACLs, with an ordinary
// socket through its Docker gateway or with raw frames through its veth.
// Nothing opens a connection to a veth address but a colocated node's
// tailscaled, to tailscaled's WireGuard port through its own veth, so
// colocated nodes keep their direct path instead of falling back to DERP;
// WireGuard drops what is not from a peer. Everything else forwarded there is
// dropped unless it answers a connection the container opened, whatever
// interface it arrives on: a stack file names a bridge as it likes, and a LAN
// host may route vethRange through the host. It does not depend on
// TSLINK_ISOLATE_HOST_TAILNET.
const VethIsolateChain = "TSLINK-VETH-FWD"

// WireGuardInputChain makes the external ports of containers' tailscaled
// (portMapChain) take only packets on flows the containers opened, which the
// host forwards. A packet
// that arrives first, from a peer probing a port the container advertises,
// has no NAT to undo and goes to the host itself: conntrack keeps an entry
// for it, whose reply is the tuple the container's own packets to that peer
// need, and as each container has a single port, they are dropped for as
// long as the peer keeps probing. Dropping such packets before conntrack
// confirms them leaves no entry behind; the container's first packet to the
// peer then opens the path for the peer's next. It does not depend on
// TSLINK_ISOLATE_HOST_TAILNET.
const WireGuardInputChain = "TSLINK-WG-IN"

// wireGuardInputRule drops new UDP to the ports in r.
func wireGuardInputRule(r PortRange) []string {
	return []string{
		"-p", udp, "-m", udp, dportFlag, fmt.Sprintf("%d:%d", r.First, r.Last),
		"-m", "conntrack", "--ctstate", "NEW", "-j", dropTarget,
	}
}

// The built-in chains tslink jumps from.
const (
	forwardChain = "FORWARD"
	inputChain   = "INPUT"
)

// dportFlag is iptables' destination port match flag.
const dportFlag = "--dport"

// dropTarget is the target of the isolation rules that drop traffic.
const dropTarget = "DROP"

// returnTarget is the target of the isolation rules that let traffic pass.
const returnTarget = "RETURN"

// tslinkChains are the chains tslink jumps to first from a built-in chain;
// any order among them counts as first.
var tslinkChains = []string{
	VethIsolateChain, WireGuardInputChain, IsolateForwardChain, IsolateInputChain,
}

// containerInterfaces are the host interfaces container traffic arrives on:
// Docker's bridges, and the host ends of veth pairs, tslink's among them. A
// bridge network may name its bridge otherwise: the host's tailnet isolation
// takes those names from Docker.
var containerInterfaces = []string{"docker0", "docker_gwbridge", "br-+", "veth+"}

// bridgeName matches the bridge names the host's tailnet isolation takes. A
// "+" would make a name an iptables pattern for other interfaces too.
var bridgeName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`)

// isolatedInterfaces returns containerInterfaces and the bridges they do not
// match, in a stable order. Names that are no interface name are left out.
func isolatedInterfaces(bridges []string) []string {
	ifcs := slices.Clone(containerInterfaces)
	for _, b := range slices.Sorted(slices.Values(bridges)) {
		if !bridgeName.MatchString(b) {
			logger.Warnf("Host tailnet isolation: ignoring bridge %q, not an interface name", b)
			continue
		}
		if !matchesInterface(ifcs, b) {
			ifcs = append(ifcs, b)
		}
	}
	return ifcs
}

// matchesInterface reports whether one of the iptables interface patterns
// matches name: a trailing "+" matches any name with that prefix.
func matchesInterface(patterns []string, name string) bool {
	for _, p := range patterns {
		prefix, wildcard := strings.CutSuffix(p, "+")
		if wildcard && strings.HasPrefix(name, prefix) || !wildcard && p == name {
			return true
		}
	}
	return false
}

// tailscaleInterfaces matches the host tailscaled's interface, tailscale0 by
// default.
const tailscaleInterfaces = "tailscale+"

// isolationFamily holds what the rules need of one address family.
type isolationFamily struct {
	name    string
	proto   iptables.Protocol
	tailnet string // Tailscale's address range, the host's own addresses among it
	quad100 string // Tailscale's DNS resolver
}

var isolationFamilies = []isolationFamily{
	{"ipv4", iptables.ProtocolIPv4, tailnetV4.String(), "100.100.100.100"},
	{"ipv6", iptables.ProtocolIPv6, tailnetV6.String(), "fd7a:115c:a1e0::53"},
}

// rules returns the rules of chain. Forwarded traffic may reach the host
// tailscaled's DNS resolver only: Docker passes a host resolv.conf that names
// it on to containers, and they would lose DNS. That lets a container resolve
// tailnet names, nothing more.
func (f isolationFamily) rules(chain string, ifcs []string) [][]string {
	var rules [][]string
	for _, ifc := range ifcs {
		switch chain {
		case IsolateForwardChain:
			for _, proto := range []string{"udp", "tcp"} {
				rules = append(rules, []string{
					"-i", ifc, "-o", tailscaleInterfaces, "-d", f.quad100,
					"-p", proto, dportFlag, "53", "-j", returnTarget,
				})
			}
			rules = append(rules, []string{"-i", ifc, "-o", tailscaleInterfaces, "-j", dropTarget})
		case IsolateInputChain:
			rules = append(rules, []string{"-i", ifc, "-d", f.tailnet, "-j", dropTarget})
		}
	}
	return rules
}

// SetupHostIsolation installs the host's tailnet isolation, or restores it:
// rules another program removed, and jumps it pushed down. bridges are the
// interface names of Docker's bridge networks, which containerInterfaces may
// not match. It changes nothing that is in place, so it can run periodically.
func SetupHostIsolation(bridges []string) error {
	return defaultHost.setupHostIsolation(bridges)
}

// SetupVethIsolation installs the veth isolation, or restores it, letting
// only replies and UDP to port from tslink's veths through to them. It also
// installs TSLINK-WG-IN, restores the veths' forwarding and MASQUERADE
// (syncForwarding) and syncs their WireGuard port mappings
// (SyncPortMappings). IPv4 only: the veths have no other addresses than
// link-local IPv6 ones. It changes nothing that is in place, so it can run
// periodically.
func SetupVethIsolation(port int) error {
	return defaultHost.setupVethIsolation(port)
}

func (s *hostState) setupHostIsolation(bridges []string) error {
	ifcs := isolatedInterfaces(bridges)
	var errs []error
	for _, f := range isolationFamilies {
		if err := s.setupIsolation(f, ifcs); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f.name, err))
		}
	}
	return errors.Join(errs...)
}

func (s *hostState) setupIsolation(fam isolationFamily, ifcs []string) error {
	f, err := s.family(fam.proto)
	if err != nil {
		return err
	}
	mangle, err := f.list("mangle")
	if err != nil {
		return err
	}
	for _, c := range []struct{ chain, from string }{
		{IsolateForwardChain, forwardChain},
		{IsolateInputChain, inputChain},
	} {
		mangle, err = s.ensureFirstChain(f, mangle, c.from, c.chain, fam.rules(c.chain, ifcs))
		if err != nil {
			return err
		}
	}
	return nil
}

// ensureFirstChain makes chain in the mangle table hold exactly rules, and a
// jump to it the first rule of the built-in chain from, so no rule ahead of it
// accepts the traffic first. mangle is the table as listed, and it returns the
// table as it is then. What is not in place is written in one
// iptables-restore: emptying the chain and adding its rules one by one, or
// deleting the jump and inserting it again, would let through for a moment
// what the chain drops.
func (s *hostState) ensureFirstChain(
	f *ipFamily, mangle tableRules, from, chain string, rules [][]string,
) (tableRules, error) {
	w := chainWrite{table: "mangle", chain: chain, rules: rules, setRules: true}
	if mangle.chains[chain] {
		ok, err := s.chainHolds(f, "mangle", chain, mangle.rules[chain], rules)
		if err != nil {
			return mangle, err
		}
		if !ok {
			logger.Warnf("Restoring the rules of %s", chain)
		}
		w.setRules = !ok
	}
	first, jumps := jumpPosition(mangle.rules[from], from, chain)
	if !first {
		w.from, w.jumpExists = from, jumps
	}
	if !w.setRules && w.from == "" {
		return mangle, nil
	}
	if err := f.apply(w); err != nil {
		return mangle, fmt.Errorf("failed to write %s: %w", chain, err)
	}
	// What it wrote, as iptables lists it, for the next check
	mangle, err := f.list("mangle")
	if err != nil {
		return mangle, err
	}
	if lines := mangle.rules[chain]; w.setRules && len(lines) == len(rules) {
		s.remember(f, "mangle", chain, lines, rules)
	}
	return mangle, nil
}

// jumpPosition reports whether the built-in chain from, whose rules are
// listed, jumps to chain first, and whether it jumps to it at all. The
// leading rules may be jumps to any of tslink's chains, so two of them do not
// keep pushing each other down.
func jumpPosition(listed []string, from, chain string) (bool, bool) {
	return slices.Contains(LeadingJumps(listed, from), chain),
		slices.Contains(listed, "-A "+from+" -j "+chain)
}

// LeadingJumps returns the chains of the leading run of jumps to tslink's
// chains among the rules of the built-in chain from, as iptables -S lists
// them; its policy line is skipped.
func LeadingJumps(rules []string, from string) []string {
	var jumps []string
	for _, rule := range rules {
		rule = strings.TrimSpace(rule)
		if strings.HasPrefix(rule, "-P ") {
			continue
		}
		target, ok := strings.CutPrefix(rule, "-A "+from+" -j ")
		if !ok || !slices.Contains(tslinkChains, target) {
			break
		}
		jumps = append(jumps, target)
	}
	return jumps
}

func (s *hostState) setupVethIsolation(port int) error {
	f, err := s.family(iptables.ProtocolIPv4)
	if err != nil {
		return err
	}
	mangle, err := f.list("mangle")
	if err != nil {
		return err
	}
	dst := vethRange.String()
	rules := [][]string{
		// Replies on connections the container opened, as tailscaled's
		{"-d", dst, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", returnTarget},
		// WireGuard from a colocated node's tailscaled, which uses its veth
		{"-i", "veth+", "-d", dst, "-p", "udp", dportFlag, strconv.Itoa(port), "-j", returnTarget},
		{"-d", dst, "-j", dropTarget},
	}
	mangle, err = s.ensureFirstChain(f, mangle, forwardChain, VethIsolateChain, rules)
	if err != nil {
		return err
	}
	input := [][]string{wireGuardInputRule(s.configuredPorts())}
	if _, err := s.ensureFirstChain(f, mangle, inputChain, WireGuardInputChain, input); err != nil {
		return err
	}
	// So do the forwarding and MASQUERADE the veths need, which a firewall
	// reload removes as well
	if err := s.syncForwarding(f); err != nil {
		return fmt.Errorf("forwarding: %w", err)
	}
	// It runs when the plugin starts and on every watchdog run, as the port
	// mappings of the veths need to
	if err := s.syncPortMappings(f); err != nil {
		return fmt.Errorf("WireGuard port mappings: %w", err)
	}
	return nil
}

// RemoveHostIsolation removes the host's tailnet isolation, and tolerates its
// absence.
func RemoveHostIsolation() error {
	var errs []error
	for _, f := range isolationFamilies {
		ipt, err := defaultHost.family(f.proto)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f.name, err))
			continue
		}
		for _, c := range []struct{ chain, from string }{
			{IsolateForwardChain, forwardChain},
			{IsolateInputChain, inputChain},
		} {
			// iptables cannot look for a jump to a chain that does not exist,
			// and there is none
			exists, err := ipt.ChainExists("mangle", c.chain)
			if err != nil || !exists {
				if err != nil {
					errs = append(
						errs,
						fmt.Errorf("%s: failed to check chain %s: %w", f.name, c.chain, err),
					)
				}
				continue
			}
			if err := ipt.DeleteIfExists("mangle", c.from, "-j", c.chain); err != nil {
				errs = append(
					errs,
					fmt.Errorf("%s: failed to remove jump to %s: %w", f.name, c.chain, err),
				)
				continue
			}
			if err := ipt.ClearAndDeleteChain("mangle", c.chain); err != nil {
				errs = append(
					errs,
					fmt.Errorf("%s: failed to remove chain %s: %w", f.name, c.chain, err),
				)
			}
		}
	}
	return errors.Join(errs...)
}
