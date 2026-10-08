//go:build linux

package netutil

import (
	"net"
	"testing"

	"github.com/vishvananda/netlink"
)

// Guards: G1
func TestBypassRoute(t *testing.T) {
	// The test netns's main default route goes via dummy0, like an overlay
	// task's goes via docker_gwbridge; tailscaled's own traffic must use
	// the tslink veth instead, here dummy1.
	nsPath := newTestNetNS(t)
	if err := inNetNS(nsPath, func() error {
		veth := &netlink.Dummy{Name: "dummy1"}
		if err := netlink.LinkAdd(veth); err != nil {
			return err
		}
		addr, _ := netlink.ParseAddr("10.200.0.2/30")
		if err := netlink.AddrAdd(veth, addr); err != nil {
			return err
		}
		return netlink.LinkSetUp(veth)
	}); err != nil {
		t.Fatalf("set up veth stand-in: %v", err)
	}

	for range 2 { // Join may be retried; setup must be idempotent
		if err := SetupBypassRoute(nsPath, "dummy1", "10.200.0.1"); err != nil {
			t.Fatalf("SetupBypassRoute: %v", err)
		}
	}

	lookup := func(mark uint32) netlink.Route {
		t.Helper()
		var routes []netlink.Route
		if err := inNetNS(nsPath, func() error {
			var err error
			routes, err = netlink.RouteGetWithOptions(
				net.ParseIP("1.1.1.1"),
				&netlink.RouteGetOptions{Mark: mark},
			)
			return err
		}); err != nil || len(routes) == 0 {
			t.Fatalf("route get (mark %#x): %v", mark, err)
		}
		return routes[0]
	}
	if r := lookup(tailscaledBypassMark); !r.Gw.Equal(net.ParseIP("10.200.0.1")) {
		t.Errorf("tailscaled traffic routes via %v, want 10.200.0.1", r.Gw)
	}
	if r := lookup(0); r.Gw != nil {
		t.Errorf("other traffic routes via %v, want the main default route", r.Gw)
	}
}
