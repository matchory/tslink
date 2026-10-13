//go:build linux

package netutil

import (
	"net"
	"strconv"
	"testing"

	"github.com/vishvananda/netlink"
)

// Docker lists the address its IPAM assigned to the endpoint in the
// container's /etc/hosts. Once addIPAMAddress put it on the container, a
// process reaching its own name gets an answer, as RabbitMQ's epmd needs.
func TestAddIPAMAddress(t *testing.T) {
	nsPath := newTestNetNS(t)

	h, link := testHandle(t, nsPath, "dummy0")
	for range 2 { // Join may be retried
		if err := addIPAMAddress(h, link, "10.42.6.51"); err != nil {
			t.Fatalf("addIPAMAddress: %v", err)
		}
	}

	if err := inNetNS(nsPath, func() error {
		// Local delivery goes through lo, which a container has up
		lo, err := netlink.LinkByName("lo")
		if err != nil {
			return err
		}
		if err := netlink.LinkSetUp(lo); err != nil {
			return err
		}
		link, err := netlink.LinkByName("dummy0")
		if err != nil {
			return err
		}
		addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
		if err != nil {
			return err
		}
		var found []string
		for _, a := range addrs {
			found = append(found, a.IPNet.String())
		}
		if len(found) != 1 || found[0] != "10.42.6.51/32" {
			t.Errorf("dummy0 has %v, want only 10.42.6.51/32", found)
		}

		// No subnet route: the network's other addresses stay unknown here
		_, subnet, _ := net.ParseCIDR("10.42.6.0/24")
		routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4,
			&netlink.Route{Dst: subnet}, netlink.RT_FILTER_DST)
		if err != nil {
			return err
		}
		if len(routes) != 0 {
			t.Errorf("routes to %s: %v", subnet, routes)
		}

		l, err := net.Listen("tcp", "0.0.0.0:0")
		if err != nil {
			return err
		}
		defer l.Close()
		port := l.Addr().(*net.TCPAddr).Port
		go func() {
			if c, err := l.Accept(); err == nil {
				c.Close()
			}
		}()
		c, err := net.Dial("tcp", net.JoinHostPort("10.42.6.51", strconv.Itoa(port)))
		if err != nil {
			t.Errorf("connect to the IPAM address: %v", err)
			return nil
		}
		return c.Close()
	}); err != nil {
		t.Fatalf("in netns: %v", err)
	}
}

func TestAddIPAMAddressRejectsBadAddress(t *testing.T) {
	nsPath := newTestNetNS(t)
	h, link := testHandle(t, nsPath, "dummy0")
	if err := addIPAMAddress(h, link, "not-an-ip"); err == nil {
		t.Error("addIPAMAddress accepted an invalid address")
	}
}
