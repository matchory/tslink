package tailscale

import (
	"bufio"
	"bytes"
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// Where tailscaled's upstream resolvers come from. Docker gives the plugin
// the host's /etc/resolv.conf, and the plugin's /run is the host's.
var (
	hostResolvConfPath     = "/etc/resolv.conf"
	resolvedResolvConfPath = "/run/systemd/resolve/resolv.conf"
)

var (
	// Docker's embedded resolver, in every container on a tslink network.
	dockerResolver = netip.MustParseAddr("127.0.0.11")

	// Tailscale's own resolver, which in the container's network namespace is
	// the container's tailscaled.
	tailscaleResolvers = []netip.Addr{
		netip.MustParseAddr("100.100.100.100"),
		netip.MustParseAddr("fd7a:115c:a1e0::53"),
	}

	// What Docker falls back to when the host has no resolver a container can
	// reach.
	dockerDefaultResolvers = []netip.Addr{
		netip.MustParseAddr("8.8.8.8"),
		netip.MustParseAddr("8.8.4.4"),
	}
)

// resolvConf holds what tailscaled reads from a resolv.conf.
type resolvConf struct {
	nameservers []netip.Addr
	search      []string
	options     []string
}

// parseResolvConf parses b as Docker does: the last search (or domain) line
// wins, options accumulate, and invalid nameservers and other lines are
// dropped.
func parseResolvConf(b []byte) resolvConf {
	var rc resolvConf
	scanner := bufio.NewScanner(bytes.NewReader(b))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "nameserver":
			if addr, err := netip.ParseAddr(fields[1]); err == nil {
				rc.nameservers = append(rc.nameservers, addr)
			}
		case "search", "domain":
			rc.search = fields[1:]
		case "options":
			rc.options = append(rc.options, fields[1:]...)
		}
	}
	return rc
}

// reachable reports whether the container's network namespace can reach a
// resolver at addr, other than its own tailscaled: not one on the host's
// loopback, nor IPv6 link-local, which is scoped to a host interface.
func reachable(addr netip.Addr) bool {
	addr = addr.Unmap()
	if addr.Is6() && addr.IsLinkLocalUnicast() {
		return false
	}
	return addr.IsValid() && !addr.IsLoopback() && !addr.IsUnspecified() &&
		!slices.Contains(tailscaleResolvers, addr)
}

// tailscaledResolvConf returns the resolv.conf tailscaled reads, for its own
// lookups and as the upstreams of its DNS forwarder, from the host's
// resolv.conf, systemd-resolved's (nil if absent), and the resolvers the
// container was given. It returns a warning when it falls back.
//
// The rule is Docker's for the container's embedded resolver: the container's
// own resolvers, without Tailscale's, which is the container's tailscaled, and
// Docker's embedded one, which forwards to the others. A resolver on the
// loopback is kept: it runs in the container's network namespace, which
// tailscaled shares. Without any, it takes the host's resolvers without those
// on its loopback, and systemd-resolved's upstreams when that leaves none.
// The search and options lines are those of the host's resolv.conf, or of
// systemd-resolved's if its upstreams are used.
func tailscaledResolvConf(host, resolved []byte, containerDNS []netip.Addr) ([]byte, string) {
	rc := hostResolvConf(host, resolved)
	own := slices.DeleteFunc(slices.Clone(containerDNS), func(a netip.Addr) bool {
		a = a.Unmap()
		return a == dockerResolver || slices.Contains(tailscaleResolvers, a)
	})
	if len(own) > 0 {
		rc.nameservers = own
	}

	var warning string
	if len(rc.nameservers) == 0 {
		rc.nameservers, warning = fallbackResolvers(containerDNS)
	}

	var b strings.Builder
	for _, ns := range rc.nameservers {
		fmt.Fprintf(&b, "nameserver %s\n", ns)
	}
	if len(rc.search) > 0 {
		fmt.Fprintf(&b, "search %s\n", strings.Join(rc.search, " "))
	}
	if len(rc.options) > 0 {
		fmt.Fprintf(&b, "options %s\n", strings.Join(rc.options, " "))
	}
	return []byte(b.String()), warning
}

// hostResolvConf parses the host's resolv.conf, without the resolvers a
// container cannot reach; if that leaves none, systemd-resolved's, if that
// names any.
func hostResolvConf(host, resolved []byte) resolvConf {
	unreachable := func(a netip.Addr) bool { return !reachable(a) }
	rc := parseResolvConf(host)
	rc.nameservers = slices.DeleteFunc(rc.nameservers, unreachable)
	if len(rc.nameservers) == 0 {
		alt := parseResolvConf(resolved)
		alt.nameservers = slices.DeleteFunc(alt.nameservers, unreachable)
		if len(alt.nameservers) > 0 {
			rc = alt
		}
	}
	return rc
}

// fallbackResolvers returns the resolvers for tailscaled when neither the
// container nor the host has one it can use, and a warning saying so: Docker's
// embedded resolver, unless the container uses Tailscale's resolver, its
// tailscaled, to which Docker's would forward; then Docker's default resolvers.
func fallbackResolvers(containerDNS []netip.Addr) ([]netip.Addr, string) {
	if !slices.ContainsFunc(
		containerDNS,
		func(a netip.Addr) bool { return slices.Contains(tailscaleResolvers, a.Unmap()) },
	) {
		return []netip.Addr{dockerResolver},
			"the host has no resolver a container can reach: tailscaled uses Docker's embedded resolver"
	}
	return dockerDefaultResolvers, fmt.Sprintf(
		"the host has no resolver a container can reach, and Docker's embedded resolver "+
			"forwards to Tailscale's (the container's DNS is %v): tailscaled uses %v",
		containerDNS,
		dockerDefaultResolvers,
	)
}
