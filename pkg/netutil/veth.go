// Package netutil sets up the Linux networking for endpoints: veth pairs,
// routing and NAT.
package netutil

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"slices"
	"strings"
	"syscall"

	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"

	"github.com/matchory/tslink/pkg/logger"
)

// DefaultMTU is the veth MTU when the network sets none.
const DefaultMTU = 1500

// Seam for tests: bringing up a new veth.
var linkSetUp = netlink.LinkSetUp

// CreateVethPair creates a veth pair with the given base name and MTU (0 for
// DefaultMTU). Returns the host-side name and container-side name. A link
// with the host-side name and no address in tslink's veth range is a leftover
// and replaced; one with such an address belongs to a live endpoint, and
// CreateVethPair fails rather than delete it.
func CreateVethPair(baseName string, mtu int) (string, string, error) {
	if mtu == 0 {
		mtu = DefaultMTU
	}
	hostName := "veth" + baseName
	containerName := "veth" + baseName + "c"

	if err := removeLeftoverVeth(hostName); err != nil {
		return "", "", err
	}

	veth := &netlink.Veth{
		Name:     hostName,
		MTU:      mtu,
		PeerName: containerName,
	}
	if err := netlink.LinkAdd(veth); err != nil {
		return "", "", fmt.Errorf("failed to create veth pair: %w", err)
	}
	if err := linkSetUp(veth); err != nil {
		if delErr := DeleteVeth(hostName); delErr != nil {
			logger.Warnf("failed to delete veth %s after error: %v", hostName, delErr)
		}
		return "", "", fmt.Errorf("failed to bring up host veth: %w", err)
	}

	logger.Debugf("Created veth pair: %s <-> %s", hostName, containerName)

	return hostName, containerName, nil
}

// removeLeftoverVeth deletes the link name unless it has an address in
// tslink's veth range: Join assigns one before it returns, so a link without
// one is left over from a Join that failed.
func removeLeftoverVeth(name string) error {
	link, err := netlink.LinkByName(name)
	if _, ok := errors.AsType[netlink.LinkNotFoundError](
		err,
	); ok ||
		errors.Is(err, syscall.ENODEV) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to get %s: %w", name, err)
	}
	addrs, err := netlink.AddrList(link, syscall.AF_INET)
	if err != nil {
		return fmt.Errorf("failed to list addresses of %s: %w", name, err)
	}
	for _, a := range addrs {
		if ip, ok := netip.AddrFromSlice(a.IP.To4()); ok && vethRange.Contains(ip) {
			return fmt.Errorf(
				"veth %s exists with %s: another endpoint's ID starts with the same 8 characters",
				name, ip,
			)
		}
	}
	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("failed to delete leftover veth %s: %w", name, err)
	}
	return nil
}

// MoveToNetNS moves a network interface to the specified network namespace.
func MoveToNetNS(ifName string, nsPath string) error {
	link, err := netlink.LinkByName(ifName)
	if err != nil {
		return fmt.Errorf("interface %s not found: %w", ifName, err)
	}
	ns, opened, err := openNetNS(nsPath)
	if err != nil {
		return err
	}
	defer ns.Close()
	if opened != nsPath {
		logger.Warnf("Docker passed %s but the namespace is at %s: "+
			"/var/run may not be a link to /run on this system", nsPath, opened)
	}
	if err := netlink.LinkSetNsFd(link, int(ns)); err != nil {
		return fmt.Errorf("failed to move %s to netns %s: %w", ifName, opened, err)
	}
	return nil
}

// DeleteVeth deletes a veth interface.
func DeleteVeth(name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		// Check if interface simply doesn't exist (not found)
		if _, ok := errors.AsType[netlink.LinkNotFoundError](err); ok {
			return nil // Already gone, nothing to delete
		}
		// Check for syscall "no such device" error
		if errors.Is(err, syscall.ENODEV) {
			return nil // Already gone
		}
		return fmt.Errorf("failed to get veth %s: %w", name, err)
	}

	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("failed to delete veth %s: %w", name, err)
	}

	logger.Debugf("Deleted veth %s", name)
	return nil
}

// SetupHostRouting sets up routing on the host side for internet access.
func SetupHostRouting(vethHost string, hostIP string) error {
	logger.Debugf("Setting up host routing for %s with IP %s", vethHost, hostIP)

	link, err := netlink.LinkByName(vethHost)
	if err != nil {
		return fmt.Errorf("failed to get host veth: %w", err)
	}

	addr, err := netlink.ParseAddr(hostIP + "/30")
	if err != nil {
		return fmt.Errorf("failed to parse host IP: %w", err)
	}

	if err := netlink.AddrAdd(link, addr); err != nil {
		return fmt.Errorf("failed to add %s to %s: %w", addr, vethHost, err)
	}

	return nil
}

// vethRange holds the subnets of tslink's veth pairs.
var vethRange = netip.MustParsePrefix("10.200.0.0/16")

// UsedVethSubnets returns the /30 subnets in tslink's veth range that links
// of the current network namespace have addresses in.
func UsedVethSubnets() (map[netip.Prefix]bool, error) {
	addrs, err := netlink.AddrList(nil, syscall.AF_INET)
	if err != nil {
		return nil, fmt.Errorf("failed to list addresses: %w", err)
	}
	used := map[netip.Prefix]bool{}
	for _, a := range addrs {
		ip, ok := netip.AddrFromSlice(a.IP.To4())
		if ok && vethRange.Contains(ip) {
			used[netip.PrefixFrom(ip, 30).Masked()] = true
		}
	}
	return used, nil
}

// VethIPv4 returns the IPv4 address in tslink's veth range of the link name.
func VethIPv4(name string) (string, error) {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return "", fmt.Errorf("failed to get %s: %w", name, err)
	}
	addrs, err := netlink.AddrList(link, syscall.AF_INET)
	if err != nil {
		return "", fmt.Errorf("failed to list addresses of %s: %w", name, err)
	}
	for _, a := range addrs {
		if ip, ok := netip.AddrFromSlice(a.IP.To4()); ok && vethRange.Contains(ip) {
			return ip.String(), nil
		}
	}
	return "", fmt.Errorf("%s has no address in %s", name, vethRange)
}

// chainName is the custom iptables chain for tslink forwarding rules.
const chainName = "TSLINK-FORWARD"

// natSource is the veth range tslink masquerades.
const natSource = "10.200.0.0/16"

// ipForwardPath is the IPv4 forwarding sysctl.
const ipForwardPath = "/proc/sys/net/ipv4/ip_forward"

// SetupNAT sets up forwarding and MASQUERADE for traffic from the container,
// in the chain TSLINK-FORWARD. vethHost must have its address: the
// container's tailscaled gets its own external port (see portMapChain).
func SetupNAT(vethHost string) error {
	return defaultHost.setupNAT(vethHost)
}

// CleanupNAT removes the FORWARD rules and the port mapping for a specific
// veth interface, and the port mappings of veths that are gone.
// The global MASQUERADE rule and chain structure are intentionally left in place.
func CleanupNAT(vethHost string) error {
	return defaultHost.cleanupNAT(vethHost)
}

func (s *hostState) setupNAT(vethHost string) error {
	logger.Debugf("Setting up NAT for %s", vethHost)

	// Enable IP forwarding (idempotent)
	if err := enableIPForward(); err != nil {
		logger.Warnf("Failed to enable IP forwarding: %v", err)
	}

	f, err := s.family(iptables.ProtocolIPv4)
	if err != nil {
		return err
	}

	s.forwardMu.Lock()
	err = ensureForwarding(f, []string{vethHost}, false)
	s.forwardMu.Unlock()
	if err != nil {
		return err
	}

	// One external port for tailscaled, whatever the destination
	if err := s.setupPortMapping(f, vethHost); err != nil {
		return fmt.Errorf("failed to map the WireGuard port behind %s: %w", vethHost, err)
	}

	logger.Debugf("NAT setup complete for %s", vethHost)
	return nil
}

// enableIPForward turns on IPv4 forwarding unless it is on already.
func enableIPForward() error {
	if b, err := os.ReadFile(ipForwardPath); err == nil && strings.TrimSpace(string(b)) == "1" {
		return nil
	}
	return os.WriteFile(ipForwardPath, []byte("1"), 0o600)
}

// ensureForwarding puts in place what any container's traffic needs:
// TSLINK-FORWARD and the jump to it, MASQUERADE of the veth range, and the
// forwarding rules of the veths named. With exact, TSLINK-FORWARD then holds
// the rules of those veths only. It runs for each new veth and on every
// watchdog run, so rules a firewall reload or "iptables -F" removed come back.
// Missing rules stop traffic rather than let it through, so they need no
// atomic write. It lists the filter and nat tables once each. The caller
// holds forwardMu.
func ensureForwarding(f *ipFamily, veths []string, exact bool) error {
	filter, err := f.list("filter")
	if err != nil {
		return err
	}
	nat, err := f.list("nat")
	if err != nil {
		return err
	}
	// Rules missing from a chain that exists were removed: worth a warning,
	// unlike setting them up the first time
	exists := filter.chains[chainName]
	if !exists {
		if err := f.NewChain("filter", chainName); err != nil {
			return fmt.Errorf("failed to create chain %s: %w", chainName, err)
		}
	}
	base := []struct {
		table, chain string
		rule         []string
		first        bool
		listed       []string
	}{
		{"filter", forwardChain, []string{"-j", chainName}, true, filter.rules[forwardChain]},
		{
			"nat", "POSTROUTING",
			[]string{"-s", natSource, "-j", "MASQUERADE"},
			false,
			nat.rules["POSTROUTING"],
		},
	}
	for _, r := range base {
		ok, err := hasRule(f, r.table, r.chain, r.listed, r.rule)
		if err != nil {
			return err
		}
		if ok {
			continue
		}
		if exists {
			logger.Warnf("Restoring %s %s", r.chain, strings.Join(r.rule, " "))
		}
		if r.first {
			err = f.Insert(r.table, r.chain, 1, r.rule...)
		} else {
			err = f.Append(r.table, r.chain, r.rule...)
		}
		if err != nil {
			return fmt.Errorf("failed to add %s %s: %w", r.chain, strings.Join(r.rule, " "), err)
		}
	}
	return syncVethRules(f, filter.rules[chainName], veths, exact)
}

// syncVethRules adds the forwarding rules of veths to TSLINK-FORWARD, whose
// rules are listed, and, with exact, removes those of other interfaces.
func syncVethRules(f *ipFamily, listed, veths []string, exact bool) error {
	var order []string
	want := map[string][]string{}
	for _, veth := range veths {
		for _, dir := range []string{"-i", "-o"} {
			rule := []string{dir, veth, "-j", "ACCEPT"}
			line := "-A " + chainName + " " + strings.Join(rule, " ")
			order = append(order, line)
			want[line] = rule
		}
	}
	var errs []error
	for _, line := range listed {
		rule, ok := strings.CutPrefix(line, "-A "+chainName+" ")
		if !ok {
			continue
		}
		if _, keep := want[line]; keep {
			delete(want, line)
			continue
		}
		if exact {
			logger.Debugf("Removing %s %s", chainName, rule)
			if err := f.Delete("filter", chainName, strings.Fields(rule)...); err != nil {
				errs = append(errs, fmt.Errorf("failed to remove %s %s: %w", chainName, rule, err))
			}
		}
	}
	for _, line := range order {
		if rule, ok := want[line]; ok {
			if err := f.Append("filter", chainName, rule...); err != nil {
				errs = append(errs, fmt.Errorf("failed to add %s: %w", line, err))
			}
		}
	}
	return errors.Join(errs...)
}

// syncForwarding makes TSLINK-FORWARD hold the forwarding rules of the veths
// in tslink's veth range, and restores the jump to it and MASQUERADE.
func (s *hostState) syncForwarding(f *ipFamily) error {
	s.forwardMu.Lock()
	defer s.forwardMu.Unlock()
	veths, err := tslinkVeths()
	if err != nil {
		return err
	}
	return ensureForwarding(f, veths, true)
}

// tslinkVeths returns the links of the current network namespace with an
// address in tslink's veth range, the host ends of its veths, sorted.
func tslinkVeths() ([]string, error) {
	addrs, err := netlink.AddrList(nil, syscall.AF_INET)
	if err != nil {
		return nil, fmt.Errorf("failed to list addresses: %w", err)
	}
	var names []string
	for _, a := range addrs {
		ip, ok := netip.AddrFromSlice(a.IP.To4())
		if !ok || !vethRange.Contains(ip) {
			continue
		}
		link, err := netlink.LinkByIndex(a.LinkIndex)
		if err != nil {
			return nil, fmt.Errorf("failed to get link %d: %w", a.LinkIndex, err)
		}
		names = append(names, link.Attrs().Name)
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

func (s *hostState) cleanupNAT(vethHost string) error {
	logger.Debugf("Cleaning up NAT rules for %s", vethHost)

	f, err := s.family(iptables.ProtocolIPv4)
	if err != nil {
		logger.Debugf(
			"Failed to initialize iptables, NAT rules for %s not cleaned: %v",
			vethHost,
			err,
		)
		return nil
	}

	// Remove FORWARD rules from our custom chain (ignore errors if rules don't exist)
	s.forwardMu.Lock()
	if err := f.DeleteIfExists("filter", chainName, "-i", vethHost, "-j", "ACCEPT"); err != nil {
		logger.Debugf("FORWARD -i rule not found for %s (already cleaned): %v", vethHost, err)
	}

	if err := f.DeleteIfExists("filter", chainName, "-o", vethHost, "-j", "ACCEPT"); err != nil {
		logger.Debugf("FORWARD -o rule not found for %s (already cleaned): %v", vethHost, err)
	}
	s.forwardMu.Unlock()

	if err := s.removePortMapping(f, vethHost); err != nil {
		logger.Warnf("Failed to remove the WireGuard port mapping behind %s: %v", vethHost, err)
	}

	logger.Debugf("NAT cleanup complete for %s", vethHost)
	return nil
}
