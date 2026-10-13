package netutil

import (
	"cmp"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"syscall"

	"github.com/vishvananda/netlink"
)

// Host endpoints let tslink nodes on hosts that share a network go direct
// over it. A container's tailscaled sees only the container's interfaces, so
// it never advertises the host's addresses, as the host's own tailscaled
// does; tslink gives it the host's private addresses, with the container's
// external port (portMapChain), and tailscaled advertises them as explicitly
// configured endpoints. Public addresses need not be given: STUN finds them.

// MaxHostEndpoints is how many endpoints tailscaled takes from
// TS_DEBUG_PRETENDPOINT; it drops more without a word.
const MaxHostEndpoints = 3

// HostEndpointsConfig selects the addresses tslink nodes advertise: none if
// Off, Addrs if given, and otherwise the host's private addresses.
type HostEndpointsConfig struct {
	Off   bool
	Addrs []netip.Addr
}

// ConfigureHostEndpoints sets the addresses HostEndpointAddrs returns.
func ConfigureHostEndpoints(c HostEndpointsConfig) {
	defaultHost.hostEndpointsMu.Lock()
	defer defaultHost.hostEndpointsMu.Unlock()
	defaultHost.hostEndpoints = c
}

// HostEndpointAddrs returns the addresses tslink nodes advertise, as
// configured: none, the given ones, or the host's private addresses
// (selectHostAddrs).
func HostEndpointAddrs() ([]netip.Addr, error) {
	return defaultHost.hostEndpointAddrs()
}

// HostEndpoints returns the endpoints the tailscaled behind vethHost
// advertises: each of HostEndpointAddrs with its external port. None if no
// address is configured or found. The port comes from the mappings tslink
// wrote or last listed, so it starts no process: a rule another program
// removed counts until the next SyncPortMappings maps the container again.
func HostEndpoints(vethHost string) ([]netip.AddrPort, error) {
	return defaultHost.hostEndpointsOf(vethHost)
}

func (s *hostState) hostEndpointAddrs() ([]netip.Addr, error) {
	s.hostEndpointsMu.Lock()
	c := s.hostEndpoints
	s.hostEndpointsMu.Unlock()
	switch {
	case c.Off:
		return nil, nil
	case c.Addrs != nil:
		return slices.Clone(c.Addrs), nil
	}
	links, err := hostLinks()
	if err != nil {
		return nil, err
	}
	return selectHostAddrs(links), nil
}

// hostLink is what selectHostAddrs needs of an interface.
type hostLink struct {
	Index    int
	Type     string // netlink's link type: device, bond, vlan, bridge, veth, ...
	Up       bool
	Loopback bool
	Addrs    []netip.Addr
}

// hostLinkTypes are the link types of a host's own network: NICs, and the
// bonds and VLANs built on them. A bridge may be Docker's, libvirt's or
// LXD's, and tun, wireguard and vxlan devices are other networks.
var hostLinkTypes = []string{"device", "bond", "vlan"}

// selectHostAddrs returns the private IPv4 addresses of the links of
// hostLinkTypes that are up, in interface order, at most MaxHostEndpoints.
func selectHostAddrs(links []hostLink) []netip.Addr {
	links = slices.Clone(links)
	slices.SortFunc(links, func(a, b hostLink) int { return cmp.Compare(a.Index, b.Index) })
	var addrs []netip.Addr
	for _, l := range links {
		if !l.Up || l.Loopback || !slices.Contains(hostLinkTypes, l.Type) {
			continue
		}
		for _, a := range l.Addrs {
			if len(addrs) < MaxHostEndpoints && a.Is4() && a.IsPrivate() &&
				a.IsGlobalUnicast() && !vethRange.Contains(a) {
				addrs = append(addrs, a)
			}
		}
	}
	return addrs
}

// hostLinks lists the host's interfaces and their IPv4 addresses, with one
// netlink dump of each: AddrList(link, ...) would otherwise dump every
// address and filter client-side, for every link.
func hostLinks() ([]hostLink, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("failed to list interfaces: %w", err)
	}
	addrs, err := netlink.AddrList(nil, syscall.AF_INET)
	if err != nil {
		return nil, fmt.Errorf("failed to list addresses: %w", err)
	}
	byIndex := map[int][]netip.Addr{}
	for _, a := range addrs {
		if ip, ok := netip.AddrFromSlice(a.IP); ok {
			byIndex[a.LinkIndex] = append(byIndex[a.LinkIndex], ip.Unmap())
		}
	}
	out := make([]hostLink, 0, len(links))
	for _, l := range links {
		attrs := l.Attrs()
		out = append(out, hostLink{
			Index:    attrs.Index,
			Type:     l.Type(),
			Up:       attrs.Flags&net.FlagUp != 0,
			Loopback: attrs.Flags&net.FlagLoopback != 0,
			Addrs:    byIndex[attrs.Index],
		})
	}
	return out, nil
}

// ErrNoPortMapping is HostEndpoints' error for a container whose tailscaled
// has no external port (portMapChain) at the moment, as while its rule is
// missing until the next SyncPortMappings.
var ErrNoPortMapping = errors.New("no external port mapped")

func (s *hostState) hostEndpointsOf(vethHost string) ([]netip.AddrPort, error) {
	addrs, err := s.hostEndpointAddrs()
	if err != nil || len(addrs) == 0 {
		return nil, err
	}
	src, err := containerAddr(vethHost)
	if err != nil {
		return nil, err
	}
	s.portMapMu.Lock()
	port, ok := s.ports[src]
	s.portMapMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%s: %w", src, ErrNoPortMapping)
	}
	return hostEndpointsOf(addrs, port), nil
}

// hostEndpointsOf returns each of addrs with port.
func hostEndpointsOf(addrs []netip.Addr, port int) []netip.AddrPort {
	if len(addrs) == 0 {
		return nil
	}
	eps := make([]netip.AddrPort, len(addrs))
	for i, a := range addrs {
		eps[i] = netip.AddrPortFrom(a, uint16(port)) //nolint:gosec // ports are at most 65535
	}
	return eps
}
