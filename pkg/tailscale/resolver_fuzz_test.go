package tailscale

import (
	"net/netip"
	"strings"
	"testing"
	"unicode"
)

// FuzzTailscaledResolvConf checks that whatever the host's resolv.conf files
// and the container's DNS servers hold, tailscaled gets at least one
// nameserver, and only nameserver lines with an address, and search and
// options lines without any whitespace but single spaces.
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
	f.Fuzz(func(t *testing.T, host, resolved, dns []byte) {
		var addrs []netip.Addr
		for ; len(dns) >= 4; dns = dns[4:] {
			addr, _ := netip.AddrFromSlice(dns[:4])
			addrs = append(addrs, addr)
		}
		out, _ := tailscaledResolvConf(host, resolved, addrs)
		nameservers := 0
		for line := range strings.SplitSeq(strings.TrimSuffix(string(out), "\n"), "\n") {
			keyword, rest, _ := strings.Cut(line, " ")
			switch keyword {
			case "nameserver":
				nameservers++
				if _, err := netip.ParseAddr(rest); err != nil {
					t.Fatalf("resolv.conf %q: nameserver %q", out, rest)
				}
			case "search", "options":
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
