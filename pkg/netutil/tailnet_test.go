//go:build linux

package netutil

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// newTestNetNS creates a named network namespace with a default route via a
// dummy interface, so tailnet traffic would leak without the blackhole.
func newTestNetNS(t *testing.T) string {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires root (CAP_NET_ADMIN)")
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	origNS, err := netns.Get()
	if err != nil {
		t.Fatalf("get current netns: %v", err)
	}
	defer origNS.Close()

	name := fmt.Sprintf("tslink-test-%d", os.Getpid())
	ns, err := netns.NewNamed(name)
	if err != nil {
		t.Fatalf("create netns: %v", err)
	}
	defer ns.Close()
	t.Cleanup(func() {
		if err := netns.DeleteNamed(name); err != nil {
			t.Errorf("delete netns: %v", err)
		}
	})

	dummy := &netlink.Dummy{Name: "dummy0"}
	setup := func() error {
		if err := netlink.LinkAdd(dummy); err != nil {
			return err
		}
		if err := netlink.LinkSetUp(dummy); err != nil {
			return err
		}
		_, any4, _ := net.ParseCIDR("0.0.0.0/0")
		return netlink.RouteAdd(&netlink.Route{LinkIndex: dummy.Index, Dst: any4})
	}
	err = setup()
	if restoreErr := netns.Set(origNS); restoreErr != nil {
		t.Fatalf("restore netns: %v", restoreErr)
	}
	if err != nil {
		t.Fatalf("set up test netns: %v", err)
	}

	return filepath.Join("/var/run/netns", name)
}

// routeGet resolves dst inside the namespace at nsPath.
func routeGet(t *testing.T, nsPath string, dst string) error {
	t.Helper()
	var getErr error
	if err := inNetNS(nsPath, func() error {
		_, getErr = netlink.RouteGet(net.ParseIP(dst))
		return nil
	}); err != nil {
		t.Fatalf("enter netns: %v", err)
	}
	return getErr
}

func TestTailnetBlackhole(t *testing.T) {
	nsPath := newTestNetNS(t)

	if err := routeGet(t, nsPath, "100.100.1.1"); err != nil {
		t.Fatalf("precondition: expected tailnet address to route via default, got %v", err)
	}

	if err := SetupTailnetBlackhole(nsPath); err != nil {
		t.Fatalf("SetupTailnetBlackhole: %v", err)
	}
	// Join may be retried; setup must be idempotent.
	if err := SetupTailnetBlackhole(nsPath); err != nil {
		t.Fatalf("SetupTailnetBlackhole (second call): %v", err)
	}

	for _, dst := range []string{"100.64.0.1", "100.100.1.1", "100.127.255.254"} {
		if err := routeGet(t, nsPath, dst); !errors.Is(err, syscall.EHOSTUNREACH) {
			t.Errorf("route to %s: got %v, want EHOSTUNREACH", dst, err)
		}
	}
	// The test netns has IPv6 enabled but no IPv6 route, so check the table directly.
	var v6 []netlink.Route
	if err := inNetNS(nsPath, func() error {
		var err error
		v6, err = netlink.RouteListFiltered(netlink.FAMILY_V6,
			&netlink.Route{Dst: tailnetV6, Type: rtnUnreachable},
			netlink.RT_FILTER_DST|netlink.RT_FILTER_TYPE)
		return err
	}); err != nil {
		t.Fatalf("list IPv6 routes: %v", err)
	}
	if len(v6) != 1 {
		t.Errorf("want 1 unreachable route for %s, got %d", tailnetV6, len(v6))
	}
	if err := routeGet(t, nsPath, "8.8.8.8"); err != nil {
		t.Errorf("route to 8.8.8.8 should be unaffected, got %v", err)
	}

	if err := CleanupTailnetBlackhole(nsPath); err != nil {
		t.Fatalf("CleanupTailnetBlackhole: %v", err)
	}
	if err := routeGet(t, nsPath, "100.100.1.1"); err != nil {
		t.Errorf("after cleanup, expected default route again, got %v", err)
	}
	// Leave may run after routes are gone.
	if err := CleanupTailnetBlackhole(nsPath); err != nil {
		t.Errorf("CleanupTailnetBlackhole (second call): %v", err)
	}
}

func TestCleanupTailnetBlackholeMissingNetNS(t *testing.T) {
	if err := CleanupTailnetBlackhole("/var/run/netns/tslink-does-not-exist"); err != nil {
		t.Errorf("expected nil for missing netns, got %v", err)
	}
}
