package tailscale

import (
	"testing"

	"tailscale.com/client/local"
)

// readinessAPI answers the queries readiness makes as tailscaled would, with
// the given status, serve config and prefs; an empty one fails.
func readinessAPI(t *testing.T, status, serve, prefs string) *local.Client {
	t.Helper()
	f := newFakeLocalAPI(t)
	f.set(statusPath, status)
	f.set(servePath, serve)
	f.set(prefsPath, prefs)
	return f.client()
}

func TestReadiness(t *testing.T) {
	const (
		running = `{"BackendState":"Running",` +
			`"CurrentTailnet":{"MagicDNSSuffix":"example.ts.net"}}`
		approved = `{"BackendState":"Running",` +
			`"CurrentTailnet":{"MagicDNSSuffix":"example.ts.net"},` +
			`"Self":{"CapMap":{"service-host":[{"svc:other":["100.65.0.2"]},{"svc:ai":["100.65.0.1"]}]}}}`
		advertised = `{"AdvertiseServices":["svc:ai"]}`
		serviceTCP = `{"Services":{"svc:ai":{"TCP":{"443":{"HTTPS":true}}}}}`
	)
	https := []ServeEndpoint{{Proto: "https", Port: "443", Target: "8081"}}
	http := []ServeEndpoint{{Proto: "http", Port: "80", Target: "8081"}}
	tests := []struct {
		name                 string
		service              string
		endpoints            []ServeEndpoint
		direct               bool
		status, serve, prefs string
		drained              bool
		want                 Readiness
	}{
		{name: "not running", direct: true, want: ReadyStarting},
		{
			name:   "needs login",
			direct: true,
			status: `{"BackendState":"NeedsLogin"}`,
			want:   ReadyLoggedOut,
		},
		{
			name:   "starting",
			direct: true,
			status: `{"BackendState":"Starting"}`,
			want:   ReadyStarting,
		},
		{name: "direct, nothing to serve", direct: true, status: running, want: Ready},
		{
			name:      "direct serve missing",
			direct:    true,
			endpoints: https,
			status:    running,
			serve:     `{}`,
			want:      ReadyServingPending,
		},
		{
			name: "direct serve applied", direct: true, endpoints: https, status: running,
			serve: `{"TCP":{"443":{"HTTPS":true}}}`, want: Ready,
		},
		{
			name:      "service not configured, https, no certificate",
			service:   "svc:ai",
			endpoints: https,
			status:    running,
			serve:     `{}`,
			want:      ReadyCertificatePending,
		},
		{
			name: "service not configured, http", service: "svc:ai", endpoints: http,
			status: running, serve: `{}`, want: ReadyServingPending,
		},
		{
			name:      "service configured, unadvertised, no certificate",
			service:   "svc:ai",
			endpoints: https,
			status:    running,
			serve:     serviceTCP,
			prefs:     `{}`,
			want:      ReadyCertificatePending,
		},
		{
			name: "service advertised, not approved", service: "svc:ai", endpoints: https,
			status: running, serve: serviceTCP, prefs: advertised, want: ReadyAwaitingApproval,
		},
		{
			name: "service approved", service: "svc:ai", endpoints: https,
			status: approved, serve: serviceTCP, prefs: advertised, want: Ready,
		},
		{
			name:      "service approved, direct serve too but missing",
			service:   "svc:ai",
			endpoints: https,
			direct:    true,
			status:    approved,
			serve:     serviceTCP,
			prefs:     advertised,
			want:      ReadyServingPending,
		},
		{
			name:      "drained",
			service:   "svc:ai",
			endpoints: https,
			status:    approved,
			serve:     serviceTCP,
			prefs:     advertised,
			drained:   true,
			want:      ReadyDrained,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newTestDaemon(t, &fakeCLI{}, tt.service)
			d.config.Endpoints, d.config.Direct = tt.endpoints, tt.direct
			d.config.CertsDir = t.TempDir()
			d.lc = readinessAPI(t, tt.status, tt.serve, tt.prefs)
			d.config.gate.drained.Store(tt.drained)
			if got := d.readiness(t.Context()); got != tt.want {
				t.Errorf("readiness = %q, want %q", got, tt.want)
			}
		})
	}
}
