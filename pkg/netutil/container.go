package netutil

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"

	"github.com/matchory/tslink/pkg/logger"
)

// ContainerSide is the container's end of a tslink veth.
type ContainerSide struct {
	NSPath    string     // the container's network namespace
	Interface string     // the container end of the veth, already in NSPath
	Address   netip.Addr // its address in the veth's /30
	Gateway   netip.Addr // the host end's address
	// IPAMAddress is the address Docker's IPAM assigned to the endpoint, if any
	IPAMAddress netip.Addr
}

// SetupContainerSide configures the container's end of the veth, through one
// netlink handle in its namespace: it brings the interface up, adds its /30
// address, routes tailscaled's own traffic through it (setupBypassRoute),
// makes the tailnet ranges unreachable outside tailscaled
// (setupTailnetBlackhole) and adds the IPAM address (addIPAMAddress). It is
// idempotent, so recovery can run it again. A failure to add the IPAM address
// is only logged.
//
// It sets no default route: Docker connects the container to its gateway
// network like any other.
func SetupContainerSide(c ContainerSide) error {
	addr := netlinkAddr(c.Address, 30)

	h, err := handleAt(c.NSPath)
	if err != nil {
		return err
	}
	defer h.Close()

	link, err := h.LinkByName(c.Interface)
	if err != nil {
		return fmt.Errorf("interface %s not found in %s: %w", c.Interface, c.NSPath, err)
	}
	if err := h.LinkSetUp(link); err != nil {
		return fmt.Errorf("failed to bring up %s: %w", c.Interface, err)
	}
	if err := h.AddrAdd(link, addr); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("failed to add %s to %s: %w", addr.IPNet, c.Interface, err)
	}
	if err := setupBypassRoute(h, link, net.IP(c.Gateway.AsSlice())); err != nil {
		return err
	}
	if c.IPAMAddress.IsValid() {
		if err := addIPAMAddress(h, link, c.IPAMAddress); err != nil {
			logger.Warnf("Failed to add IPAM address %s to the container: %v", c.IPAMAddress, err)
		}
	}
	if err := setupTailnetBlackhole(h); err != nil {
		return err
	}
	logger.Debugf("Container side set up: %s %s, peer %s", c.Interface, c.Address, c.Gateway)
	return nil
}
