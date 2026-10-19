package tailscale

import (
	"bytes"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn"

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

// approvedWithPorts is the status of a running node control approved as a
// host of svc:ai, with capabilities describing Services as caps gives them:
// "services/<id>" keys whose values are tailcfg.ServiceDetails.
func approvedWithPorts(caps string) string {
	if caps != "" {
		caps = "," + caps
	}
	return `{"BackendState":"Running",` +
		`"CurrentTailnet":{"MagicDNSSuffix":"example.ts.net"},` +
		`"Self":{"CapMap":{"service-host":[{"svc:ai":["100.65.0.1"]}]` + caps + `}}}`
}

// tcpEndpoints returns TCP endpoints forwarding each of ports to itself.
func tcpEndpoints(ports ...string) []ServeEndpoint {
	eps := make([]ServeEndpoint, 0, len(ports))
	for _, p := range ports {
		eps = append(eps, ServeEndpoint{Proto: "tcp", Port: p, Target: p})
	}
	return eps
}

// A Service host that does not serve every port of the Service's definition
// gets no traffic from control: its state is ports-uncovered, with a warning
// naming the ports, when the node's capabilities define the ports.
func TestReadinessPortsUncovered(t *testing.T) {
	const (
		web     = `"services/a1":[{"Name":"svc:ai","Ports":["tcp:80","tcp:443"]}]`
		ranged  = `"services/a1":[{"Name":"svc:ai","Ports":["tcp:8000-8002"]}]`
		other   = `"services/b2":[{"Name":"svc:other","Ports":["tcp:80","tcp:443"]}]`
		svc443  = `{"Services":{"svc:ai":{"TCP":{"443":{"HTTPS":true}}}}}`
		svcBoth = `{"Services":{"svc:ai":{"TCP":{"80":{"HTTP":true},"443":{"HTTPS":true}}}}}`
		svcTun  = `{"Services":{"svc:ai":{"Tun":true}}}`
		svcTwo  = `{"Services":{"svc:ai":{"TCP":{"8000":{},"8001":{}}}}}`
		svcAll  = `{"Services":{"svc:ai":{"TCP":{"8000":{},"8001":{},"8002":{}}}}}`
	)
	https := []ServeEndpoint{{Proto: "https", Port: "443", Target: "8081"}}
	both := []ServeEndpoint{https[0], {Proto: "http", Port: "80", Target: "8081"}}
	tun := []ServeEndpoint{{Proto: "tun", Port: "0", Target: "0"}}
	tests := []struct {
		name      string
		endpoints []ServeEndpoint
		caps      string
		serve     string
		want      Readiness
		warning   string // in the ports/svc:ai warning; empty for none
	}{
		{"one port of two", https, web, svc443, ReadyPortsUncovered, "tcp:80"},
		{"both ports", both, web, svcBoth, Ready, ""},
		{"tun covers all", tun, web, svcTun, Ready, ""},
		{
			"range partly", tcpEndpoints("8000", "8001"), ranged, svcTwo,
			ReadyPortsUncovered, "tcp:8000-8002",
		},
		{"range covered", tcpEndpoints("8000", "8001", "8002"), ranged, svcAll, Ready, ""},
		{"no capability", https, "", svc443, Ready, ""},
		{"other Service's entry", https, other, svc443, Ready, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, rec := newWarningDaemon(t)
			d.config.Services = []Service{{Name: "svc:ai", Endpoints: tt.endpoints}}
			d.backends = newBackends(d)
			d.lc = readinessAPI(t, approvedWithPorts(tt.caps), tt.serve,
				`{"AdvertiseServices":["svc:ai"]}`)
			report := d.readinessReport(t.Context())
			if report.State != tt.want || report.Services["svc:ai"] != tt.want {
				t.Errorf("readinessReport = %+v, want %q", report, tt.want)
			}
			got := rec.get("ports/svc:ai")
			if (tt.warning == "") != (got == "") || !strings.Contains(got, tt.warning) {
				t.Errorf("warning = %q, want one naming %q", got, tt.warning)
			}
		})
	}
}

// The warning about uncovered ports goes once the host serves them.
func TestPortsUncoveredWarningCleared(t *testing.T) {
	d, rec := newWarningDaemon(t)
	d.config.Services = []Service{{Name: "svc:ai", Endpoints: []ServeEndpoint{
		{Proto: "https", Port: "443", Target: "8081"},
	}}}
	d.backends = newBackends(d)
	f := newFakeLocalAPI(t)
	f.set(statusPath, approvedWithPorts(
		`"services/a1":[{"Name":"svc:ai","Ports":["tcp:80","tcp:443"]}]`))
	f.set(servePath, `{"Services":{"svc:ai":{"TCP":{"443":{"HTTPS":true}}}}}`)
	f.set(prefsPath, `{"AdvertiseServices":["svc:ai"]}`)
	d.lc = f.client()
	if got := d.readiness(t.Context()); got != ReadyPortsUncovered {
		t.Fatalf("readiness = %q, want %q", got, ReadyPortsUncovered)
	}
	if rec.get("ports/svc:ai") == "" {
		t.Fatal("no warning")
	}
	f.set(servePath,
		`{"Services":{"svc:ai":{"TCP":{"80":{"HTTP":true},"443":{"HTTPS":true}}}}}`)
	if got := d.readiness(t.Context()); got != Ready {
		t.Errorf("readiness = %q, want %q", got, Ready)
	}
	if got := rec.get("ports/svc:ai"); got != "" {
		t.Errorf("warning left: %q", got)
	}
}

// A Service waiting for its certificate keeps the node certificate-pending,
// even when another Service's ports are uncovered.
func TestReadinessCertificatePendingWinsOverPorts(t *testing.T) {
	d := newOfflineDaemon(t, "")
	d.config.Services = []Service{tcpService("svc:ai", "1"), httpsService("svc:b")}
	d.backends = newBackends(d)
	d.lc = readinessAPI(t,
		approvedWithPorts(`"services/a1":[{"Name":"svc:ai","Ports":["tcp:1","tcp:2"]}]`),
		`{"Services":{"svc:ai":{"TCP":{"1":{}}}}}`,
		`{"AdvertiseServices":["svc:ai"]}`)
	got := d.readinessReport(t.Context())
	if got.State != ReadyCertificatePending || got.Services["svc:ai"] != ReadyPortsUncovered {
		t.Errorf("readinessReport = %+v, want certificate-pending, svc:ai ports-uncovered", got)
	}
}

// newControlDaemon returns a daemon for svc:ai, serving HTTPS on 443, whose
// tailscaled is approved as a host of svc:ai and advertises the Services
// advertised, and its fake LocalAPI.
func newControlDaemon(t *testing.T, advertised ...string) (*daemon, *fakeLocalAPI) {
	t.Helper()
	d, api := newTestDaemon(t, "svc:ai", ServeEndpoint{Proto: "https", Port: "443", Target: "1"})
	api.serviceHost(ipn.Prefs{AdvertiseServices: advertised})
	api.set(statusPath, approvedWithPorts(""))
	api.set(servePath, `{"Services":{"svc:ai":{"TCP":{"443":{"HTTPS":true}}}}}`)
	return d, api
}

// A Service tslink advertised is awaiting-control until control fetches the
// node's Service list, even with the service-host capability: control was
// seen not to fetch it, and then routes nothing to the host. A fetch before
// the advertisement does not count.
func TestReadinessAwaitsControlFetch(t *testing.T) {
	d, _ := newControlDaemon(t)
	b := d.only(t)
	d.handleLine(servicesFetchLine) // before the advertisement
	if err := b.advertise("svc:ai"); err != nil {
		t.Fatal(err)
	}
	if got := d.readiness(t.Context()); got != ReadyAwaitingControl {
		t.Fatalf("readiness = %q, want %q", got, ReadyAwaitingControl)
	}
	d.handleLine(servicesFetchLine)
	if got := d.readiness(t.Context()); got != Ready {
		t.Errorf("after the fetch, readiness = %q, want %q", got, Ready)
	}
}

// Advertising anew waits for a fetch again.
func TestReadinessReadvertiseAwaitsControl(t *testing.T) {
	d, _ := newControlDaemon(t)
	b := d.only(t)
	if err := b.advertise("svc:ai"); err != nil {
		t.Fatal(err)
	}
	d.handleLine(servicesFetchLine)
	if err := b.readvertise("svc:ai", 0); err != nil {
		t.Fatal(err)
	}
	if got := d.readiness(t.Context()); got != ReadyAwaitingControl {
		t.Errorf("readiness = %q, want %q", got, ReadyAwaitingControl)
	}
}

// A Service the node advertised before tslink came to it, as from its
// state, counts as fetched: control had it.
func TestReadinessPreexistingAdvertisement(t *testing.T) {
	d, _ := newControlDaemon(t, "svc:ai")
	if err := d.only(t).advertise("svc:ai"); err != nil {
		t.Fatal(err)
	}
	if got := d.readiness(t.Context()); got != Ready {
		t.Errorf("readiness = %q, want %q", got, Ready)
	}
}

// When advertising anew never gets control to fetch, the Service stays
// awaiting-control with a warning, which a fetch clears.
func TestAwaitingControlWarning(t *testing.T) {
	old := nodeWarnings
	nodeWarnings = &warnLimiter{}
	t.Cleanup(func() { nodeWarnings = old })
	d, _ := newControlDaemon(t)
	rec := &recordedWarnings{m: map[string]string{}}
	d.config.Warn = rec.warn
	advertiseTiming(d)
	if err := d.only(t).advertise("svc:ai"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for rec.get("control/svc:ai") == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if rec.get("control/svc:ai") == "" {
		t.Fatal("no warning after the last attempt")
	}
	if got := d.readiness(t.Context()); got != ReadyAwaitingControl {
		t.Errorf("readiness = %q, want %q", got, ReadyAwaitingControl)
	}
	d.handleLine(servicesFetchLine)
	if got := rec.get("control/svc:ai"); got != "" {
		t.Errorf("warning left after the fetch: %q", got)
	}
	if got := d.readiness(t.Context()); got != Ready {
		t.Errorf("after the fetch, readiness = %q, want %q", got, Ready)
	}
}

// fakeServicesAPI is Tailscale's API as far as reading a Service goes: it
// issues a token for any client secret, and answers the definition of
// svc:ai with ports, or with code if it is not 200. It counts the reads of
// the definition.
func fakeServicesAPI(t *testing.T, code int, ports string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body string
		switch r.URL.Path {
		case "/api/v2/oauth/token":
			body = `{"access_token":"token","token_type":"Bearer","expires_in":3600}`
		case "/api/v2/tailnet/-/vip-services/svc:ai":
			reads.Add(1)
			if code != http.StatusOK {
				w.WriteHeader(code)
				body = `{"message":"insufficient scope"}`
				break
			}
			body = `{"name":"svc:ai","ports":` + ports + `}`
		default:
			http.NotFound(w, r)
			return
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &reads
}

// Without the ports in its capabilities, the Service's definition comes from
// Tailscale's API, if the credential is an OAuth client that may read it.
// "do-not-validate" ports, a refusal and other credentials leave the ports
// unknown; a refusal is logged once.
func TestDefinedPortsFromAPI(t *testing.T) {
	tests := []struct {
		name   string
		code   int
		ports  string
		secret string // the credential, the fake API's URL appended
		want   Readiness
		reads  int32
		logged bool
	}{
		{
			name: "ports compared", code: http.StatusOK, ports: `["tcp:80","tcp:443"]`,
			secret: "tskey-client-x", want: ReadyPortsUncovered, reads: 2,
		},
		{
			name: "ports covered", code: http.StatusOK, ports: `["tcp:443"]`,
			secret: "tskey-client-x", want: Ready, reads: 2,
		},
		{
			name: "do-not-validate", code: http.StatusOK, ports: `["do-not-validate"]`,
			secret: "tskey-client-x", want: Ready, reads: 2,
		},
		{
			name: "refused", code: http.StatusForbidden,
			secret: "tskey-client-x", want: Ready, reads: 2, logged: true,
		},
		{
			name: "auth key", code: http.StatusOK, ports: `["tcp:80"]`,
			secret: "tskey-auth-x", want: Ready,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var log lockedLog
			logger.SetOutput(&log)
			t.Cleanup(func() { logger.SetOutput(os.Stdout) })
			srv, reads := fakeServicesAPI(t, tt.code, tt.ports)
			d := newOfflineDaemon(t, "svc:ai",
				ServeEndpoint{Proto: "https", Port: "443", Target: "1"})
			d.config.AuthKey = func() (string, error) {
				return tt.secret + "?baseURL=" + srv.URL, nil
			}
			d.lc = readinessAPI(t, approvedWithPorts(""),
				`{"Services":{"svc:ai":{"TCP":{"443":{"HTTPS":true}}}}}`,
				`{"AdvertiseServices":["svc:ai"]}`)
			b := d.only(t)
			b.refreshDefinedPorts() // at start, and
			b.refreshDefinedPorts() // with the approval check
			if got := d.readiness(t.Context()); got != tt.want {
				t.Errorf("readiness = %q, want %q", got, tt.want)
			}
			if got := reads.Load(); got != tt.reads {
				t.Errorf("%d reads of the Service, want %d", got, tt.reads)
			}
			n := strings.Count(log.String(), "ports of svc:ai")
			if tt.logged && n != 1 || !tt.logged && n != 0 {
				t.Errorf("logged %d times, want logged = %v: %s", n, tt.logged, log.String())
			}
		})
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
