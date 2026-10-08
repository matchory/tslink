package netutil

import (
	"errors"
	"fmt"
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
	isolateForwardChain = "TSLINK-ISOLATE-FWD"
	isolateInputChain   = "TSLINK-ISOLATE-IN"
)

// Veth isolation keeps containers from reaching each other through tslink's
// veths. The host forwards between the veths' /30s in vethRange, so without
// it a container reaches another tslink container's veth address, past
// Docker's network isolation and past the tailnet's ACLs, with an ordinary
// socket through its Docker gateway or with raw frames through its veth. Only
// tailscaled's WireGuard port passes, so co-located nodes keep their direct
// path instead of falling back to DERP; WireGuard drops what is not from a
// peer. It does not depend on TSLINK_ISOLATE_HOST_TAILNET.
const vethIsolateChain = "TSLINK-VETH-FWD"

// dropTarget is the target of the isolation rules that drop traffic.
const dropTarget = "DROP"

// tslinkChains are the chains tslink jumps to first from a built-in chain;
// any order among them counts as first.
var tslinkChains = []string{vethIsolateChain, isolateForwardChain, isolateInputChain}

// containerInterfaces are the host interfaces container traffic arrives on:
// Docker's bridges, and the host ends of veth pairs, tslink's among them.
var containerInterfaces = []string{"docker0", "docker_gwbridge", "br-+", "veth+"}

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
	{"ipv4", iptables.ProtocolIPv4, "100.64.0.0/10", "100.100.100.100"},
	{"ipv6", iptables.ProtocolIPv6, "fd7a:115c:a1e0::/48", "fd7a:115c:a1e0::53"},
}

// rules returns the rules of chain. Forwarded traffic may reach the host
// tailscaled's DNS resolver only: Docker passes a host resolv.conf that names
// it on to containers, and they would lose DNS. That lets a container resolve
// tailnet names, nothing more.
func (f isolationFamily) rules(chain string) [][]string {
	var rules [][]string
	for _, ifc := range containerInterfaces {
		switch chain {
		case isolateForwardChain:
			for _, proto := range []string{"udp", "tcp"} {
				rules = append(rules, []string{
					"-i", ifc, "-o", tailscaleInterfaces, "-d", f.quad100,
					"-p", proto, "--dport", "53", "-j", "RETURN",
				})
			}
			rules = append(rules, []string{"-i", ifc, "-o", tailscaleInterfaces, "-j", dropTarget})
		case isolateInputChain:
			rules = append(rules, []string{"-i", ifc, "-d", f.tailnet, "-j", dropTarget})
		}
	}
	return rules
}

// SetupHostIsolation installs the host's tailnet isolation, or restores it:
// rules another program removed, and jumps it pushed down. It changes nothing
// that is in place, so it can run periodically.
func SetupHostIsolation() error {
	var errs []error
	for _, f := range isolationFamilies {
		if err := setupIsolation(f); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f.name, err))
		}
	}
	return errors.Join(errs...)
}

func setupIsolation(f isolationFamily) error {
	ipt, err := iptables.NewWithProtocol(f.proto)
	if err != nil {
		return fmt.Errorf("failed to initialize iptables: %w", err)
	}
	for _, c := range []struct{ chain, from string }{
		{isolateForwardChain, "FORWARD"},
		{isolateInputChain, "INPUT"},
	} {
		if err := ensureChain(ipt, c.chain, f.rules(c.chain)); err != nil {
			return err
		}
		if err := ensureFirstJump(ipt, c.from, c.chain); err != nil {
			return err
		}
	}
	return nil
}

// ensureChain makes chain in the mangle table hold exactly rules, rewriting
// it only if it does not.
func ensureChain(ipt *iptables.IPTables, chain string, rules [][]string) error {
	exists, err := ipt.ChainExists("mangle", chain)
	if err != nil {
		return fmt.Errorf("failed to check chain %s: %w", chain, err)
	}
	if exists {
		ok, err := chainHolds(ipt, chain, rules)
		if err != nil || ok {
			return err
		}
		logger.Warnf("Restoring the rules of %s", chain)
	}
	// Creates the chain, or empties it
	if err := ipt.ClearChain("mangle", chain); err != nil {
		return fmt.Errorf("failed to clear chain %s: %w", chain, err)
	}
	for _, rule := range rules {
		if err := ipt.Append("mangle", chain, rule...); err != nil {
			return fmt.Errorf("failed to add rule to %s: %w", chain, err)
		}
	}
	return nil
}

// chainHolds reports whether chain holds rules and no others. iptables lists
// rules in its own normal form, so they are checked with -C.
func chainHolds(ipt *iptables.IPTables, chain string, rules [][]string) (bool, error) {
	listed, err := ipt.List("mangle", chain)
	if err != nil {
		return false, fmt.Errorf("failed to list chain %s: %w", chain, err)
	}
	if len(listed) != len(rules)+1 { // and "-N <chain>"
		return false, nil
	}
	for _, rule := range rules {
		ok, err := ipt.Exists("mangle", chain, rule...)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// ensureFirstJump makes a jump to chain the first rule of the built-in chain
// from, so no rule ahead of it accepts the traffic first.
func ensureFirstJump(ipt *iptables.IPTables, from, chain string) error {
	listed, err := ipt.List("mangle", from)
	if err != nil {
		return fmt.Errorf("failed to list chain %s: %w", from, err)
	}
	// After "-P <chain> <policy>", the leading rules may be jumps to any of
	// tslink's chains, so two of them do not keep pushing each other down
	for _, rule := range listed[min(1, len(listed)):] {
		target, ok := strings.CutPrefix(rule, "-A "+from+" -j ")
		if !ok || !slices.Contains(tslinkChains, target) {
			break
		}
		if target == chain {
			return nil
		}
	}
	if err := ipt.DeleteIfExists("mangle", from, "-j", chain); err != nil {
		return fmt.Errorf("failed to move jump to %s: %w", chain, err)
	}
	if err := ipt.Insert("mangle", from, 1, "-j", chain); err != nil {
		return fmt.Errorf("failed to add jump to %s: %w", chain, err)
	}
	return nil
}

// SetupVethIsolation installs the veth isolation, or restores it, letting
// only UDP to port through between containers and tslink's veths. IPv4 only:
// the veths have no other addresses than link-local IPv6 ones. It changes
// nothing that is in place, so it can run periodically.
func SetupVethIsolation(port int) error {
	ipt, err := iptables.NewWithProtocol(iptables.ProtocolIPv4)
	if err != nil {
		return fmt.Errorf("failed to initialize iptables: %w", err)
	}
	dst := vethRange.String()
	var rules [][]string
	for _, ifc := range containerInterfaces {
		rules = append(
			rules,
			[]string{
				"-i",
				ifc,
				"-d",
				dst,
				"-p",
				"udp",
				"--dport",
				strconv.Itoa(port),
				"-j",
				"RETURN",
			},
			[]string{"-i", ifc, "-d", dst, "-j", dropTarget},
		)
	}
	if err := ensureChain(ipt, vethIsolateChain, rules); err != nil {
		return err
	}
	return ensureFirstJump(ipt, "FORWARD", vethIsolateChain)
}

// RemoveHostIsolation removes the host's tailnet isolation, and tolerates its
// absence.
func RemoveHostIsolation() error {
	var errs []error
	for _, f := range isolationFamilies {
		ipt, err := iptables.NewWithProtocol(f.proto)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: failed to initialize iptables: %w", f.name, err))
			continue
		}
		for _, c := range []struct{ chain, from string }{
			{isolateForwardChain, "FORWARD"},
			{isolateInputChain, "INPUT"},
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
