package docker

import (
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/matchory/tslink/pkg/core"
	"github.com/matchory/tslink/pkg/logger"
	"github.com/matchory/tslink/pkg/tailscale"
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
		"zürich":       "z-rich",
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
		want        *tailscale.ServeEndpoint
	}{
		{
			"5432",
			"tcp:5432?proxy-protocol=2",
			&tailscale.ServeEndpoint{
				Proto:         "tcp",
				Port:          "5432",
				Target:        "5432",
				ProxyProtocol: "2",
			},
		},
		{
			"5432",
			"tcp?proxy-protocol=1",
			&tailscale.ServeEndpoint{
				Proto:         "tcp",
				Port:          "5432",
				Target:        "5432",
				ProxyProtocol: "1",
			},
		},
		{
			"443",
			"tls-terminated-tcp:8443?proxy-protocol=2",
			&tailscale.ServeEndpoint{
				Proto:         "tls-terminated-tcp",
				Port:          "443",
				Target:        "8443",
				ProxyProtocol: "2",
			},
		},
		{"5432", "tcp:5432", &tailscale.ServeEndpoint{Proto: "tcp", Port: "5432", Target: "5432"}},
		{"5432", "tcp:5432?proxy-protocol=3", nil},
		{"5432", "tcp:5432?proxy-protocol=", nil},
		{"5432", "tcp:5432?bogus=1", nil},
		{"80", "http:80?proxy-protocol=2", nil}, // TCP forwarding only
	}
	for _, tt := range tests {
		got := parseServeValue("web", core.ServeLabelPrefix+tt.port, tt.port, tt.value)
		if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
			t.Errorf("parseServeValue(%q, %q) = %+v, want %+v", tt.port, tt.value, got, tt.want)
		}
	}
}

func TestParseServeValueAcceptAppCaps(t *testing.T) {
	tests := []struct {
		port, value string
		want        *tailscale.ServeEndpoint
	}{
		{
			"80",
			"http:80?accept-app-caps=example.com/cap/a",
			&tailscale.ServeEndpoint{
				Proto:         "http",
				Port:          "80",
				Target:        "80",
				AcceptAppCaps: "example.com/cap/a",
			},
		},
		{
			"443",
			"https:8080/api?accept-app-caps=example.com/cap/a,example.com/cap/b",
			&tailscale.ServeEndpoint{
				Proto:         "https",
				Port:          "443",
				Target:        "8080",
				Path:          "/api",
				AcceptAppCaps: "example.com/cap/a,example.com/cap/b",
			},
		},
		{"80", "http:80?accept-app-caps=", nil},
		{"80", "http:80?accept-app-caps=nodomain", nil},
		{"80", "http:80?accept-app-caps=example.com/cap/a,,example.com/cap/b", nil},
		{"5432", "tcp:5432?accept-app-caps=example.com/cap/a", nil}, // HTTP only
		{"5432", "tcp:5432?proxy-protocol=2&accept-app-caps=example.com/cap/a", nil},
	}
	for _, tt := range tests {
		got := parseServeValue("web", core.ServeLabelPrefix+tt.port, tt.port, tt.value)
		if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
			t.Errorf("parseServeValue(%q, %q) = %+v, want %+v", tt.port, tt.value, got, tt.want)
		}
	}
}

func TestParseContainerInfoDirect(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{"default without a Service", map[string]string{}, true},
		{"default with a Service", map[string]string{"tslink.service": "svc:web"}, false},
		{
			"explicit with a Service",
			map[string]string{"tslink.service": "svc:web", "tslink.direct": "true"},
			true,
		},
		{"explicit off", map[string]string{"tslink.direct": "false"}, false},
	}
	for _, tt := range tests {
		if got := parseContainerInfo("web", tt.labels).Direct; got != tt.want {
			t.Errorf("%s: Direct = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// tslink.health turns on the readiness endpoint on that loopback port; a
// value that is no port leaves it off.
func TestParseContainerInfoHealth(t *testing.T) {
	for v, want := range map[string]int{
		"9002": 9002, "1": 1, "65535": 65535,
		"": 0, "0": 0, "65536": 0, "-1": 0, "http": 0, "9002x": 0,
	} {
		info := parseContainerInfo("web", map[string]string{"tslink.health": v})
		if info.HealthPort != want {
			t.Errorf("tslink.health=%q: HealthPort = %d, want %d", v, info.HealthPort, want)
		}
	}
	if info := parseContainerInfo("web", nil); info.HealthPort != 0 {
		t.Errorf("without the label: HealthPort = %d", info.HealthPort)
	}
}

// Serve endpoints come in the order of their ports, whatever the order of
// the labels, and two labels of one port in the order of the labels. A label
// whose port is not one is ignored.
func TestParseServeEndpointsInPortOrder(t *testing.T) {
	labels := map[string]string{
		"tslink.serve.0443":  "https:9090",
		"tslink.serve.https": "http:80",
		"tslink.serve.0":     "tcp",
		"tslink.serve.70000": "tcp",
		"tslink.serve.8443":  "https:8443",
		"tslink.serve.443":   "https:8080",
		"tslink.serve.80":    "http:8080",
		"tslink.serve.5432":  "tcp",
		"tslink.serve.22":    "tcp",
	}
	for range 10 {
		var got []string
		for _, ep := range parseServeEndpoints("web", labels) {
			got = append(got, ep.Port)
		}
		if want := []string{"22", "80", "0443", "443", "5432", "8443"}; !slices.Equal(got, want) {
			t.Fatalf("ports %v, want %v", got, want)
		}
	}
}

// A label under tslink's prefix that tslink does not know is logged with the
// container's name, and changes nothing.
func TestParseContainerInfoWarnsOfUnknownLabels(t *testing.T) {
	var log lockedBuffer
	logger.SetOutput(&log)
	t.Cleanup(func() { logger.SetOutput(os.Stdout) })

	info := parseContainerInfo("web", map[string]string{
		"tslink.services":   "svc:web",
		core.ServiceLabel:   "svc:web",
		"tslink.serve.443":  "https:8080",
		"com.example.label": "x",
	})
	if len(info.Services) != 1 || info.Services[0].Name != "svc:web" || len(info.Endpoints) != 1 {
		t.Errorf("parsed %+v", info)
	}
	out := log.String()
	if !strings.Contains(out, "Container web: ignoring unknown label tslink.services") {
		t.Errorf("no warning of the unknown label in %q", out)
	}
	if strings.Count(out, "unknown label") != 1 {
		t.Errorf("warned of other labels: %q", out)
	}
}

// The container labels reference lists exactly the labels tslink reads.
func TestContainerLabelsMatchReference(t *testing.T) {
	keys := []string{
		core.ServeLabelPrefix + "<port>",
		core.ServiceLabelPrefix + "<name>.serve.<port>",
	}
	for key := range containerLabels {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	b, err := os.ReadFile("../../docs/reference/container-labels.md")
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, m := range regexp.MustCompile("(?m)^\\| \\[`([^`]+)`\\]").FindAllStringSubmatch(string(b), -1) {
		listed = append(listed, m[1])
	}
	slices.Sort(listed)
	if !slices.Equal(listed, keys) {
		t.Errorf("container-labels.md lists %q, tslink reads %q", listed, keys)
	}
}

func TestParseServices(t *testing.T) {
	tests := []struct {
		name    string
		labels  map[string]string
		want    []tailscale.Service
		wantErr string
	}{
		{
			name: "two services, sorted",
			labels: map[string]string{
				"tslink.service.b.serve.443": "https:8081",
				"tslink.service.a.serve.443": "https:8080",
				"tslink.service.a.serve.80":  "http:8080",
			},
			want: []tailscale.Service{
				{Name: "svc:a", Endpoints: []tailscale.ServeEndpoint{
					{Proto: "http", Port: "80", Target: "8080"},
					{Proto: "https", Port: "443", Target: "8080"},
				}},
				{Name: "svc:b", Endpoints: []tailscale.ServeEndpoint{
					{Proto: "https", Port: "443", Target: "8081"},
				}},
			},
		},
		{
			name:   "service named serve",
			labels: map[string]string{"tslink.service.serve.serve.443": "tcp"},
			want: []tailscale.Service{{Name: "svc:serve", Endpoints: []tailscale.ServeEndpoint{
				{Proto: "tcp", Port: "443", Target: "443"},
			}}},
		},
		{
			name: "invalid endpoint skipped",
			labels: map[string]string{
				"tslink.service.a.serve.443": "https:8080",
				"tslink.service.a.serve.21":  "ftp",
			},
			want: []tailscale.Service{{Name: "svc:a", Endpoints: []tailscale.ServeEndpoint{
				{Proto: "https", Port: "443", Target: "8080"},
			}}},
		},
		{name: "none", labels: map[string]string{"tslink.serve.443": "https:80"}},
		{
			name:    "no port",
			labels:  map[string]string{"tslink.service.a.serve.": "tcp"},
			wantErr: "tslink.service.a.serve.",
		},
		{
			name:    "not a serve key",
			labels:  map[string]string{"tslink.service.a.443": "tcp"},
			wantErr: "tslink.service.a.443",
		},
		{
			name:    "upper case",
			labels:  map[string]string{"tslink.service.A.serve.443": "tcp"},
			wantErr: "tslink.service.A.serve.443",
		},
		{
			name: "one port twice",
			labels: map[string]string{
				"tslink.service.a.serve.443":  "tcp",
				"tslink.service.a.serve.0443": "tcp",
			},
			wantErr: "one port",
		},
		{
			name:    "not a port",
			labels:  map[string]string{"tslink.service.a.serve.x": "tcp"},
			wantErr: "not a port",
		},
		{
			name:    "no valid endpoint",
			labels:  map[string]string{"tslink.service.a.serve.443": "ftp"},
			wantErr: "svc:a has no valid endpoint",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseServices("web", tt.labels)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseServices error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseServices = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseContainerInfoServices(t *testing.T) {
	tests := []struct {
		name       string
		labels     map[string]string
		wantNames  []string
		wantDirect bool
		wantErr    bool
	}{
		{
			name: "legacy",
			labels: map[string]string{
				"tslink.service":   "svc:web",
				"tslink.serve.443": "https:80",
			},
			wantNames: []string{"svc:web"},
		},
		{
			name:       "direct only",
			labels:     map[string]string{"tslink.serve.443": "https:80"},
			wantDirect: true,
		},
		{
			name: "namespaced",
			labels: map[string]string{
				"tslink.service.a.serve.443": "https:80",
				"tslink.service.b.serve.443": "https:81",
			},
			wantNames: []string{"svc:a", "svc:b"},
		},
		{
			name: "namespaced with direct=false",
			labels: map[string]string{
				"tslink.service.a.serve.443": "https:80",
				"tslink.direct":              "false",
			},
			wantNames: []string{"svc:a"},
		},
		{
			name: "mixed with tslink.service",
			labels: map[string]string{
				"tslink.service":             "svc:web",
				"tslink.service.a.serve.443": "https:80",
			},
			wantErr: true,
		},
		{
			name: "mixed with tslink.serve",
			labels: map[string]string{
				"tslink.serve.80":            "http:80",
				"tslink.service.a.serve.443": "https:80",
			},
			wantErr: true,
		},
		{
			name: "namespaced with direct=true",
			labels: map[string]string{
				"tslink.service.a.serve.443": "https:80",
				"tslink.direct":              "true",
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := parseContainerInfo("web", tt.labels)
			if (info.ServiceErr != nil) != tt.wantErr {
				t.Fatalf("ServiceErr = %v, want error %v", info.ServiceErr, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			var names []string
			for _, s := range info.Services {
				names = append(names, s.Name)
			}
			if !slices.Equal(names, tt.wantNames) || info.Direct != tt.wantDirect {
				t.Errorf("Services = %q, Direct = %v; want %q, %v",
					names, info.Direct, tt.wantNames, tt.wantDirect)
			}
			if tt.name == "legacy" && len(info.Services[0].Endpoints) != 1 {
				t.Errorf("legacy Service endpoints = %v, want the tslink.serve one",
					info.Services[0].Endpoints)
			}
		})
	}
}
