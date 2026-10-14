package tailscale

import (
	"bytes"
	"maps"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"tailscale.com/client/local"

	"github.com/matchory/tslink/pkg/logger"
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
			d := newOfflineDaemon(t, tt.service, tt.endpoints...)
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

// A Service name too short to hold the svc: prefix does not panic: it waits
// for a certificate it has none of.
func TestPendingServiceShortName(t *testing.T) {
	d := &daemon{config: DaemonConfig{StateDir: t.TempDir()}}
	b := &serviceBackend{
		d:         d,
		name:      "svc",
		endpoints: []ServeEndpoint{{Proto: "https", Port: "443", Target: "8080"}},
	}
	if got := b.pendingService("example.ts.net"); got != ReadyCertificatePending {
		t.Errorf("pendingService = %v, want %v", got, ReadyCertificatePending)
	}
}

// A backend control has not approved yet is normal right after it starts:
// tslink warns only if it is still not approved after approvalGrace.
func TestApprovalWarnsOnlyIfItLasts(t *testing.T) {
	const (
		running = `{"BackendState":"Running",` +
			`"CurrentTailnet":{"MagicDNSSuffix":"example.ts.net"}}`
		approved = `{"BackendState":"Running",` +
			`"CurrentTailnet":{"MagicDNSSuffix":"example.ts.net"},` +
			`"Self":{"CapMap":{"service-host":[{"svc:ai":["100.65.0.1"]}]}}}`
		advertised = `{"AdvertiseServices":["svc:ai"]}`
		serviceTCP = `{"Services":{"svc:ai":{"TCP":{"443":{"HTTPS":true}}}}}`
	)
	for name, tt := range map[string]struct {
		status string
		warns  bool
	}{
		"still not approved": {running, true},
		"approved meanwhile": {approved, false},
	} {
		t.Run(name, func(t *testing.T) {
			var log lockedLog
			logger.SetOutput(&log)
			t.Cleanup(func() { logger.SetOutput(os.Stdout) })
			d := newOfflineDaemon(t, "svc:ai")
			d.setEndpoints([]ServeEndpoint{{Proto: "https", Port: "443", Target: "8081"}}...)
			d.config.CertsDir = t.TempDir()
			setTiming(&d.config, func(tm *timing) { tm.approvalGrace = 50 * time.Millisecond })
			d.lc = readinessAPI(t, tt.status, serviceTCP, advertised)

			d.only(t).checkApprovalLater() // as advertising the Service does
			if strings.Contains(log.String(), "level=WARN") {
				t.Fatalf("warned at once: %s", log.String())
			}
			d.wg.Wait()
			if got := strings.Contains(log.String(), "pending admin approval"); got != tt.warns {
				t.Errorf("warned = %v, want %v: %s", got, tt.warns, log.String())
			}
		})
	}
}

// lockedLog is a log buffer that goroutines may write concurrently.
type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedLog) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The node is as ready as its least ready Service, and the report gives
// each Service's state, from one read of each kind.
func TestReadinessPerService(t *testing.T) {
	const status = `{"BackendState":"Running",` +
		`"CurrentTailnet":{"MagicDNSSuffix":"example.ts.net"},` +
		`"Self":{"CapMap":{"service-host":[{"svc:a":["100.65.0.1"]}]}}}`
	const serve = `{"Services":{"svc:a":{"TCP":{"1":{}}},"svc:b":{"TCP":{"2":{}}}}}`
	tests := []struct {
		name  string
		prefs string
		want  ReadinessReport
	}{
		{
			name:  "b not approved",
			prefs: `{"AdvertiseServices":["svc:a","svc:b"]}`,
			want: ReadinessReport{State: ReadyAwaitingApproval, Services: map[string]Readiness{
				"svc:a": Ready, "svc:b": ReadyAwaitingApproval,
			}},
		},
		{
			name:  "b not advertised",
			prefs: `{"AdvertiseServices":["svc:a"]}`,
			want: ReadinessReport{State: ReadyServingPending, Services: map[string]Readiness{
				"svc:a": Ready, "svc:b": ReadyServingPending,
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newOfflineDaemon(t, "")
			d.config.Services = []Service{tcpService("svc:a", "1"), tcpService("svc:b", "2")}
			d.backends = newBackends(d)
			f := newFakeLocalAPI(t)
			f.set(statusPath, status)
			f.set(servePath, serve)
			f.set(prefsPath, tt.prefs)
			d.lc = f.client()
			got := d.readinessReport(t.Context())
			if got.State != tt.want.State || !maps.Equal(got.Services, tt.want.Services) {
				t.Errorf("readinessReport = %+v, want %+v", got, tt.want)
			}
			if n := countPrefix(f.paths(), "GET "); n != 3 {
				t.Errorf("%d reads (%q), want 3: status, serve config, prefs", n, f.paths())
			}
		})
	}
}

// A Service waiting for its certificate makes the node certificate-pending,
// whatever the others are.
func TestReadinessCertificatePendingWins(t *testing.T) {
	d := newOfflineDaemon(t, "")
	d.config.Services = []Service{tcpService("svc:a", "1"), httpsService("svc:b")}
	d.backends = newBackends(d)
	d.lc = readinessAPI(t,
		`{"BackendState":"Running","CurrentTailnet":{"MagicDNSSuffix":"example.ts.net"},`+
			`"Self":{"CapMap":{"service-host":[{"svc:a":["100.65.0.1"]}]}}}`,
		`{"Services":{"svc:a":{"TCP":{"1":{}}}}}`,
		`{"AdvertiseServices":["svc:a"]}`)
	got := d.readinessReport(t.Context())
	if got.State != ReadyCertificatePending || got.Services["svc:a"] != Ready {
		t.Errorf("readinessReport = %+v, want certificate-pending with svc:a ready", got)
	}
}

func countPrefix(list []string, prefix string) int {
	n := 0
	for _, s := range list {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}
