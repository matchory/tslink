//go:build linux

package netutil

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// joinNamespaces creates a host and a container namespace, and in the host's
// the veth pair vethtslj/vethtsljc, with 10.200.0.5/30 on the host end.
func joinNamespaces(t *testing.T) (string, string, *netlink.Handle) {
	t.Helper()
	pid := os.Getpid()
	hostNS, _ := newNamedNetNS(t, fmt.Sprintf("tsl-%d-cs-h", pid))
	ctrNS, ctr := newNamedNetNS(t, fmt.Sprintf("tsl-%d-cs-c", pid))
	if err := inNetNS(hostNS, func() error {
		if _, _, err := CreateVethPair("tslj", 0); err != nil {
			return err
		}
		return SetupHostRouting("vethtslj", netip.MustParseAddr("10.200.0.5"))
	}); err != nil {
		t.Fatalf("set up host side: %v", err)
	}
	return hostNS, ctrNS, ctr
}

// countSetNS counts the namespace switches until the test ends.
func countSetNS(t *testing.T) *int {
	t.Helper()
	orig := setNS
	t.Cleanup(func() { setNS = orig })
	n := new(int)
	setNS = func(ns netns.NsHandle) error {
		*n++
		return orig(ns)
	}
	return n
}

// Join's setup of the container side gives the container the veth address,
// tailscaled's route through the veth, the tailnet blackhole and the IPAM
// address, entering the container's namespace only to open one netlink
// handle.
//
// Guards: G1
func TestSetupContainerSide(t *testing.T) {
	hostNS, ctrNS, ctr := joinNamespaces(t)
	switches := countSetNS(t)
	if err := inNetNS(hostNS, func() error { return MoveToNetNS("vethtsljc", ctrNS) }); err != nil {
		t.Fatalf("MoveToNetNS: %v", err)
	}
	if *switches != 2 { // The test's inNetNS(hostNS), entry and return
		t.Errorf("MoveToNetNS switched namespaces %d times, want 0", *switches-2)
	}
	*switches = 0

	side := ContainerSide{
		NSPath:      ctrNS,
		Interface:   "vethtsljc",
		Address:     netip.MustParseAddr("10.200.0.6"),
		Gateway:     netip.MustParseAddr("10.200.0.5"),
		IPAMAddress: netip.MustParseAddr("10.42.6.51"),
	}
	for i := range 2 { // Recovery runs it again
		if err := SetupContainerSide(side); err != nil {
			t.Fatalf("SetupContainerSide (call %d): %v", i+1, err)
		}
	}
	if *switches != 4 {
		t.Errorf("%d namespace switches, want 4: one entry and one return per call", *switches)
	}

	link, err := ctr.LinkByName("vethtsljc")
	if err != nil {
		t.Fatal(err)
	}
	if link.Attrs().Flags&net.FlagUp == 0 {
		t.Error("vethtsljc is down")
	}
	addrs, err := ctr.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range addrs {
		got = append(got, a.IPNet.String())
	}
	slices.Sort(got)
	if want := []string{"10.200.0.6/30", "10.42.6.51/32"}; !slices.Equal(got, want) {
		t.Errorf("addresses %v, want %v", got, want)
	}

	routes, err := ctr.RouteListFiltered(netlink.FAMILY_V4,
		&netlink.Route{Table: bypassTable}, netlink.RT_FILTER_TABLE)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].Dst != nil && routes[0].Dst.String() != "0.0.0.0/0" ||
		!routes[0].Gw.Equal(net.ParseIP("10.200.0.5")) ||
		routes[0].LinkIndex != link.Attrs().Index {
		t.Errorf("table %d: %v, want one default route via 10.200.0.5 dev vethtsljc",
			bypassTable, routes)
	}

	rules, err := ctr.RuleList(netlink.FAMILY_V4)
	if err != nil {
		t.Fatal(err)
	}
	var bypass []netlink.Rule
	for _, r := range rules {
		if r.Priority == bypassPriority {
			bypass = append(bypass, r)
		}
	}
	if len(bypass) != 1 || bypass[0].Table != bypassTable ||
		bypass[0].Mark != tailscaledBypassMark || bypass[0].Mask == nil ||
		*bypass[0].Mask != tailscaledMarkMask {
		t.Errorf("rules at priority %d: %+v, want one for mark %#x/%#x to table %d",
			bypassPriority, bypass, tailscaledBypassMark, tailscaledMarkMask, bypassTable)
	}

	for _, dst := range []*net.IPNet{tailnetV4, tailnetV6} {
		family := netlink.FAMILY_V4
		if dst.IP.To4() == nil {
			family = netlink.FAMILY_V6
		}
		found, err := ctr.RouteListFiltered(family,
			&netlink.Route{Dst: dst, Type: rtnUnreachable},
			netlink.RT_FILTER_DST|netlink.RT_FILTER_TYPE)
		if err != nil {
			t.Fatal(err)
		}
		if len(found) != 1 {
			t.Errorf("want 1 unreachable route for %s, got %d", dst, len(found))
		}
	}
}

// An address the kernel refuses on the container's veth fails the setup.
func TestSetupContainerSideReturnsAddressError(t *testing.T) {
	nsPath := newTestNetNS(t)
	// Lowered below IPv4's minimum MTU, the link loses its IPv4
	// configuration, and the kernel refuses IPv4 addresses on it
	if err := inNetNS(nsPath, func() error {
		dummy := &netlink.Dummy{Name: "dummy1"}
		if err := netlink.LinkAdd(dummy); err != nil {
			return err
		}
		return netlink.LinkSetMTU(dummy, 60)
	}); err != nil {
		t.Fatalf("add dummy1: %v", err)
	}
	err := SetupContainerSide(ContainerSide{
		NSPath:    nsPath,
		Interface: "dummy1",
		Address:   netip.MustParseAddr("10.200.0.6"),
		Gateway:   netip.MustParseAddr("10.200.0.5"),
	})
	if err == nil || !strings.Contains(err.Error(), "10.200.0.6/30") {
		t.Errorf("err = %v, want the failed address", err)
	}
}

// linkIndex returns the index of the link name in h, or 0 if there is none.
func linkIndex(t *testing.T, h *netlink.Handle, name string) int {
	t.Helper()
	link, err := h.LinkByName(name)
	if _, ok := errors.AsType[netlink.LinkNotFoundError](err); ok {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return link.Attrs().Index
}

// Docker's namespace path may be a link with a relative target, which is
// relative to the link's directory, not the plugin's working directory.
func TestMoveToNetNSRelativeLink(t *testing.T) {
	pid := os.Getpid()
	hostNS, h := newNamedNetNS(t, fmt.Sprintf("tsl-%d-mv-h", pid))
	ctrNS, ctr := newNamedNetNS(t, fmt.Sprintf("tsl-%d-mv-c", pid))
	if err := h.LinkAdd(&netlink.Veth{
		Name:     "vethtslm",
		PeerName: "vethtslmc",
	}); err != nil {
		t.Fatal(err)
	}

	// A link next to the namespace, to its name
	target := filepath.Base(ctrNS)
	link := ctrNS + "-link"
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(link); err != nil {
			t.Error(err)
		}
	})

	if err := inNetNS(hostNS, func() error { return MoveToNetNS("vethtslmc", link) }); err != nil {
		t.Fatalf("MoveToNetNS via %s -> %s: %v", link, target, err)
	}
	if linkIndex(t, ctr, "vethtslmc") == 0 {
		t.Error("vethtslmc is not in the container's namespace")
	}
}
