package core

import (
	"strings"
	"testing"
)

func TestValidHostname(t *testing.T) {
	for _, h := range []string{"web", "my-app_1", "billing_api.1.rhqn99qc3kawxeu8hgljewtff", "A1"} {
		if !validHostname.MatchString(h) {
			t.Errorf("%q should be valid", h)
		}
	}
	for _, h := range []string{"", ".", "..", "../other", "a/b", "/abs", "-lead", ".hidden", "a b", "a\x00b"} {
		if validHostname.MatchString(h) {
			t.Errorf("%q should be invalid", h)
		}
	}
}

func TestStateDirFor(t *testing.T) {
	tests := []struct {
		stack, hostname, want string
	}{
		{"", "web", "/data/by-hostname/web"},
		{"billing", "billing_api.1.abc", "/data/by-stack/billing/billing_api.1.abc"},
	}
	for _, tt := range tests {
		got, err := stateDirFor("/data", tt.stack, tt.hostname)
		if err != nil || got != tt.want {
			t.Errorf("stateDirFor(%q, %q) = %q, %v; want %q", tt.stack, tt.hostname, got, err, tt.want)
		}
	}
	for _, tt := range [][2]string{{"", "../x"}, {"..", "web"}, {"a/b", "web"}, {"billing", ""}} {
		if got, err := stateDirFor("/data", tt[0], tt[1]); err == nil {
			t.Errorf("stateDirFor(%q, %q) = %q, want error", tt[0], tt[1], got)
		}
	}
}

func TestStartTailscaleRejectsForeignStack(t *testing.T) {
	e := &Endpoint{
		ID:         "0123456789abcdef",
		Network:    &Network{ID: "net", AuthKey: "tskey-test"},
		SandboxKey: "/var/run/docker/netns/test",
		DataDir:    t.TempDir(),
	}
	for _, stack := range []string{"other", ""} {
		info := &ContainerInfo{Hostname: "web", Stack: stack, NetworkStack: "billing"}
		err := e.StartTailscale(info)
		if err == nil || !strings.Contains(err.Error(), "does not match network stack") {
			t.Errorf("stack %q: got %v, want stack mismatch error", stack, err)
		}
	}
}

func TestSocketPathForFitsSunPath(t *testing.T) {
	// Longest valid stack and hostname must not push the socket past the
	// 108-byte sun_path limit, inside the plugin or as seen from the host.
	id := strings.Repeat("a", 64)
	for _, dataDir := range []string{"/data", "/var/lib/docker-plugins/tailscale"} {
		p := socketPathFor(dataDir, id)
		if len(p) >= 108 {
			t.Errorf("socket path %q is %d bytes, want < 108", p, len(p))
		}
	}
	if got := socketPathFor("/data", id); got != "/data/sock/aaaaaaaaaaaa.sock" {
		t.Errorf("socketPathFor = %q", got)
	}
}
