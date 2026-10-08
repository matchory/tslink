package docker

import (
	"strings"
	"testing"
)

// FuzzParseServeValue checks that a tslink.serve label yields an endpoint
// only with a known protocol, the label's port, a path that starts with a
// slash, and options valid for its protocol.
//
// Guards: G3
func FuzzParseServeValue(f *testing.F) {
	for _, v := range []string{
		"https:8080", "http:3000/api", "tcp", "tcp:5432?proxy-protocol=2",
		"https?accept-app-caps=example.com/cap", "tun", "ftp:21", "http:80?x=y", "",
		"http:80/a?proxy-protocol=1", "HTTPS:443", "tcp:--bg",
	} {
		f.Add("443", v)
	}
	f.Fuzz(func(t *testing.T, port, value string) {
		ep := parseServeValue(port, value)
		if ep == nil {
			return
		}
		switch ep.Proto {
		case "http", "https", "tcp", "tls-terminated-tcp", "tun":
		default:
			t.Fatalf("parseServeValue(%q, %q): protocol %q", port, value, ep.Proto)
		}
		l4 := ep.Proto == "tcp" || ep.Proto == "tls-terminated-tcp"
		l7 := ep.Proto == "http" || ep.Proto == "https"
		switch {
		case ep.Port != port:
			t.Fatalf("parseServeValue(%q, %q): port %q", port, value, ep.Port)
		case ep.Path != "" && !strings.HasPrefix(ep.Path, "/"):
			t.Fatalf("parseServeValue(%q, %q): path %q", port, value, ep.Path)
		case ep.ProxyProtocol != "" && (!l4 || (ep.ProxyProtocol != "1" && ep.ProxyProtocol != "2")):
			t.Fatalf("parseServeValue(%q, %q): proxy-protocol %q", port, value, ep.ProxyProtocol)
		case ep.AcceptAppCaps != "" && (!l7 || !validAppCaps(ep.AcceptAppCaps)):
			t.Fatalf("parseServeValue(%q, %q): accept-app-caps %q", port, value, ep.AcceptAppCaps)
		}
	})
}
