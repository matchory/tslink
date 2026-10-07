package docker

import (
	"testing"

	"github.com/aaomidi/tslink/pkg/core"
)

func TestParseContainerInfoStack(t *testing.T) {
	info := parseContainerInfo("billing_api.1.abc", map[string]string{core.StackLabel: "billing"})
	if info.Stack != "billing" {
		t.Errorf("Stack = %q, want billing", info.Stack)
	}
	if info := parseContainerInfo("web", nil); info.Stack != "" {
		t.Errorf("Stack = %q, want empty", info.Stack)
	}
}

func TestHostnameFromName(t *testing.T) {
	tests := map[string]string{
		"web":      "web",
		"my-app-1": "my-app-1",
		"billing_api.1.rhqn99qc3kawxeu8hgljewtff": "billing-api-1-rhqn99qc3kawxeu8hgljewtff",
		"_lead.trail_": "lead-trail",
		"café":         "caf",
		"a_very_long_stack_name_for_billing_api.1.rhqn99qc3kawxeu8hgljewtff": "a-very-long-stack-name-for-billing-api-1-rhqn99qc3kawxeu8hgljew",
	}
	for in, want := range tests {
		if got := hostnameFromName(in); got != want {
			t.Errorf("hostnameFromName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseContainerInfoHostnameLabelKept(t *testing.T) {
	info := parseContainerInfo("stack_api.1.abc", map[string]string{"tslink.hostname": "my-api"})
	if info.Hostname != "my-api" {
		t.Errorf("Hostname = %q, want my-api", info.Hostname)
	}
}

func TestIsStopSignal(t *testing.T) {
	tests := []struct {
		signal, stopSignal string
		want               bool
	}{
		{"15", "", true},         // docker stop, default SIGTERM
		{"9", "", true},          // stop timeout reached
		{"15", "SIGTERM", true},  // explicit stop signal
		{"3", "SIGQUIT", true},   // custom stop signal by name
		{"3", "QUIT", true},      // without SIG prefix
		{"2", "2", true},         // by number
		{"1", "", false},         // SIGHUP reload
		{"15", "SIGQUIT", false}, // not this container's stop signal
		{"", "", false},
	}
	for _, tt := range tests {
		if got := isStopSignal(tt.signal, tt.stopSignal); got != tt.want {
			t.Errorf("isStopSignal(%q, %q) = %v, want %v", tt.signal, tt.stopSignal, got, tt.want)
		}
	}
}

func TestParseServeValueProxyProtocol(t *testing.T) {
	tests := []struct {
		port, value string
		want        *core.ServeEndpoint
	}{
		{"5432", "tcp:5432?proxy-protocol=2", &core.ServeEndpoint{Proto: "tcp", Port: "5432", Target: "5432", ProxyProtocol: "2"}},
		{"5432", "tcp?proxy-protocol=1", &core.ServeEndpoint{Proto: "tcp", Port: "5432", Target: "5432", ProxyProtocol: "1"}},
		{"443", "tls-terminated-tcp:8443?proxy-protocol=2", &core.ServeEndpoint{Proto: "tls-terminated-tcp", Port: "443", Target: "8443", ProxyProtocol: "2"}},
		{"5432", "tcp:5432", &core.ServeEndpoint{Proto: "tcp", Port: "5432", Target: "5432"}},
		{"5432", "tcp:5432?proxy-protocol=3", nil},
		{"5432", "tcp:5432?proxy-protocol=", nil},
		{"5432", "tcp:5432?bogus=1", nil},
		{"80", "http:80?proxy-protocol=2", nil}, // TCP forwarding only
	}
	for _, tt := range tests {
		got := parseServeValue(tt.port, tt.value)
		if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
			t.Errorf("parseServeValue(%q, %q) = %+v, want %+v", tt.port, tt.value, got, tt.want)
		}
	}
}
