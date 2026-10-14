package tailscale

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
	"unicode"
)

// FuzzTailscaledResolvConf checks that whatever the host's resolv.conf files
// and the container's DNS servers hold, tailscaled gets at least one
// nameserver, and only nameserver lines with an address, and at most one
// search and one options line, without any whitespace but single spaces. And
// it never resolves through itself: no nameserver is Tailscale's resolver, and
// Docker's embedded resolver only when the container's DNS does not forward
// to Tailscale's.
//
// Guards: G3
func FuzzTailscaledResolvConf(f *testing.F) {
	f.Add(
		[]byte("nameserver 1.1.1.1\nsearch a b\noptions ndots:2\n"),
		[]byte(""),
		[]byte{8, 8, 8, 8},
	)
	f.Add(
		[]byte("nameserver 127.0.0.53\n"),
		[]byte("nameserver 9.9.9.9\nsearch x\n"),
		[]byte{100, 100, 100, 100},
	)
	f.Add(
		[]byte("search a\x0bb\r\noptions x\x00y\nnameserver fe80::1%eth0\n"),
		[]byte(nil),
		[]byte{127, 0, 0, 11},
	)
	// No resolver to use but the container's tailscaled: the fallback
	f.Add([]byte("nameserver 127.0.0.53\n"), []byte(nil), []byte{100, 100, 100, 100})
	f.Fuzz(func(t *testing.T, host, resolved, dns []byte) {
		var addrs []netip.Addr
		for ; len(dns) >= 4; dns = dns[4:] {
			addr, _ := netip.AddrFromSlice(dns[:4])
			addrs = append(addrs, addr)
		}
		out, _ := tailscaledResolvConf(host, resolved, addrs)
		nameservers := 0
		seen := map[string]bool{}
		for line := range strings.SplitSeq(strings.TrimSuffix(string(out), "\n"), "\n") {
			keyword, rest, _ := strings.Cut(line, " ")
			switch keyword {
			case "nameserver":
				nameservers++
				addr, err := netip.ParseAddr(rest)
				if err != nil {
					t.Fatalf("resolv.conf %q: nameserver %q", out, rest)
				}
				checkNoLoop(t, out, addr.Unmap(), addrs)
			case "search", "options":
				if seen[keyword] {
					t.Fatalf("resolv.conf %q: a second %s line", out, keyword)
				}
				seen[keyword] = true
				if strings.TrimSpace(rest) == "" ||
					strings.ContainsFunc(
						rest,
						func(r rune) bool { return r != ' ' && unicode.IsSpace(r) },
					) {
					t.Fatalf("resolv.conf %q: line %q", out, line)
				}
			default:
				t.Fatalf("resolv.conf %q: line %q", out, line)
			}
		}
		if nameservers == 0 {
			t.Fatalf("resolv.conf %q has no nameserver", out)
		}
	})
}

// checkNoLoop fails the test if nameserver addr leads tailscaled back to
// itself: Tailscale's resolver is the container's tailscaled, and Docker's
// embedded resolver forwards to the container's DNS servers.
func checkNoLoop(t *testing.T, out []byte, addr netip.Addr, containerDNS []netip.Addr) {
	t.Helper()
	if slices.Contains(tailscaleResolvers, addr) {
		t.Fatalf("resolv.conf %q: nameserver %s is Tailscale's resolver", out, addr)
	}
	if addr == dockerResolver && slices.ContainsFunc(containerDNS, func(a netip.Addr) bool {
		return slices.Contains(tailscaleResolvers, a.Unmap())
	}) {
		t.Fatalf(
			"resolv.conf %q: Docker's resolver forwards to Tailscale's (%v)",
			out,
			containerDNS,
		)
	}
}
