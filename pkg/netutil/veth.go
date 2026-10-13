// Package netutil sets up the Linux networking for endpoints: veth pairs,
// routing and NAT.
package netutil

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/matchory/tslink/pkg/logger"
)

// DefaultMTU is the veth MTU when the network sets none.
const DefaultMTU = 1500

// CreateVethPair creates a veth pair with the given base name and MTU (0 for
// DefaultMTU). Returns the host-side name and container-side name.
func CreateVethPair(baseName string, mtu int) (string, string, error) {
	if mtu == 0 {
		mtu = DefaultMTU
	}
	hostName := "veth" + baseName
	containerName := "veth" + baseName + "c"

	// Delete existing interfaces if they exist
	if link, err := netlink.LinkByName(hostName); err == nil {
		if err := netlink.LinkDel(link); err != nil {
			logger.Warnf("failed to delete existing veth %s: %v", hostName, err)
		}
	}

	veth := &netlink.Veth{
		Name:     hostName,
		MTU:      mtu,
		PeerName: containerName,
	}

	if err := netlink.LinkAdd(veth); err != nil {
		return "", "", fmt.Errorf("failed to create veth pair: %w", err)
	}

	link, err := netlink.LinkByName(hostName)
	if err != nil {
		return "", "", fmt.Errorf("failed to get host veth: %w", err)
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return "", "", fmt.Errorf("failed to bring up host veth: %w", err)
	}

	logger.Debugf("Created veth pair: %s <-> %s", hostName, containerName)

	return hostName, containerName, nil
}

// MoveToNetNS moves a network interface to the specified network namespace.
func MoveToNetNS(ifName string, nsPath string) error {
	logger.Debugf("MoveToNetNS: interface=%s nsPath=%s", ifName, nsPath)

	// Try the given path first, then fallback to alternate path if needed
	// (handles Alpine Linux where /var/run and /run are separate)
	pathsToTry := []string{nsPath}
	if strings.HasPrefix(nsPath, "/var/run/docker/netns/") {
		alternatePath := strings.Replace(nsPath, "/var/run/docker/netns/", "/run/docker/netns/", 1)
		pathsToTry = append(pathsToTry, alternatePath)
	} else if strings.HasPrefix(nsPath, "/run/docker/netns/") {
		alternatePath := strings.Replace(nsPath, "/run/docker/netns/", "/var/run/docker/netns/", 1)
		pathsToTry = append(pathsToTry, alternatePath)
	}

	// Get the interface once before trying paths
	link, err := netlink.LinkByName(ifName)
	if err != nil {
		return fmt.Errorf("interface %s not found: %w", ifName, err)
	}
	logger.Debugf("MoveToNetNS: found interface %s (index=%d, type=%s)",
		ifName, link.Attrs().Index, link.Type())

	var lastErr error
	for i, tryPath := range pathsToTry {
		// Validate the netns path exists and is accessible
		fi, err := os.Lstat(tryPath)
		if err != nil {
			lastErr = fmt.Errorf("netns path %s: %w", tryPath, err)
			continue
		}
		logger.Debugf("MoveToNetNS: path %s exists, mode=%s isSymlink=%v",
			tryPath, fi.Mode().String(), fi.Mode()&os.ModeSymlink != 0)

		// If it's a symlink, resolve it
		realPath := tryPath
		if fi.Mode()&os.ModeSymlink != 0 {
			realPath, err = os.Readlink(tryPath)
			if err != nil {
				lastErr = fmt.Errorf("failed to resolve symlink %s: %w", tryPath, err)
				continue
			}
			logger.Debugf("MoveToNetNS: resolved symlink to %s", realPath)
		}

		// Open the network namespace
		ns, err := netns.GetFromPath(realPath)
		if err != nil {
			lastErr = fmt.Errorf("failed to get netns from %s: %w", realPath, err)
			continue
		}
		logger.Debugf("MoveToNetNS: opened netns fd=%d", int(ns))

		// Verify it's actually a network namespace by checking the fd type
		var stat unix.Stat_t
		if err := unix.Fstat(int(ns), &stat); err != nil {
			logger.Warnf("MoveToNetNS: fstat on netns fd failed: %v", err)
		} else {
			logger.Debugf("MoveToNetNS: netns fd stat: mode=%o", stat.Mode)
		}

		// Move the interface to the namespace
		if err := netlink.LinkSetNsFd(link, int(ns)); err != nil {
			if closeErr := ns.Close(); closeErr != nil { // Clean up fd immediately on failure
				logger.Debugf("MoveToNetNS: failed to close netns fd: %v", closeErr)
			}

			// Log additional debug info on failure
			logger.Errorf("MoveToNetNS: LinkSetNsFd failed: interface=%s fd=%d err=%v",
				ifName, int(ns), err)

			// Check if the interface is still in the current namespace
			if _, checkErr := netlink.LinkByName(ifName); checkErr != nil {
				logger.Debugf("MoveToNetNS: interface no longer in current namespace after error")
			} else {
				logger.Debugf("MoveToNetNS: interface still in current namespace")
			}

			lastErr = fmt.Errorf("failed to move %s to netns (fd=%d, path=%s): %w",
				ifName, int(ns), realPath, err)
			continue
		}

		// Success! Clean up and return
		if err := ns.Close(); err != nil {
			logger.Debugf("MoveToNetNS: failed to close netns fd: %v", err)
		}
		if i > 0 {
			logger.Warnf("MoveToNetNS: WARNING - Docker passed %s but namespace was at %s. "+
				"This may indicate /var/run is not symlinked to /run on your system. "+
				"Consider running: ln -sf /run /var/run", nsPath, tryPath)
		}
		logger.Debugf("Moved interface %s to netns %s", ifName, tryPath)
		return nil
	}

	// All paths failed
	return lastErr
}

// SetupInterfaceInNS sets up an interface inside a network namespace.
func SetupInterfaceInNS(nsPath string, ifName string, newName string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Get current namespace
	origNS, err := netns.Get()
	if err != nil {
		return fmt.Errorf("failed to get current netns: %w", err)
	}
	defer origNS.Close()

	// Switch to target namespace
	targetNS, err := netns.GetFromPath(nsPath)
	if err != nil {
		return fmt.Errorf("failed to get target netns: %w", err)
	}
	defer targetNS.Close()

	if err := netns.Set(targetNS); err != nil {
		return fmt.Errorf("failed to switch to target netns: %w", err)
	}
	defer func() {
		if err := netns.Set(origNS); err != nil {
			logger.Warnf("failed to restore original netns: %v", err)
		}
	}()

	link, err := netlink.LinkByName(ifName)
	if err != nil {
		return fmt.Errorf("interface %s not found in target netns: %w", ifName, err)
	}

	if newName != "" && newName != ifName {
		if err := netlink.LinkSetName(link, newName); err != nil {
			return fmt.Errorf("failed to rename interface: %w", err)
		}
		link, _ = netlink.LinkByName(newName)
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("failed to bring up interface: %w", err)
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

// SetupContainerRouting sets up routing inside the container namespace.
func SetupContainerRouting(
	nsPath string,
	ifName string,
	containerIP string,
	gatewayIP string,
) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Get current namespace
	origNS, err := netns.Get()
	if err != nil {
		return fmt.Errorf("failed to get current netns: %w", err)
	}
	defer origNS.Close()

	// Switch to container namespace
	targetNS, err := netns.GetFromPath(nsPath)
	if err != nil {
		return fmt.Errorf("failed to get target netns: %w", err)
	}
	defer targetNS.Close()

	if err := netns.Set(targetNS); err != nil {
		return fmt.Errorf("failed to switch to target netns: %w", err)
	}
	defer func() {
		if err := netns.Set(origNS); err != nil {
			logger.Warnf("failed to restore original netns: %v", err)
		}
	}()

	link, err := netlink.LinkByName(ifName)
	if err != nil {
		return fmt.Errorf("interface %s not found: %w", ifName, err)
	}

	addr, err := netlink.ParseAddr(containerIP + "/30")
	if err != nil {
		return fmt.Errorf("failed to parse container IP: %w", err)
	}

	if err := netlink.AddrAdd(link, addr); err != nil {
		logger.Debugf("Warning: failed to add IP to container interface: %v", err)
	}

	// No default route: Docker connects the container to its gateway network
	// like any other, and SetupBypassRoute sends tailscaled's own traffic
	// through this veth.
	logger.Debugf("Container routing setup: %s, peer %s", containerIP, gatewayIP)
	return nil
}

// chainName is the custom iptables chain for tslink forwarding rules.
const chainName = "TSLINK-FORWARD"

// natSource is the veth range tslink masquerades.
const natSource = "10.200.0.0/16"

// ipForwardPath is the IPv4 forwarding sysctl.
const ipForwardPath = "/proc/sys/net/ipv4/ip_forward"

// forwardMu serializes changes to TSLINK-FORWARD: the watchdog's sync must
// not remove the rules of a veth SetupNAT adds meanwhile.
var forwardMu sync.Mutex

// SetupNAT sets up forwarding and MASQUERADE for traffic from the container,
// in the chain TSLINK-FORWARD. vethHost must have its address: the
// container's tailscaled gets its own external port (see portMapChain).
func SetupNAT(vethHost string) error {
	logger.Debugf("Setting up NAT for %s", vethHost)

	// Enable IP forwarding (idempotent)
	if err := enableIPForward(); err != nil {
		logger.Warnf("Failed to enable IP forwarding: %v", err)
	}

	ipt, err := iptables.New()
	if err != nil {
		return fmt.Errorf("failed to initialize iptables: %w", err)
	}

	forwardMu.Lock()
	err = ensureForwarding(ipt, []string{vethHost}, false)
	forwardMu.Unlock()
	if err != nil {
		return err
	}

	// One external port for tailscaled, whatever the destination
	if err := setupPortMapping(ipt, vethHost); err != nil {
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
// atomic write. The caller holds forwardMu.
func ensureForwarding(ipt *iptables.IPTables, veths []string, exact bool) error {
	// Rules missing from a chain that exists were removed: worth a warning,
	// unlike setting them up the first time
	exists, err := ipt.ChainExists("filter", chainName)
	if err != nil {
		return fmt.Errorf("failed to check chain %s: %w", chainName, err)
	}
	if !exists {
		if err := ipt.NewChain("filter", chainName); err != nil {
			return fmt.Errorf("failed to create chain %s: %w", chainName, err)
		}
	}
	base := []struct {
		table, chain string
		rule         []string
		first        bool
	}{
		{"filter", forwardChain, []string{"-j", chainName}, true},
		{"nat", "POSTROUTING", []string{"-s", natSource, "-j", "MASQUERADE"}, false},
	}
	for _, r := range base {
		ok, err := ipt.Exists(r.table, r.chain, r.rule...)
		if err != nil {
			return fmt.Errorf("failed to check %s %s: %w", r.chain, strings.Join(r.rule, " "), err)
		}
		if ok {
			continue
		}
		if exists {
			logger.Warnf("Restoring %s %s", r.chain, strings.Join(r.rule, " "))
		}
		if r.first {
			err = ipt.Insert(r.table, r.chain, 1, r.rule...)
		} else {
			err = ipt.Append(r.table, r.chain, r.rule...)
		}
		if err != nil {
			return fmt.Errorf("failed to add %s %s: %w", r.chain, strings.Join(r.rule, " "), err)
		}
	}
	return syncVethRules(ipt, veths, exact)
}

// syncVethRules adds the forwarding rules of veths to TSLINK-FORWARD and,
// with exact, removes those of other interfaces.
func syncVethRules(ipt *iptables.IPTables, veths []string, exact bool) error {
	listed, err := ipt.List("filter", chainName)
	if err != nil {
		return fmt.Errorf("failed to list chain %s: %w", chainName, err)
	}
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
			if err := ipt.Delete("filter", chainName, strings.Fields(rule)...); err != nil {
				errs = append(errs, fmt.Errorf("failed to remove %s %s: %w", chainName, rule, err))
			}
		}
	}
	for _, line := range order {
		if rule, ok := want[line]; ok {
			if err := ipt.Append("filter", chainName, rule...); err != nil {
				errs = append(errs, fmt.Errorf("failed to add %s: %w", line, err))
			}
		}
	}
	return errors.Join(errs...)
}

// syncForwarding makes TSLINK-FORWARD hold the forwarding rules of the veths
// in tslink's veth range, and restores the jump to it and MASQUERADE.
func syncForwarding(ipt *iptables.IPTables) error {
	forwardMu.Lock()
	defer forwardMu.Unlock()
	veths, err := tslinkVeths()
	if err != nil {
		return err
	}
	return ensureForwarding(ipt, veths, true)
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

// CleanupNAT removes the FORWARD rules and the port mapping for a specific
// veth interface, and the port mappings of veths that are gone.
// The global MASQUERADE rule and chain structure are intentionally left in place.
func CleanupNAT(vethHost string) error {
	logger.Debugf("Cleaning up NAT rules for %s", vethHost)

	ipt, err := iptables.New()
	if err != nil {
		logger.Debugf(
			"Failed to initialize iptables, NAT rules for %s not cleaned: %v",
			vethHost,
			err,
		)
		return nil
	}

	// Remove FORWARD rules from our custom chain (ignore errors if rules don't exist)
	forwardMu.Lock()
	if err := ipt.DeleteIfExists("filter", chainName, "-i", vethHost, "-j", "ACCEPT"); err != nil {
		logger.Debugf("FORWARD -i rule not found for %s (already cleaned): %v", vethHost, err)
	}

	if err := ipt.DeleteIfExists("filter", chainName, "-o", vethHost, "-j", "ACCEPT"); err != nil {
		logger.Debugf("FORWARD -o rule not found for %s (already cleaned): %v", vethHost, err)
	}
	forwardMu.Unlock()

	if err := removePortMapping(ipt, vethHost); err != nil {
		logger.Warnf("Failed to remove the WireGuard port mapping behind %s: %v", vethHost, err)
	}

	logger.Debugf("NAT cleanup complete for %s", vethHost)
	return nil
}
