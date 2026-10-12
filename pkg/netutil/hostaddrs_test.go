package netutil

import (
	"net/netip"
	"slices"
	"testing"
)

func addrs(s ...string) []netip.Addr {
	out := make([]netip.Addr, len(s))
	for i, a := range s {
		out[i] = netip.MustParseAddr(a)
	}
	return out
}

// Only the private IPv4 addresses of NICs and what is built on them count, in
// interface order, at most three: bridges (Docker's, libvirt's), veths,
// tun and wireguard devices are not the hosts' shared network.
func TestSelectHostAddrs(t *testing.T) {
	links := []hostLink{
		{Index: 3, Type: "device", Up: true, Addrs: addrs("10.10.0.2", "fd00::2")},
		{Index: 1, Type: "device", Up: true, Loopback: true, Addrs: addrs("127.0.0.1")},
		{Index: 2, Type: "device", Up: true, Addrs: addrs("138.199.145.146")},
		{Index: 4, Type: "bridge", Up: true, Addrs: addrs("10.242.0.1")},    // docker_gwbridge
		{Index: 5, Type: "bridge", Up: true, Addrs: addrs("192.168.122.1")}, // virbr0
		{Index: 6, Type: "veth", Up: true, Addrs: addrs("10.200.0.1")},
		{Index: 7, Type: "tuntap", Up: true, Addrs: addrs("100.90.29.91")},
		{Index: 8, Type: "wireguard", Up: true, Addrs: addrs("10.99.0.1")},
		{Index: 9, Type: "vlan", Up: true, Addrs: addrs("172.16.5.2")},
		{Index: 10, Type: "bond", Up: false, Addrs: addrs("192.168.1.2")},
		{Index: 11, Type: "bond", Up: true, Addrs: addrs("192.168.1.3", "192.168.1.4")},
	}
	want := addrs("10.10.0.2", "172.16.5.2", "192.168.1.3")
	if got := selectHostAddrs(links); !slices.Equal(got, want) {
		t.Errorf("selectHostAddrs = %v, want %v", got, want)
	}
	if got := selectHostAddrs(nil); len(got) != 0 {
		t.Errorf("no links: %v", got)
	}
}

// An address in tslink's veth range is never the host's network, whatever
// its interface claims.
func TestSelectHostAddrsSkipsVethRange(t *testing.T) {
	links := []hostLink{
		{Index: 2, Type: "device", Up: true, Addrs: addrs("10.200.3.1", "10.10.0.9")},
	}
	if got := selectHostAddrs(links); !slices.Equal(got, addrs("10.10.0.9")) {
		t.Errorf("selectHostAddrs = %v", got)
	}
}

// Off advertises nothing, explicit addresses are taken as given, and the
// default detects them.
func TestHostEndpointAddrsConfig(t *testing.T) {
	t.Cleanup(func() { ConfigureHostEndpoints(HostEndpointsConfig{}) })

	ConfigureHostEndpoints(HostEndpointsConfig{Off: true})
	if got, err := HostEndpointAddrs(); err != nil || len(got) != 0 {
		t.Errorf("off: %v, %v", got, err)
	}
	ConfigureHostEndpoints(HostEndpointsConfig{Addrs: addrs("10.1.2.3")})
	if got, err := HostEndpointAddrs(); err != nil || !slices.Equal(got, addrs("10.1.2.3")) {
		t.Errorf("explicit: %v, %v", got, err)
	}
}
