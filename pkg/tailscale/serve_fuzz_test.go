package tailscale

import (
	"regexp"
	"strings"
	"testing"

	"tailscale.com/ipn"
	"tailscale.com/tailcfg"
)

var (
	// proxyTarget matches the only target of a web handler: a port on the
	// container's loopback.
	proxyTarget = regexp.MustCompile(`^http://127\.0\.0\.1:[0-9]+$`)
	// tcpTarget matches the only target of TCP forwarding.
	tcpTarget = regexp.MustCompile(`^127\.0\.0\.1:[0-9]+$`)
)

// FuzzServeConfig checks that whatever a label puts into an endpoint, the
// serve config it becomes forwards to the container's loopback only, under
// the endpoint's Service or the node's own name only.
//
// Guards: G3
func FuzzServeConfig(f *testing.F) {
	f.Add("https", "443", "8080", "/api", "", "", "")
	f.Add("tcp", "5432", "5432", "", "2", "", "svc:db")
	f.Add("tun", "0", "0", "", "", "", "svc:x")
	f.Add("http", "80", "80", "", "", "example.com/cap", "svc:a --bg")
	f.Add("http", "80", "+80", "/x y", "", "", "")
	f.Add("http", "80", "80", "/../../x", "", "", "")
	f.Fuzz(func(t *testing.T, proto, port, target, path, proxyProtocol, appCaps, service string) {
		ep := ServeEndpoint{
			Proto: proto, Port: port, Target: target, Path: path,
			ProxyProtocol: proxyProtocol, AcceptAppCaps: appCaps,
		}
		const host, mds = "node.example.ts.net", "example.ts.net"
		svc := tailcfg.ServiceName(service)
		sc := new(ipn.ServeConfig)
		if err := applyServeEndpoint(sc, ep, svc, host, mds); err != nil {
			return
		}
		tcp, web := sc.TCP, sc.Web
		if svc != "" {
			if len(sc.TCP) != 0 || len(sc.Web) != 0 || len(sc.Services) > 1 {
				t.Fatalf("%+v for %q: serves outside the Service: %+v", ep, service, sc)
			}
			cfg := sc.Services[svc]
			if cfg == nil {
				t.Fatalf("%+v for %q: Service missing: %+v", ep, service, sc)
			}
			tcp, web = cfg.TCP, cfg.Web
		} else if len(sc.Services) != 0 {
			t.Fatalf("%+v: direct serve configured a Service: %+v", ep, sc)
		}
		checkTargets(t, ep, tcp, web)
	})
}

// checkTargets fails the test if a handler forwards anywhere but the
// container's loopback, or a web handler serves a path outside its mount.
func checkTargets(
	t *testing.T,
	ep ServeEndpoint,
	tcp map[uint16]*ipn.TCPPortHandler,
	web map[ipn.HostPort]*ipn.WebServerConfig,
) {
	t.Helper()
	for p, h := range tcp {
		if h.TCPForward != "" && !tcpTarget.MatchString(h.TCPForward) {
			t.Fatalf("%+v: port %d forwards to %q", ep, p, h.TCPForward)
		}
	}
	for hp, cfg := range web {
		for mount, h := range cfg.Handlers {
			if !proxyTarget.MatchString(h.Proxy) || h.Path != "" || h.Text != "" {
				t.Fatalf("%+v: %s%s serves %+v", ep, hp, mount, h)
			}
			if !strings.HasPrefix(mount, "/") || strings.Contains(mount, "/../") {
				t.Fatalf("%+v: %s has mount %q", ep, hp, mount)
			}
		}
	}
}
