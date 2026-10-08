//go:build linux

package netutil

import (
	"net"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
)

// A listener opened in another namespace accepts connections there, on its
// loopback, and not in the namespace that opened it.
func TestListenInNetNS(t *testing.T) {
	nsPath := newTestNetNS(t)
	if err := inNetNS(nsPath, func() error {
		lo, err := netlink.LinkByName("lo")
		if err != nil {
			return err
		}
		return netlink.LinkSetUp(lo)
	}); err != nil {
		t.Fatalf("bring lo up: %v", err)
	}
	ln, err := ListenInNetNS(nsPath, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenInNetNS: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	addr := ln.Addr().String()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	if err := inNetNS(nsPath, func() error {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			_ = c.Close()
		}
		return err
	}); err != nil {
		t.Errorf("dial %s inside the namespace: %v", addr, err)
	}
	// Control: the test's own namespace has nothing on that port
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		_ = c.Close()
		t.Errorf("dial %s outside the namespace succeeded", addr)
	}
}
