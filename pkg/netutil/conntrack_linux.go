//go:build linux

package netutil

import (
	"fmt"
	"net/netip"
	"syscall"

	"github.com/vishvananda/netlink"
)

// forgetUDPFlows deletes the conntrack entries of UDP flows from each of
// srcs on port sport, in one pass through the conntrack table. The kernel
// keeps a flow's NAT for as long as its entry lives, so a new mapping applies
// to the flow's next packet only once its entry is gone.
func forgetUDPFlows(srcs []netip.Addr, sport int) (uint, error) {
	filters := make([]netlink.CustomConntrackFilter, 0, len(srcs))
	for _, src := range srcs {
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
		filters = append(filters, f)
	}
	n, err := netlink.ConntrackDeleteFilters(netlink.ConntrackTable, netlink.FAMILY_V4, filters...)
	if err != nil {
		return n, fmt.Errorf(
			"failed to delete the conntrack entries of %v:%d: %w",
			srcs,
			sport,
			err,
		)
	}
	return n, nil
}
