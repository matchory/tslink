//go:build linux

package netutil

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"
)

// lanHost is one side of lanTopology: the host's namespace, its LAN address,
// its task's namespace and the task's external port.
type lanHost struct {
	ns, task string
	lan      netip.Addr
	port     int
}

// lanTopology builds two hosts on a shared LAN (192.0.2.0/24), each with one
// tslink task behind a veth, NATed as SetupNAT does. The tasks' veth subnets
// differ (10.200.0.0/30 and 10.200.0.4/30), as on two real hosts they may not.
func lanTopology(t *testing.T, id string) [2]lanHost {
	t.Helper()
	pid := os.Getpid()
	var hosts [2]lanHost
	var handles [2]*netlink.Handle
	for i := range hosts {
		ns, h := newNamedNetNS(t, fmt.Sprintf("tsl-%d-%s-h%d", pid, id, i))
		taskNS, th := newNamedNetNS(t, fmt.Sprintf("tsl-%d-%s-t%d", pid, id, i))
		veth := fmt.Sprintf("vethln%d", i)
		addVeth(t, h, veth, taskNS)
		linkUp(t, h, veth, fmt.Sprintf("10.200.0.%d/30", 4*i+1), "")
		linkUp(
			t,
			th,
			"eth0",
			fmt.Sprintf("10.200.0.%d/30", 4*i+2),
			fmt.Sprintf("10.200.0.%d", 4*i+1),
		)
		handles[i] = h
		hosts[i] = lanHost{
			ns:   ns,
			task: taskNS,
			lan:  netip.MustParseAddr(fmt.Sprintf("192.0.2.%d", i+1)),
		}
	}
	// The LAN: a veth between the two host namespaces, lan0 in host 0 and
	// eth0 (addVeth's name for the peer) in host 1
	addVeth(t, handles[0], "lan0", hosts[1].ns)
	linkUp(t, handles[0], "lan0", "192.0.2.1/24", "")
	linkUp(t, handles[1], "eth0", "192.0.2.2/24", "")
	for i := range hosts {
		veth := fmt.Sprintf("vethln%d", i)
		if err := inNetNS(hosts[i].ns, func() error {
			if err := SetupNAT(veth); err != nil {
				return err
			}
			ipt, err := iptables.New()
			if err != nil {
				return err
			}
			mapped, err := listMappings(ipt)
			if err != nil {
				return err
			}
			hosts[i].port = mapped[netip.MustParseAddr(fmt.Sprintf("10.200.0.%d", 4*i+2))]
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if hosts[i].port == 0 {
			t.Fatalf("host %d: task has no external port", i)
		}
	}
	return hosts
}

// send sends one datagram from conn to addr:port.
func send(t *testing.T, conn *net.UDPConn, addr netip.Addr, port int) {
	t.Helper()
	if _, err := conn.WriteToUDPAddrPort(
		[]byte("disco"),
		netip.AddrPortFrom(addr, uint16(port)),
	); err != nil {
		t.Fatal(err)
	}
}

// arrives reports whether a datagram arrives at conn within d.
func arrives(t *testing.T, conn *net.UDPConn, d time.Duration) bool {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(d)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	_, _, err := conn.ReadFromUDPAddrPort(buf)
	return err == nil
}

// probeFirst plays hole punching with host 1's task probing first: it sends
// to host 0's advertised endpoint before host 0's task has sent anything;
// then host 0's task sends to host 1's endpoint, and host 1's again. It
// reports whether each of the two later packets arrived.
func probeFirst(t *testing.T, hosts [2]lanHost) (bool, bool) {
	t.Helper()
	c0 := udpSocket(t, hosts[0].task, "", WireGuardTestPort)
	c1 := udpSocket(t, hosts[1].task, "", WireGuardTestPort)
	send(t, c1, hosts[0].lan, hosts[0].port) // arrives unsolicited at host 0
	time.Sleep(200 * time.Millisecond)
	send(t, c0, hosts[1].lan, hosts[1].port)
	toTask1 := arrives(t, c1, time.Second)
	send(t, c1, hosts[0].lan, hosts[0].port)
	toTask0 := arrives(t, c0, time.Second)
	return toTask1, toTask0
}

// Arm A, the control: with an INPUT that accepts, the unsolicited probe
// leaves a conntrack entry on host 0 that blocks its task's own packets to
// the probing peer. If this does not reproduce, TSLINK-WG-IN is unneeded:
// stop and report (see the spec).
func TestHostEndpointRaceWithoutInputDrop(t *testing.T) {
	hosts := lanTopology(t, "ra")
	toTask1, _ := probeFirst(t, hosts)
	if toTask1 {
		t.Fatal(
			"host 0's task reached host 1's after an unsolicited probe: the race the drop is for did not happen",
		)
	}
}

// Arm B: a host whose INPUT drops everything new (a default-deny firewall)
// never had the race.
func TestHostEndpointPathWithDefaultDenyInput(t *testing.T) {
	hosts := lanTopology(t, "rb")
	for _, h := range hosts {
		if err := inNetNS(h.ns, func() error {
			ipt, err := iptables.New()
			if err != nil {
				return err
			}
			return ipt.Append(
				"filter",
				"INPUT",
				"-m",
				"conntrack",
				"--ctstate",
				"NEW",
				"-j",
				"DROP",
			)
		}); err != nil {
			t.Fatal(err)
		}
	}
	if toTask1, toTask0 := probeFirst(t, hosts); !toTask1 || !toTask0 {
		t.Fatalf("path did not form: to task 1 %v, to task 0 %v", toTask1, toTask0)
	}
}

// Arm C: with tslink's veth isolation, which installs TSLINK-WG-IN, the path
// forms whoever probes first, and when both start at once; and the host's own
// UDP clients on a port in the range still get their replies.
func TestHostEndpointPathWithWireGuardInput(t *testing.T) {
	hosts := lanTopology(t, "rc")
	for _, h := range hosts {
		if err := inNetNS(
			h.ns,
			func() error { return SetupVethIsolation(WireGuardTestPort) },
		); err != nil {
			t.Fatal(err)
		}
	}
	if toTask1, toTask0 := probeFirst(t, hosts); !toTask1 || !toTask0 {
		t.Fatalf("probe first: to task 1 %v, to task 0 %v", toTask1, toTask0)
	}
}

func TestHostEndpointSimultaneousStart(t *testing.T) {
	hosts := lanTopology(t, "rs")
	for _, h := range hosts {
		if err := inNetNS(
			h.ns,
			func() error { return SetupVethIsolation(WireGuardTestPort) },
		); err != nil {
			t.Fatal(err)
		}
	}
	c0 := udpSocket(t, hosts[0].task, "", WireGuardTestPort)
	c1 := udpSocket(t, hosts[1].task, "", WireGuardTestPort)
	ok0, ok1 := false, false
	for range 5 { // as magicsock retries
		send(t, c0, hosts[1].lan, hosts[1].port)
		send(t, c1, hosts[0].lan, hosts[0].port)
		ok1 = ok1 || arrives(t, c1, 300*time.Millisecond)
		ok0 = ok0 || arrives(t, c0, 300*time.Millisecond)
	}
	if !ok0 || !ok1 {
		t.Fatalf("simultaneous start: task 0 got %v, task 1 got %v", ok0, ok1)
	}
}

func TestWireGuardInputKeepsHostClientReplies(t *testing.T) {
	hosts := lanTopology(t, "rh")
	if err := inNetNS(
		hosts[0].ns,
		func() error { return SetupVethIsolation(WireGuardTestPort) },
	); err != nil {
		t.Fatal(err)
	}
	server := udpSocket(t, hosts[1].ns, hosts[1].lan.String(), 5353)
	client := udpSocket(t, hosts[0].ns, "", DefaultPortRange.First+7) // a host client in the range
	send(t, client, hosts[1].lan, 5353)
	if !arrives(t, server, time.Second) {
		t.Fatal("the client's request did not arrive")
	}
	if err := server.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	send(t, server, hosts[0].lan, DefaultPortRange.First+7)
	if !arrives(t, client, time.Second) {
		t.Fatal("TSLINK-WG-IN dropped the reply to a host client in the range")
	}
}

// hostLinks lists every link's own addresses, grouped by LinkIndex from one
// AddrList dump rather than one per link.
func TestHostLinksGroupsAddressesByLink(t *testing.T) {
	pid := os.Getpid()
	ns, h := newNamedNetNS(t, fmt.Sprintf("tsl-%d-hl-a", pid))
	peerNS, _ := newNamedNetNS(t, fmt.Sprintf("tsl-%d-hl-b", pid))
	addVeth(t, h, "vethhl", peerNS)
	linkUp(t, h, "vethhl", "10.200.9.1/30", "")
	if err := inNetNS(ns, func() error {
		links, err := hostLinks()
		if err != nil {
			return err
		}
		for _, l := range links {
			if l.Type != "veth" {
				continue
			}
			want := netip.MustParseAddr("10.200.9.1")
			if len(l.Addrs) == 1 && l.Addrs[0] == want {
				return nil
			}
			return fmt.Errorf("vethhl addrs = %v, want [%v]", l.Addrs, want)
		}
		return errors.New("vethhl not found")
	}); err != nil {
		t.Fatal(err)
	}
}

// The endpoint follows the task's port mapping: present while it exists,
// ErrNoPortMapping once CleanupNAT removed it, and back once SyncPortMappings
// restores it. A rule another program flushes counts until then: the
// mappings come from what tslink wrote or listed, not from iptables.
func TestHostEndpointsFollowPortMapping(t *testing.T) {
	hosts := lanTopology(t, "rd")
	ConfigureHostEndpoints(HostEndpointsConfig{Addrs: []netip.Addr{hosts[0].lan}})
	t.Cleanup(func() { ConfigureHostEndpoints(HostEndpointsConfig{}) })
	if err := inNetNS(hosts[0].ns, func() error {
		got, err := HostEndpoints("vethln0")
		if err != nil {
			return err
		}
		want := netip.AddrPortFrom(hosts[0].lan, uint16(hosts[0].port))
		if len(got) != 1 || got[0] != want {
			return fmt.Errorf("HostEndpoints = %v, want [%v]", got, want)
		}
		if err := CleanupNAT("vethln0"); err != nil {
			return err
		}
		if _, err := HostEndpoints("vethln0"); !errors.Is(err, ErrNoPortMapping) {
			return fmt.Errorf("without a rule: %w, want ErrNoPortMapping", err)
		}
		if err := SyncPortMappings(); err != nil {
			return err
		}
		got, err = HostEndpoints("vethln0")
		if err != nil || len(got) != 1 || got[0].Addr() != hosts[0].lan {
			return fmt.Errorf("after sync: %v, %w", got, err)
		}
		// A flushed chain goes unnoticed until the watchdog's sync, which
		// maps the task again
		ipt, err := iptables.New()
		if err != nil {
			return err
		}
		if err := ipt.ClearChain("nat", portMapChain); err != nil {
			return err
		}
		if err := SyncPortMappings(); err != nil {
			return err
		}
		mapped, err := listMappings(ipt)
		if err != nil {
			return err
		}
		port := mapped[netip.MustParseAddr("10.200.0.2")]
		got, err = HostEndpoints("vethln0")
		if err != nil || len(got) != 1 || port == 0 || int(got[0].Port()) != port {
			return fmt.Errorf("after a flush and sync: %v, %w, want port %d", got, err, port)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
