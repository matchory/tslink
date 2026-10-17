package docker

import (
	"strconv"
	"strings"
	"testing"

	"tailscale.com/tailcfg"
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
		ep := parseServeValue("web", "tslink.serve."+port, port, value)
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
		case !validPort(ep.Port):
			t.Fatalf("parseServeValue(%q, %q): accepted port %q", port, value, ep.Port)
		case ep.Path != "" && !strings.HasPrefix(ep.Path, "/"):
			t.Fatalf("parseServeValue(%q, %q): path %q", port, value, ep.Path)
		case ep.ProxyProtocol != "" && (!l4 || (ep.ProxyProtocol != "1" && ep.ProxyProtocol != "2")):
			t.Fatalf("parseServeValue(%q, %q): proxy-protocol %q", port, value, ep.ProxyProtocol)
		case ep.AcceptAppCaps != "" && (!l7 || !validAppCaps(ep.AcceptAppCaps)):
			t.Fatalf("parseServeValue(%q, %q): accept-app-caps %q", port, value, ep.AcceptAppCaps)
		}
	})
}

// validPort reports whether port is a TCP or UDP port number.
func validPort(port string) bool {
	n, err := strconv.ParseUint(port, 10, 16)
	return err == nil && n > 0
}

// FuzzParseServices checks that tslink.service.<name>.serve.<port> labels
// yield only valid, lower-case Service names, each with endpoints of known
// protocols on distinct ports.
//
// Guards: G3
func FuzzParseServices(f *testing.F) {
	f.Add("tslink.service.a.serve.443", "https:8080", "tslink.service.b.serve.80", "http:80")
	f.Add("tslink.service.a.serve.443", "tcp", "tslink.service.a.serve.0443", "tcp")
	f.Add("tslink.service..serve.443", "tcp", "tslink.service.x/../y.serve.1", "tcp")
	f.Fuzz(func(t *testing.T, k1, v1, k2, v2 string) {
		services, err := parseServices("web", map[string]string{k1: v1, k2: v2})
		if err != nil {
			return
		}
		for _, svc := range services {
			if tailcfg.ServiceName(svc.Name).Validate() != nil ||
				svc.Name != strings.ToLower(svc.Name) {
				t.Fatalf("parseServices: Service name %q", svc.Name)
			}
			seen := map[uint64]bool{}
			for _, ep := range svc.Endpoints {
				n, err := strconv.ParseUint(ep.Port, 10, 16)
				if err != nil || n == 0 || seen[n] {
					t.Fatalf("parseServices: %s port %q", svc.Name, ep.Port)
				}
				seen[n] = true
				switch ep.Proto {
				case "http", "https", "tcp", "tls-terminated-tcp", "tun":
				default:
					t.Fatalf("parseServices: %s protocol %q", svc.Name, ep.Proto)
				}
			}
		}
	})
}
