package netutil

import (
	"errors"
	"fmt"
	"net/netip"
	"syscall"

	"github.com/vishvananda/netlink"
)

// addIPAMAddress puts the address Docker's IPAM assigned to an endpoint on
// link, as a /32. tslink connects the container through its own veth subnet,
// but Docker lists the IPAM address in the container's /etc/hosts: a process
// that resolves its own name to it, as RabbitMQ's epmd does, must be able to
// reach it. A /32 adds no route, so the network's other addresses stay
// unreachable here.
func addIPAMAddress(h *netlink.Handle, link netlink.Link, ip netip.Addr) error {
	if !ip.Is4() {
		return fmt.Errorf("%s is not an IPv4 address", ip)
	}
	addr := netlinkAddr(ip, 32)
	// Join may be retried: the address may already be there
	if err := h.AddrAdd(link, addr); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("failed to add %s to %s: %w", addr.IPNet, link.Attrs().Name, err)
	}
	return nil
}
