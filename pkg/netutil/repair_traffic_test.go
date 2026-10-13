//go:build linux

package netutil

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"
)

// addHostTailnet gives the host namespace the host tailscaled's interface and
// tailnet address, 100.64.0.1.
func addHostTailnet(t *testing.T, hostNS string) {
	t.Helper()
	if err := inNetNS(hostNS, func() error {
		ts := &netlink.Dummy{Name: "tailscale0"}
		if err := netlink.LinkAdd(ts); err != nil {
			return err
		}
		addr, err := netlink.ParseAddr("100.64.0.1/32")
		if err != nil {
			return err
		}
		if err := netlink.AddrAdd(ts, addr); err != nil {
			return err
		}
		return netlink.LinkSetUp(ts)
	}); err != nil {
		t.Fatal(err)
	}
}

// countUDP counts the datagrams that reach addr in the namespace ns until the
// test ends.
func countUDP(t *testing.T, ns, addr string) *atomic.Int64 {
	t.Helper()
	var conn net.PacketConn
	if err := inNetNS(ns, func() error {
		var err error
		conn, err = net.ListenPacket("udp4", addr)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var n atomic.Int64
	var wg sync.WaitGroup
	wg.Go(func() {
		buf := make([]byte, 64)
		for {
			if _, _, err := conn.ReadFrom(buf); err != nil {
				return
			}
			n.Add(1)
		}
	})
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
		wg.Wait()
	})
	return &n
}

// floodUDP sends datagrams from the namespace ns to each of dsts, as fast as
// it can, until the test ends.
func floodUDP(t *testing.T, ns string, dsts ...string) {
	t.Helper()
	var conn net.PacketConn
	if err := inNetNS(ns, func() error {
		var err error
		conn, err = net.ListenPacket("udp4", ":0")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	addrs := make([]*net.UDPAddr, 0, len(dsts))
	for _, d := range dsts {
		a, err := net.ResolveUDPAddr("udp4", d)
		if err != nil {
			t.Fatal(err)
		}
		addrs = append(addrs, a)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, a := range addrs {
				if _, err := conn.WriteTo([]byte("x"), a); err != nil &&
					errors.Is(err, net.ErrClosed) {
					return
				}
			}
		}
	})
	t.Cleanup(func() {
		close(stop)
		wg.Wait()
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	})
}

// waitCount waits until n is positive.
func waitCount(t *testing.T, what string, n *atomic.Int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for n.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("control: nothing reached %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Restoring an isolation chain, or moving the jump to it back to the top, lets
// nothing through that the chain drops, at any moment: the watchdog does both
// after a firewall reload or another program's rules, while containers send.
// Guards: G1, G2
func TestIsolationRepairHasNoWindow(t *testing.T) {
	hostNS, bridgedNS, tslinkNS := vethTopology(t, "w", "custombr0")
	addHostTailnet(t, hostNS)
	bridges := []string{"custombr0"}
	toVeth := countUDP(t, tslinkNS, "10.200.0.2:9999")
	toHost := countUDP(t, hostNS, "100.64.0.1:9999")
	floodUDP(t, bridgedNS, "10.200.0.2:9999", "100.64.0.1:9999")

	// Control: without the isolation, both get through
	waitCount(t, "the veth address", toVeth)
	waitCount(t, "the host's tailnet address", toHost)

	setup := func() error {
		if err := SetupVethIsolation(WireGuardTestPort); err != nil {
			return err
		}
		return SetupHostIsolation(bridges)
	}
	if err := inNetNS(hostNS, setup); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // for datagrams on their way
	toVeth.Store(0)
	toHost.Store(0)

	junk := []string{"-d", "192.0.2.1/32", "-j", returnTarget}
	for range 20 {
		if err := inNetNS(hostNS, func() error {
			ipt, err := iptables.New()
			if err != nil {
				return err
			}
			// A rule the chains do not hold makes them be rewritten; one
			// ahead of the jumps makes them be moved
			for _, chain := range tslinkChains {
				if err := ipt.Append("mangle", chain, junk...); err != nil {
					return err
				}
			}
			for _, from := range []string{"FORWARD", "INPUT"} {
				if err := ipt.Insert("mangle", from, 1, junk...); err != nil {
					return err
				}
			}
			return setup()
		}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if n := toVeth.Load(); n > 0 {
		t.Errorf("%d datagrams reached the veth address during repairs", n)
	}
	if n := toHost.Load(); n > 0 {
		t.Errorf("%d datagrams reached the host's tailnet address during repairs", n)
	}
}
