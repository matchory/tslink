//go:build linux

package netutil

import (
	"fmt"
	"net/netip"
	"syscall"

	"github.com/vishvananda/netlink"
)

// forgetUDPFlows deletes the conntrack entries of UDP flows from src:sport.
// The kernel keeps a flow's NAT for as long as its entry lives, so a new
// mapping applies to the flow's next packet only once its entry is gone.
func forgetUDPFlows(src netip.Addr, sport int) (uint, error) {
	f := &netlink.ConntrackFilter{}
	if err := f.AddProtocol(syscall.IPPROTO_UDP); err != nil {
		return 0, err
	}
	if err := f.AddIP(netlink.ConntrackOrigSrcIP, src.AsSlice()); err != nil {
		return 0, err
	}
	//nolint:gosec // sport is tailscaled's port
	if err := f.AddPort(netlink.ConntrackOrigSrcPort, uint16(sport)); err != nil {
		return 0, err
	}
	n, err := netlink.ConntrackDeleteFilters(netlink.ConntrackTable, netlink.FAMILY_V4, f)
	if err != nil {
		return n, fmt.Errorf("failed to delete the conntrack entries of %s:%d: %w", src, sport, err)
	}
	return n, nil
}
