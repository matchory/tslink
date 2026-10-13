//go:build linux

package netutil

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
)

// Two endpoints whose IDs share their first 8 characters get the same veth
// name: the second must not take the first one's veth.
func TestCreateVethPairRefusesLiveVeth(t *testing.T) {
	nsPath, h := newNamedNetNS(t, fmt.Sprintf("tsl-%d-cv", os.Getpid()))
	live := &netlink.Veth{Name: "vethtsll", PeerName: "vethtsllc"}
	leftover := &netlink.Veth{Name: "vethtslo", PeerName: "vethtsloc"}
	for _, v := range []*netlink.Veth{live, leftover} {
		if err := h.LinkAdd(v); err != nil {
			t.Fatal(err)
		}
	}
	addr, _ := netlink.ParseAddr("10.200.0.1/30")
	if err := h.AddrAdd(live, addr); err != nil {
		t.Fatal(err)
	}
	liveIndex := linkIndex(t, h, "vethtsll")
	leftoverIndex := linkIndex(t, h, "vethtslo")

	var liveErr, leftoverErr error
	if err := inNetNS(nsPath, func() error {
		_, _, liveErr = CreateVethPair("tsll", 0)
		_, _, leftoverErr = CreateVethPair("tslo", 0)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if liveErr == nil || !strings.Contains(liveErr.Error(), "vethtsll") {
		t.Errorf("CreateVethPair over a live veth: err = %v, want the collision", liveErr)
	}
	if got := linkIndex(t, h, "vethtsll"); got != liveIndex {
		t.Errorf("live veth replaced (index %d, was %d)", got, liveIndex)
	}
	if leftoverErr != nil {
		t.Errorf("CreateVethPair over a leftover veth: %v", leftoverErr)
	}
	if got := linkIndex(t, h, "vethtslo"); got == leftoverIndex || got == 0 {
		t.Errorf("leftover veth not replaced (index %d, was %d)", got, leftoverIndex)
	}
}

// A veth pair that cannot be brought up does not stay behind.
func TestCreateVethPairFailureLeavesNoVeth(t *testing.T) {
	nsPath, h := newNamedNetNS(t, fmt.Sprintf("tsl-%d-cf", os.Getpid()))
	orig := linkSetUp
	t.Cleanup(func() { linkSetUp = orig })
	linkSetUp = func(netlink.Link) error { return errors.New("injected") }

	var createErr error
	if err := inNetNS(nsPath, func() error {
		_, _, createErr = CreateVethPair("tslf", 0)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if createErr == nil {
		t.Fatal("CreateVethPair succeeded despite the failure")
	}
	for _, name := range []string{"vethtslf", "vethtslfc"} {
		if linkIndex(t, h, name) != 0 {
			t.Errorf("%s left behind", name)
		}
	}
}
