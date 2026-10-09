//go:build linux

package netutil

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// addVeth adds a veth from h's namespace, named name there, to the namespace
// at peerNS, named eth0 there.
func addVeth(t *testing.T, h *netlink.Handle, name, peerNS string) {
	t.Helper()
	fd, err := netns.GetFromPath(peerNS)
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	if err := h.LinkAdd(&netlink.Veth{
		Name: name, PeerName: "eth0", PeerNamespace: netlink.NsFd(fd),
	}); err != nil {
		t.Fatal(err)
	}
}

// natTopology builds a host namespace that masquerades two tslink containers
// behind veths, as tslink sets them up, and an upstream namespace on the
// host's uplink with two addresses, as two destinations on the internet. It
// returns the host's, the containers' and the upstream namespaces.
func natTopology(t *testing.T, id string) (string, [2]string, string) {
	t.Helper()
	pid := os.Getpid()
	hostNS, host := newNamedNetNS(t, fmt.Sprintf("tsl-%d-%s-h", pid, id))
	upNS, up := newNamedNetNS(t, fmt.Sprintf("tsl-%d-%s-u", pid, id))
	var tasks [2]string
	for i := range tasks {
		ns, h := newNamedNetNS(t, fmt.Sprintf("tsl-%d-%s-t%d", pid, id, i))
		veth := fmt.Sprintf("vethtp%d", i)
		addVeth(t, host, veth, ns)
		linkUp(t, host, veth, fmt.Sprintf("10.200.0.%d/30", 4*i+1), "")
		linkUp(
			t,
			h,
			"eth0",
			fmt.Sprintf("10.200.0.%d/30", 4*i+2),
			fmt.Sprintf("10.200.0.%d", 4*i+1),
		)
		tasks[i] = ns
	}
	addVeth(t, host, "up0", upNS)
	linkUp(t, host, "up0", "198.51.100.1/24", "")
	linkUp(t, up, "eth0", "198.51.100.2/24", "")
	a, err := netlink.ParseAddr("198.51.100.3/24")
	if err != nil {
		t.Fatal(err)
	}
	eth0, err := up.LinkByName("eth0")
	if err != nil {
		t.Fatal(err)
	}
	if err := up.AddrAdd(eth0, a); err != nil {
		t.Fatal(err)
	}

	if err := inNetNS(hostNS, func() error {
		resetChainInitialized()
		if err := SetupNAT("vethtp0"); err != nil {
			return err
		}
		return SetupNAT("vethtp1")
	}); err != nil {
		t.Fatal(err)
	}
	return hostNS, tasks, upNS
}

// udpSocket opens a UDP socket on port in the namespace ns.
func udpSocket(t *testing.T, ns string, ip string, port int) *net.UDPConn {
	t.Helper()
	var conn *net.UDPConn
	if err := inNetNS(ns, func() error {
		var err error
		conn, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(ip), Port: port})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	})
	return conn
}

// sourcePort sends a datagram from conn to dst, and returns the source port
// it arrives from at the listener on dst.
func sourcePort(t *testing.T, conn *net.UDPConn, dst *net.UDPConn) int {
	t.Helper()
	to, err := netip.ParseAddrPort(dst.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.WriteToUDPAddrPort([]byte("ping"), to); err != nil {
		t.Fatal(err)
	}
	if err := dst.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	_, from, err := dst.ReadFromUDPAddrPort(buf)
	if err != nil {
		t.Fatalf("no datagram at %s: %v", dst.LocalAddr(), err)
	}
	return int(from.Port())
}

// udpFlows returns the conntrack entries of UDP flows from src:sport in the
// namespace ns.
func udpFlows(t *testing.T, ns string, src netip.Addr, sport int) int {
	t.Helper()
	var n int
	if err := inNetNS(ns, func() error {
		flows, err := netlink.ConntrackTableList(netlink.ConntrackTable, netlink.FAMILY_V4)
		if err != nil {
			return err
		}
		for _, f := range flows {
			if f.Forward.Protocol == syscall.IPPROTO_UDP && f.Forward.SrcIP.Equal(src.AsSlice()) &&
				int(f.Forward.SrcPort) == sport {
				n++
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// A mapping added for a task whose flows exist already, as for a task an
// older version set up, applies to those flows too: the kernel keeps a flow's
// NAT for as long as its conntrack entry lives, and tailscaled's flows to
// STUN, DERP and peers never go idle long enough to expire. Mapping it again
// keeps the flows.
func TestPortMappingAppliesToExistingFlows(t *testing.T) {
	hostNS, tasks, upNS := natTopology(t, "pe")
	if err := inNetNS(hostNS, func() error {
		ipt, err := iptables.New()
		if err != nil {
			return err
		}
		return ipt.ClearChain("nat", portMapChain)
	}); err != nil {
		t.Fatal(err)
	}
	const port = WireGuardTestPort
	stun := udpSocket(t, upNS, "198.51.100.2", 3478)
	peer := udpSocket(t, upNS, "198.51.100.3", 3478)
	hostConn := udpSocket(t, hostNS, "", port)
	if got := sourcePort(t, hostConn, stun); got != port {
		t.Fatalf("the host's flow left on %d, want %d", got, port)
	}

	// Without the mapping the task's flows leave on two ports
	conn := udpSocket(t, tasks[0], "", port)
	if toSTUN, toPeer := sourcePort(t, conn, stun), sourcePort(t, conn, peer); toSTUN == toPeer {
		t.Fatalf("before the mapping the task left on %d to both", toSTUN)
	}

	var mapped map[netip.Addr]int
	if err := inNetNS(hostNS, func() error {
		if err := SyncPortMappings(); err != nil {
			return err
		}
		ipt, err := iptables.New()
		if err != nil {
			return err
		}
		mapped, err = listMappings(ipt)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	src := netip.MustParseAddr("10.200.0.2")
	want := mapped[src]
	if !DefaultPortRange.Contains(want) {
		t.Fatalf("mappings = %v, want one for %s", mapped, src)
	}
	for i, dst := range []*net.UDPConn{stun, peer} {
		if got := sourcePort(t, conn, dst); got != want {
			t.Errorf(
				"flow %d of the same socket left on %d after the mapping, want %d",
				i,
				got,
				want,
			)
		}
	}

	// Mapping again changes nothing: the flows stay
	if n := udpFlows(t, hostNS, src, port); n != 2 {
		t.Fatalf("%d flows from the task, want 2", n)
	}
	if err := inNetNS(hostNS, SyncPortMappings); err != nil {
		t.Fatal(err)
	}
	if err := inNetNS(hostNS, func() error { return SetupNAT("vethtp0") }); err != nil {
		t.Fatal(err)
	}
	if n := udpFlows(t, hostNS, src, port); n != 2 {
		t.Errorf("%d flows from the task after mapping it again, want 2", n)
	}
}

// Two tasks behind one host each leave the host on a single port of their
// own, whatever the destination, also towards one the host's own tailscaled
// already talks to from the same port: the endpoint STUN reports is the one
// peers see. The control run without the mappings shows the destination-
// dependent ports the global MASQUERADE gives.
func TestPortMappingIsEndpointIndependent(t *testing.T) {
	for _, mapped := range []bool{false, true} {
		t.Run(fmt.Sprintf("mapped=%t", mapped), func(t *testing.T) {
			id := "p0"
			if mapped {
				id = "p1"
			}
			hostNS, tasks, upNS := natTopology(t, id)
			if !mapped {
				if err := inNetNS(hostNS, func() error {
					ipt, err := iptables.New()
					if err != nil {
						return err
					}
					return ipt.ClearChain("nat", portMapChain)
				}); err != nil {
					t.Fatal(err)
				}
			}
			const port = WireGuardTestPort
			stun := udpSocket(t, upNS, "198.51.100.2", 3478)
			peer := udpSocket(t, upNS, "198.51.100.3", 3478)

			// The host's own tailscaled talks to the STUN server first
			hostConn := udpSocket(t, hostNS, "", port)
			if got := sourcePort(t, hostConn, stun); got != port {
				t.Fatalf("the host's flow left on %d, want %d", got, port)
			}

			var ports [2]int
			for i, ns := range tasks {
				conn := udpSocket(t, ns, "", port)
				toSTUN, toPeer := sourcePort(t, conn, stun), sourcePort(t, conn, peer)
				if !mapped {
					if toSTUN == toPeer {
						t.Errorf("control: task %d left on %d to both, want the "+
							"collision to remap its flow to the STUN server", i, toSTUN)
					}
					return
				}
				if toSTUN != toPeer || !DefaultPortRange.Contains(toSTUN) {
					t.Errorf("task %d left on %d to the STUN server and %d to the peer, "+
						"want one port in %s", i, toSTUN, toPeer, DefaultPortRange)
				}
				ports[i] = toSTUN
			}
			if ports[0] == ports[1] {
				t.Errorf("both tasks left on port %d", ports[0])
			}

			// The ports are the mappings', and go with the veths
			var got map[netip.Addr]int
			if err := inNetNS(hostNS, func() error {
				ipt, err := iptables.New()
				if err != nil {
					return err
				}
				if got, err = listMappings(ipt); err != nil {
					return err
				}
				if err := CleanupNAT("vethtp0"); err != nil {
					return err
				}
				return CleanupNAT("vethtp1")
			}); err != nil {
				t.Fatal(err)
			}
			for i, p := range ports {
				src := netip.AddrFrom4([4]byte{10, 200, 0, byte(4*i + 2)})
				if got[src] != p {
					t.Errorf("task %d left on %d, its mapping is %d", i, p, got[src])
				}
			}
			if after := listNATRules(t, hostNS).portMap; len(after) != 0 {
				t.Errorf("mappings after CleanupNAT: %v", after)
			}
		})
	}
}
