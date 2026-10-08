//go:build linux

package netutil

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// newNamedNetNS creates the network namespace name and returns its path and
// a netlink handle in it.
func newNamedNetNS(t *testing.T, name string) (string, *netlink.Handle) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires root (CAP_NET_ADMIN)")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	orig, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer orig.Close()
	ns, err := netns.NewNamed(name)
	if restoreErr := netns.Set(orig); restoreErr != nil {
		t.Fatalf("restore netns: %v", restoreErr)
	}
	if err != nil {
		t.Fatalf("create netns %s: %v", name, err)
	}
	t.Cleanup(func() {
		if err := ns.Close(); err != nil {
			t.Error(err)
		}
		if err := netns.DeleteNamed(name); err != nil {
			t.Errorf("delete netns %s: %v", name, err)
		}
	})
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return filepath.Join("/var/run/netns", name), h
}

// linkUp adds addr to the link name in h, brings it up and, with a gateway,
// routes everything through it.
func linkUp(t *testing.T, h *netlink.Handle, name, addr, gateway string) {
	t.Helper()
	link, err := h.LinkByName(name)
	if err != nil {
		t.Fatal(err)
	}
	if addr != "" {
		a, err := netlink.ParseAddr(addr)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.AddrAdd(link, a); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.LinkSetUp(link); err != nil {
		t.Fatal(err)
	}
	if gateway != "" {
		_, all, _ := net.ParseCIDR("0.0.0.0/0")
		if err := h.RouteAdd(&netlink.Route{
			LinkIndex: link.Attrs().Index, Dst: all, Gw: net.ParseIP(gateway),
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// vethTopology builds a host namespace with a tslink container behind a veth
// in vethRange and another container behind a bridge named bridge, as Docker
// connects containers, and returns the host's, the bridged container's and
// the tslink container's namespaces. The host forwards, with Docker's FORWARD
// policy DROP, and tslink's forwarding rules for the veth.
func vethTopology(t *testing.T, id, bridge string) (string, string, string) {
	t.Helper()
	pid := os.Getpid()
	hostNS, host := newNamedNetNS(t, fmt.Sprintf("tsl-%d-%s-h", pid, id))
	bridgedNS, bridged := newNamedNetNS(t, fmt.Sprintf("tsl-%d-%s-a", pid, id))
	tslinkNS, tslink := newNamedNetNS(t, fmt.Sprintf("tsl-%d-%s-b", pid, id))

	// The bridged container: a bridge port on the host, eth0 inside
	br := &netlink.Bridge{Name: bridge}
	if err := host.LinkAdd(br); err != nil {
		t.Fatal(err)
	}
	linkUp(t, host, bridge, "172.30.0.1/24", "")
	brLink, err := host.LinkByName(bridge)
	if err != nil {
		t.Fatal(err)
	}
	bridgedFd, err := netns.GetFromPath(bridgedNS)
	if err != nil {
		t.Fatal(err)
	}
	defer bridgedFd.Close()
	if err := host.LinkAdd(&netlink.Veth{
		Name: "port0", MasterIndex: brLink.Attrs().Index,
		PeerName:      "eth0",
		PeerNamespace: netlink.NsFd(bridgedFd),
	}); err != nil {
		t.Fatal(err)
	}
	linkUp(t, host, "port0", "", "")
	linkUp(t, bridged, "eth0", "172.30.0.2/24", "172.30.0.1")

	// The tslink container's veth
	tslinkFd, err := netns.GetFromPath(tslinkNS)
	if err != nil {
		t.Fatal(err)
	}
	defer tslinkFd.Close()
	if err := host.LinkAdd(&netlink.Veth{
		Name:          "vethtest0",
		PeerName:      "eth0",
		PeerNamespace: netlink.NsFd(tslinkFd),
	}); err != nil {
		t.Fatal(err)
	}
	linkUp(t, host, "vethtest0", "10.200.0.1/30", "")
	linkUp(t, tslink, "eth0", "10.200.0.2/30", "10.200.0.1")

	if err := inNetNS(hostNS, func() error {
		if err := enableIPForward(); err != nil {
			return err
		}
		ipt, err := iptables.New()
		if err != nil {
			return err
		}
		if err := ipt.ChangePolicy("filter", "FORWARD", "DROP"); err != nil {
			return err
		}
		resetChainInitialized()
		return SetupNAT("vethtest0")
	}); err != nil {
		t.Fatal(err)
	}
	return hostNS, bridgedNS, tslinkNS
}

// reaches reports whether a TCP connection from the namespace from to addr
// succeeds.
func reaches(t *testing.T, from, addr string) bool {
	t.Helper()
	var ok bool
	if err := inNetNS(from, func() error {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err != nil {
			return nil //nolint:nilerr // Unreachable is the answer, not a failure
		}
		ok = true
		return conn.Close()
	}); err != nil {
		t.Fatal(err)
	}
	return ok
}

// A container on any bridge reaches no port of a tslink container's veth
// address, whatever the bridge is named: a stack file names one with the
// driver option com.docker.network.bridge.name.
// Guards: G2
func TestVethIsolationBlocksEveryBridge(t *testing.T) {
	for i, tt := range []struct {
		name, bridge string
		isolate      bool
		reach        bool
	}{
		// Control: without veth isolation, tslink's forwarding lets it through
		{"control", "br-0123456789ab", false, true},
		{"docker-named bridge", "br-0123456789ab", true, false},
		{"default bridge", "docker0", true, false},
		{"custom-named bridge", "custombr0", true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			hostNS, bridgedNS, tslinkNS := vethTopology(t, strconv.Itoa(i), tt.bridge)
			if tt.isolate {
				if err := inNetNS(hostNS, func() error {
					return SetupVethIsolation(WireGuardTestPort)
				}); err != nil {
					t.Fatal(err)
				}
			}
			serveTCP(t, tslinkNS, "10.200.0.2:8080")
			if got := reaches(t, bridgedNS, "10.200.0.2:8080"); got != tt.reach {
				t.Errorf("reached the veth address: %v, want %v", got, tt.reach)
			}
		})
	}
}

// A tslink container still gets the replies on connections it opens, as
// tailscaled's to control, DERP and peers: they come back through the host.
// Guards: G2
func TestVethIsolationKeepsReplies(t *testing.T) {
	hostNS, bridgedNS, tslinkNS := vethTopology(t, "r", "custombr0")
	if err := inNetNS(hostNS, func() error {
		return SetupVethIsolation(WireGuardTestPort)
	}); err != nil {
		t.Fatal(err)
	}
	serveTCP(t, bridgedNS, "172.30.0.2:8080")
	if !reaches(t, tslinkNS, "172.30.0.2:8080") {
		t.Error("the tslink container's connection got no reply")
	}
}

// serveTCP accepts and closes connections on addr in the namespace ns until
// the test ends.
func serveTCP(t *testing.T, ns, addr string) {
	t.Helper()
	ln, err := ListenInNetNS(ns, addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ln.Close(); err != nil {
			t.Error(err)
		}
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			if err := conn.Close(); err != nil {
				return
			}
		}
	}()
}

// WireGuardTestPort is tailscaled's WireGuard port in these tests.
const WireGuardTestPort = 41641
