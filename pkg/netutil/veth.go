// Package netutil sets up the Linux networking for endpoints: veth pairs,
// routing and NAT.
package netutil

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"

	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/aaomidi/tslink/pkg/logger"
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
		LinkAttrs: netlink.LinkAttrs{
			Name: hostName,
			MTU:  mtu,
		},
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
		// Ignore if already exists
		logger.Debugf("Warning: failed to add IP to host veth (may already exist): %v", err)
	}

	return nil
}

// SetupContainerRouting sets up routing inside the container namespace.
func SetupContainerRouting(nsPath string, ifName string, containerIP string, gatewayIP string) error {
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

// chainInitialized tracks if we've already set up the custom chain.
var (
	chainMu          sync.Mutex
	chainInitialized bool
)

// SetupNAT sets up MASQUERADE for traffic from the container.
// Uses a custom chain (TS-CNI-FORWARD) for organized rule management.
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

	// Initialize our custom chain and global rules (only once, with mutex protection)
	chainMu.Lock()
	if !chainInitialized {
		if err := initializeChain(ipt); err != nil {
			chainMu.Unlock()
			return err
		}
		chainInitialized = true
	}
	chainMu.Unlock()

	// Allow forwarding for this specific veth (in our custom chain)
	if err := ipt.AppendUnique("filter", chainName, "-i", vethHost, "-j", "ACCEPT"); err != nil {
		logger.Warnf("Failed to add FORWARD rule for %s: %v", vethHost, err)
	}

	if err := ipt.AppendUnique("filter", chainName, "-o", vethHost, "-j", "ACCEPT"); err != nil {
		logger.Warnf("Failed to add FORWARD rule for %s: %v", vethHost, err)
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

// initializeChain creates the custom chain and sets up global rules.
func initializeChain(ipt *iptables.IPTables) error {
	// Create our custom chain unless it exists
	if exists, err := ipt.ChainExists("filter", chainName); err != nil || !exists {
		if err := ipt.NewChain("filter", chainName); err != nil {
			logger.Debugf("iptables chain %s may already exist: %v", chainName, err)
		}
	}

	// Add jump rule from FORWARD to our chain at the beginning, unless it exists
	if err := ipt.InsertUnique("filter", "FORWARD", 1, "-j", chainName); err != nil {
		return fmt.Errorf("failed to add jump to %s: %w", chainName, err)
	}

	// Add global MASQUERADE rule for the 10.200.0.0/16 range, unless it exists
	if err := ipt.AppendUnique("nat", "POSTROUTING", "-s", natSource, "-j", "MASQUERADE"); err != nil {
		return fmt.Errorf("failed to add MASQUERADE rule: %w", err)
	}

	logger.Infof("Initialized iptables chain %s", chainName)
	return nil
}

// CleanupNAT removes the FORWARD rules for a specific veth interface.
// The global MASQUERADE rule and chain structure are intentionally left in place.
func CleanupNAT(vethHost string) error {
	logger.Debugf("Cleaning up NAT rules for %s", vethHost)

	ipt, err := iptables.New()
	if err != nil {
		logger.Debugf("Failed to initialize iptables, NAT rules for %s not cleaned: %v", vethHost, err)
		return nil
	}

	// Remove FORWARD rules from our custom chain (ignore errors if rules don't exist)
	if err := ipt.DeleteIfExists("filter", chainName, "-i", vethHost, "-j", "ACCEPT"); err != nil {
		logger.Debugf("FORWARD -i rule not found for %s (already cleaned): %v", vethHost, err)
	}

	if err := ipt.DeleteIfExists("filter", chainName, "-o", vethHost, "-j", "ACCEPT"); err != nil {
		logger.Debugf("FORWARD -o rule not found for %s (already cleaned): %v", vethHost, err)
	}

	logger.Debugf("NAT cleanup complete for %s", vethHost)
	return nil
}

// CleanupAllNAT removes the entire custom chain and all its rules.
// This is useful for complete plugin cleanup.
func CleanupAllNAT() error {
	logger.Infof("Cleaning up all NAT rules")

	if ipt, err := iptables.New(); err != nil {
		logger.Debugf("Failed to initialize iptables, NAT rules not cleaned: %v", err)
	} else {
		// Remove jump rule from FORWARD
		if err := ipt.DeleteIfExists("filter", "FORWARD", "-j", chainName); err != nil {
			logger.Debugf("No jump rule to %s found (may already be cleaned): %v", chainName, err)
		}

		// Flush and delete our custom chain
		if err := ipt.ClearAndDeleteChain("filter", chainName); err != nil {
			logger.Debugf("Failed to delete chain %s (may not exist): %v", chainName, err)
		}

		// Remove MASQUERADE rule
		if err := ipt.DeleteIfExists("nat", "POSTROUTING", "-s", natSource, "-j", "MASQUERADE"); err != nil {
			logger.Debugf("No MASQUERADE rule found (may already be cleaned): %v", err)
		}
	}

	chainMu.Lock()
	chainInitialized = false
	chainMu.Unlock()

	logger.Infof("All NAT rules cleaned up")
	return nil
}
