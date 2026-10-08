package tailscale

import (
	"regexp"
	"testing"
)

var (
	// serveFlag matches the flags serveArgs may pass, each with its value
	// after "=", so no value can become an argument of its own.
	serveFlag = regexp.MustCompile(
		`^--(service|http|https|tcp|tls-terminated-tcp|set-path|proxy-protocol|accept-app-caps)=`,
	)
	// serveTarget matches the only positional argument: a port on the
	// container's loopback.
	serveTarget = regexp.MustCompile(`^(http://|tcp://)?127\.0\.0\.1:[+-]?[0-9]+$`)
)

// FuzzServeArgs checks that whatever a label puts into an endpoint, "tailscale
// serve" gets only known flags with their values attached, and forwards to
// the container's loopback only.
//
// Guards: G3
func FuzzServeArgs(f *testing.F) {
	f.Add("https", "443", "8080", "/api", "", "", "")
	f.Add("tcp", "5432", "5432", "", "2", "", "svc:db")
	f.Add("tun", "0", "0", "", "", "", "svc:x")
	f.Add("http", "80", "80", "", "", "example.com/cap", "svc:a --bg")
	f.Add("http", "80", "+80", "/x y", "", "", "")
	f.Fuzz(func(t *testing.T, proto, port, target, path, proxyProtocol, appCaps, service string) {
		ep := ServeEndpoint{
			Proto: proto, Port: port, Target: target, Path: path,
			ProxyProtocol: proxyProtocol, AcceptAppCaps: appCaps,
		}
		args, err := serveArgs(ep, service)
		if err != nil || args == nil {
			return
		}
		if args[0] != serveCmd {
			t.Fatalf("serveArgs(%+v, %q) = %q: not a serve command", ep, service, args)
		}
		targets := 0
		for _, a := range args[1:] {
			switch {
			case a == serveBackground || a == "--tun" || serveFlag.MatchString(a):
			case serveTarget.MatchString(a):
				targets++
			default:
				t.Fatalf("serveArgs(%+v, %q) = %q: argument %q", ep, service, args, a)
			}
		}
		want := 1
		if proto == "tun" {
			want = 0
		}
		if targets != want {
			t.Fatalf(
				"serveArgs(%+v, %q) = %q: %d targets, want %d",
				ep,
				service,
				args,
				targets,
				want,
			)
		}
	})
}
