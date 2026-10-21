package tailscale

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/tailcfg"
)

const testSocket = "/run/test.sock"

// newTestDaemon returns a daemon for service, serving endpoints on it, whose
// tailscaled is a fake LocalAPI, a running, tagged node of the tailnet
// example.ts.net, and that fake. An empty service makes a daemon without one.
func newTestDaemon(
	t *testing.T,
	service string,
	endpoints ...ServeEndpoint,
) (*daemon, *fakeLocalAPI) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	api := newFakeLocalAPI(t)
	api.serviceHost(ipn.Prefs{})
	d := &daemon{
		config:     DaemonConfig{EndpointID: "test", StateDir: t.TempDir(), gate: &serviceGate{}},
		socketPath: api.socket,
		lc:         api.client(),
		ctx:        ctx,
		cancel:     cancel,
	}
	if service != "" {
		d.config.Services = []Service{{Name: service, Endpoints: endpoints}}
	}
	d.backends = newBackends(d)
	api.log = d.handleLine
	// Registered after t.TempDir, so it runs first: the daemon's goroutines
	// write certificate leases into the state directory until they end
	t.Cleanup(func() {
		cancel()
		d.wg.Wait()
	})
	return d, api
}

// newOfflineDaemon is newTestDaemon without a tailscaled: nothing listens on
// its socket.
func newOfflineDaemon(t *testing.T, service string, endpoints ...ServeEndpoint) *daemon {
	t.Helper()
	d, _ := newTestDaemon(t, service, endpoints...)
	d.socketPath = testSocket
	d.lc = &local.Client{Socket: testSocket, UseSocketOnly: true}
	return d
}

// only returns d's single Service backend.
func (d *daemon) only(t *testing.T) *serviceBackend {
	t.Helper()
	if len(d.backends) != 1 {
		t.Fatalf("daemon has %d backends, want 1", len(d.backends))
	}
	return d.backends[0]
}

// ran reports whether ops contain the operation op.
func ran(ops [][]string, op ...string) bool {
	return slices.ContainsFunc(ops, func(c []string) bool { return slices.Equal(c, op) })
}

// serveJSON returns sc as JSON.
func serveJSON(t *testing.T, sc *ipn.ServeConfig) string {
	t.Helper()
	b, err := json.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestApplyServeEndpoint(t *testing.T) {
	const svc = "svc:web"
	const host = "node.example.ts.net"
	tests := []struct {
		name    string
		service tailcfg.ServiceName
		ep      ServeEndpoint
		want    string
		wantErr string
	}{
		// Direct machine serve
		{
			name: "direct http",
			ep:   ServeEndpoint{Proto: "http", Port: "80", Target: "8080"},
			want: `{"TCP":{"80":{"HTTP":true}},"Web":{"node.example.ts.net:80":` +
				`{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8080"}}}}}`,
		},
		{
			name: "direct https with path and app capabilities",
			ep: ServeEndpoint{
				Proto: "https", Port: "443", Target: "8080", Path: "/api",
				AcceptAppCaps: "example.com/cap/a,example.com/cap/b",
			},
			want: `{"TCP":{"443":{"HTTPS":true}},"Web":{"node.example.ts.net:443":` +
				`{"Handlers":{"/api":{"Proxy":"http://127.0.0.1:8080",` +
				`"AcceptAppCaps":["example.com/cap/a","example.com/cap/b"]}}}}}`,
		},
		{
			name: "direct tcp with proxy protocol",
			ep:   ServeEndpoint{Proto: "tcp", Port: "5432", Target: "5432", ProxyProtocol: "2"},
			want: `{"TCP":{"5432":{"TCPForward":"127.0.0.1:5432","ProxyProtocol":2}}}`,
		},
		{
			name: "direct tls-terminated-tcp",
			ep:   ServeEndpoint{Proto: "tls-terminated-tcp", Port: "443", Target: "8080"},
			want: `{"TCP":{"443":{"TCPForward":"127.0.0.1:8080",` +
				`"TerminateTLS":"node.example.ts.net"}}}`,
		},
		{
			name: "direct tun is skipped",
			ep:   ServeEndpoint{Proto: "tun", Port: "0", Target: "0"},
			want: `{}`,
		},

		// Tailscale Service backend
		{
			name:    "service https",
			service: svc,
			ep:      ServeEndpoint{Proto: "https", Port: "443", Target: "8080"},
			want: `{"Services":{"svc:web":{"TCP":{"443":{"HTTPS":true}},` +
				`"Web":{"web.example.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8080"}}}}}}}`,
		},
		{
			name:    "service tcp with proxy protocol",
			service: svc,
			ep:      ServeEndpoint{Proto: "tcp", Port: "5432", Target: "5432", ProxyProtocol: "1"},
			want: `{"Services":{"svc:web":{"TCP":{"5432":` +
				`{"TCPForward":"127.0.0.1:5432","ProxyProtocol":1}}}}}`,
		},
		{
			name:    "service tls-terminated-tcp",
			service: svc,
			ep:      ServeEndpoint{Proto: "tls-terminated-tcp", Port: "443", Target: "8080"},
			want: `{"Services":{"svc:web":{"TCP":{"443":` +
				`{"TCPForward":"127.0.0.1:8080","TerminateTLS":"web.example.ts.net"}}}}}`,
		},
		{
			name:    "service tun ignores path and options",
			service: svc,
			ep: ServeEndpoint{
				Proto: "tun", Port: "1", Target: "2", Path: "/api",
				ProxyProtocol: "2", AcceptAppCaps: "example.com/cap/a",
			},
			want: `{"Services":{"svc:web":{"Tun":true}}}`,
		},

		// Invalid input
		{
			name:    "non-numeric port",
			ep:      ServeEndpoint{Proto: "http", Port: "http", Target: "8080"},
			wantErr: `invalid external port "http": must be numeric`,
		},
		{
			name:    "port out of range",
			ep:      ServeEndpoint{Proto: "http", Port: "65536", Target: "8080"},
			wantErr: `invalid external port "65536": must be numeric`,
		},
		{
			name:    "missing target",
			ep:      ServeEndpoint{Proto: "tcp", Port: "5432"},
			wantErr: `invalid target port "": must be numeric`,
		},
		{
			name:    "signed target",
			ep:      ServeEndpoint{Proto: "tcp", Port: "5432", Target: "+5432"},
			wantErr: `invalid target port "+5432": must be numeric`,
		},
		{
			name:    "tun with invalid port",
			ep:      ServeEndpoint{Proto: "tun", Port: "x", Target: "0"},
			wantErr: `invalid external port "x": must be numeric`,
		},
		{
			name:    "service non-numeric target",
			service: svc,
			ep:      ServeEndpoint{Proto: "https", Port: "443", Target: "web:8080"},
			wantErr: `invalid target port "web:8080": must be numeric`,
		},
		{
			name:    "unclean path",
			ep:      ServeEndpoint{Proto: "https", Port: "443", Target: "8080", Path: "/a/../b"},
			wantErr: `invalid mount point "/a/../b"`,
		},
		{
			name:    "unsupported protocol",
			service: svc,
			ep:      ServeEndpoint{Proto: "HTTP", Port: "80", Target: "8080"},
			wantErr: "unsupported protocol: HTTP",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := new(ipn.ServeConfig)
			err := applyServeEndpoint(sc, tt.ep, tt.service, host, "example.ts.net")
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := serveJSON(t, sc); got != tt.want {
				t.Errorf("config =\n  %s\nwant\n  %s", got, tt.want)
			}
		})
	}
}

// A port serves either web or TCP, as with "tailscale serve".
func TestApplyServeEndpointConflicts(t *testing.T) {
	sc := new(ipn.ServeConfig)
	web := ServeEndpoint{Proto: "https", Port: "443", Target: "8080"}
	tcp := ServeEndpoint{Proto: "tcp", Port: "443", Target: "8443"}
	if err := applyServeEndpoint(sc, web, "svc:web", "", "example.ts.net"); err != nil {
		t.Fatal(err)
	}
	if err := applyServeEndpoint(sc, tcp, "svc:web", "", "example.ts.net"); err == nil {
		t.Error("TCP on a port serving web succeeded")
	}
	sc = new(ipn.ServeConfig)
	if err := applyServeEndpoint(sc, tcp, "", "node.example.ts.net", ""); err != nil {
		t.Fatal(err)
	}
	if err := applyServeEndpoint(sc, web, "", "node.example.ts.net", ""); err == nil {
		t.Error("web on a port serving TCP succeeded")
	}
}

func TestConfigureServiceAdvertises(t *testing.T) {
	d, api := newTestDaemon(t, "svc:web")
	d.setEndpoints([]ServeEndpoint{
		{Proto: "https", Port: "443", Target: "8080"},
		{Proto: "tcp", Port: "5432", Target: "5432"},
	}...)
	if err := d.only(t).configureService(); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"serve"}, {"advertise", "svc:web"}}
	if got := api.operations(); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("operations = %q, want %q", got, want)
	}
	svc := api.serveConfig().Services["svc:web"]
	if svc == nil || svc.TCP[443] == nil || svc.TCP[5432] == nil {
		t.Errorf("serve config = %s, want both endpoints", serveJSON(t, api.serveConfig()))
	}
}

// Configuring the Service keeps what the serve config holds, as direct serve,
// and sends it back with the version it read.
func TestConfigureServiceKeepsServeConfig(t *testing.T) {
	d, api := newTestDaemon(t, "svc:web")
	d.setEndpoints([]ServeEndpoint{{Proto: "tcp", Port: "22", Target: "22"}}...)
	d.config.Direct = true
	if err := d.configureDirectServe(); err != nil {
		t.Fatal(err)
	}
	if err := d.only(t).configureService(); err != nil {
		t.Fatal(err)
	}
	sc := api.serveConfig()
	if sc.TCP[22] == nil || sc.Services["svc:web"] == nil {
		t.Errorf("serve config = %s, want direct serve and the Service", serveJSON(t, sc))
	}

	// A serve config changed meanwhile is not overwritten
	api.setBefore(func(context.Context, string) {
		api.mu.Lock()
		api.etag++
		api.mu.Unlock()
	})
	if err := d.only(t).configureService(); err == nil {
		t.Error("overwrote a serve config changed since it was read")
	}
}

// A node configured without advertising, as the certificate lease holder,
// is not advertised, even if its prefs advertised the Service from before.
func TestConfigureServiceUnadvertised(t *testing.T) {
	d, api := newTestDaemon(t, "svc:web")
	api.serviceHost(ipn.Prefs{AdvertiseServices: []string{"svc:other", "svc:web"}})
	d.setEndpoints([]ServeEndpoint{{Proto: "tcp", Port: "22", Target: "22"}}...)
	if err := d.only(t).configureServiceBackend(false); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"serve"}, {"unadvertise", "svc:web"}}
	if got := api.operations(); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("operations = %q, want %q", got, want)
	}
	if err := d.only(t).configureServiceBackend(false); err != nil {
		t.Fatal(err)
	}
	if got := api.operations(); ran(got[2:], "unadvertise", "svc:web") {
		t.Errorf("unadvertised again: %q", got)
	}
}

func TestConfigureServiceSkips(t *testing.T) {
	d, api := newTestDaemon(t, "")
	d.setEndpoints([]ServeEndpoint{{Proto: "https", Port: "443", Target: "8080"}}...)
	if len(d.backends) != 0 {
		t.Fatalf("daemon without a Service has %d backends", len(d.backends))
	}
	d, api2 := newTestDaemon(t, "svc:web")
	if err := d.only(t).configureService(); err != nil {
		t.Fatal(err)
	}
	if ops := append(api.operations(), api2.operations()...); len(ops) != 0 {
		t.Errorf("ran %q without a service or endpoints", ops)
	}
}

func TestConfigureServiceErrors(t *testing.T) {
	// A node without tags cannot host a Service
	d, api := newTestDaemon(t, "svc:web")
	d.setEndpoints([]ServeEndpoint{{Proto: "https", Port: "443", Target: "8080"}}...)
	api.mu.Lock()
	api.tags = nil
	api.mu.Unlock()
	if err := d.only(t).configureService(); !errors.Is(err, errUntaggedServiceHost) {
		t.Errorf("untagged node: error = %v, want errUntaggedServiceHost", err)
	}
	if ops := api.operations(); len(ops) != 0 {
		t.Errorf("untagged node ran %q", ops)
	}

	// tailscaled refuses the config
	d, api = newTestDaemon(t, "svc:web")
	d.setEndpoints([]ServeEndpoint{{Proto: "https", Port: "443", Target: "8080"}}...)
	api.failOp("serve", "boom")
	err := d.only(t).configureService()
	if err == nil || !strings.Contains(err.Error(), "failed to set the serve config") ||
		!strings.Contains(err.Error(), "boom") {
		t.Errorf("refused config: error = %v", err)
	}
	if ops := api.operations(); len(ops) != 0 {
		t.Errorf("advertised after a refused config: %q", ops)
	}

	// An invalid endpoint sets nothing
	d, api = newTestDaemon(t, "svc:web")
	d.setEndpoints([]ServeEndpoint{{Proto: "udp", Port: "53", Target: "53"}}...)
	err = d.only(t).configureService()
	if err == nil ||
		err.Error() != "failed to configure endpoint 0 (udp:53): unsupported protocol: udp" {
		t.Errorf("invalid endpoint: error = %v", err)
	}
	if ops := api.operations(); len(ops) != 0 {
		t.Errorf("invalid endpoint ran %q", ops)
	}
}

func TestConfigureServiceAdvertiseError(t *testing.T) {
	d, api := newTestDaemon(t, "svc:web")
	d.setEndpoints([]ServeEndpoint{{Proto: "tcp", Port: "22", Target: "22"}}...)
	api.failOp("prefs", "not allowed")
	err := d.only(t).configureService()
	if err == nil || !strings.Contains(err.Error(), "failed to advertise svc:web") ||
		!strings.Contains(err.Error(), "not allowed") {
		t.Errorf("error = %v", err)
	}
}

func TestDrain(t *testing.T) {
	d, api := newTestDaemon(t, "")
	api.serviceHost(ipn.Prefs{AdvertiseServices: []string{"svc:web"}})
	if err := d.Drain(); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"unadvertise", "svc:web"}}
	if got := api.operations(); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("operations = %q, want %q", got, want)
	}
	// Once drained, draining again, as Leave does after the stop signal's drain, does nothing
	if err := d.Drain(); err != nil || len(api.operations()) != 1 {
		t.Errorf("second Drain = %v, operations %q; want none", err, api.operations())
	}

	// A failed drain still keeps the backend from being advertised, and is retried
	d, api = newTestDaemon(t, "")
	api.serviceHost(ipn.Prefs{AdvertiseServices: []string{"svc:web"}})
	api.failOp("prefs", "no such service")
	err := d.Drain()
	if err == nil || !strings.Contains(err.Error(), `failed to unadvertise ["svc:web"]`) ||
		!strings.Contains(err.Error(), "no such service") {
		t.Errorf("error = %v", err)
	}
	if !d.isDrained() {
		t.Error("failed drain left the backend advertisable")
	}
	api.failOp("prefs", "")
	if err := d.Drain(); err != nil || !ran(api.operations(), "unadvertise", "svc:web") {
		t.Errorf(
			"drain after a failure = %v, operations %q; want it run again",
			err,
			api.operations(),
		)
	}
}

// Direct serve configures the node's own name, and needs it.
func TestConfigureDirectServe(t *testing.T) {
	d, api := newTestDaemon(t, "")
	d.setEndpoints([]ServeEndpoint{{Proto: "https", Port: "443", Target: "8080"}}...)
	if err := d.configureDirectServe(); err != nil {
		t.Fatal(err)
	}
	if web := api.serveConfig().Web["node.example.ts.net:443"]; web == nil {
		t.Errorf("serve config = %s, want the node's name", serveJSON(t, api.serveConfig()))
	}

	d, api = newTestDaemon(t, "")
	d.setEndpoints([]ServeEndpoint{{Proto: "https", Port: "443", Target: "8080"}}...)
	api.mu.Lock()
	api.dnsName = ""
	api.mu.Unlock()
	if err := d.configureDirectServe(); err == nil {
		t.Error("direct serve without a DNS name succeeded")
	}
}

func TestServeDebugLogKeepsEarlierAttempts(t *testing.T) {
	d, api := newTestDaemon(t, "svc:web")
	d.setEndpoints([]ServeEndpoint{{Proto: "tcp", Port: "22", Target: "22"}}...)
	if err := d.only(t).configureService(); err != nil {
		t.Fatal(err)
	}
	api.failOp("serve", "boom")
	if err := d.only(t).configureService(); err == nil {
		t.Fatal("second attempt succeeded, want an error")
	}
	log, err := os.ReadFile(filepath.Join(d.config.StateDir, "serve-debug.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"SUCCESS service: {", `"TCPForward":"127.0.0.1:22"`, "FAILED service: ", "boom"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("log missing %q:\n%s", want, log)
		}
	}
}

func TestServeDebugLogIsBounded(t *testing.T) {
	old := serveDebugLogMaxBytes
	serveDebugLogMaxBytes = 300
	t.Cleanup(func() { serveDebugLogMaxBytes = old })

	d, _ := newTestDaemon(t, "svc:web")
	d.setEndpoints([]ServeEndpoint{{Proto: "tcp", Port: "22", Target: "22"}}...)
	for range 10 {
		if err := d.only(t).configureService(); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(d.config.StateDir, "serve-debug.log")
	for _, p := range []string{path, path + ".1"} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st.Size() > serveDebugLogMaxBytes {
			t.Errorf("%s is %d bytes, want at most %d", p, st.Size(), serveDebugLogMaxBytes)
		}
	}
}

// blockOn blocks the first operation op of api until its request's context
// is done, as tailscaled would leave a slow request to its client, or 5
// seconds passed. It closes started when that operation begins.
func blockOn(api *fakeLocalAPI, op string, started chan<- struct{}) {
	var once sync.Once
	api.setBefore(func(ctx context.Context, o string) {
		if o != op {
			return
		}
		block := false
		once.Do(func() { block = true; close(started) })
		if !block {
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	})
}

// returnsWithin fails the test unless fn returns within a second, and
// returns its error.
func returnsWithin(t *testing.T, what string, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(time.Second):
		t.Fatalf("%s did not return within a second", what)
		return nil
	}
}

// The serve configuration's LocalAPI calls end when the daemon stops, rather
// than keep a stopping endpoint waiting for their timeout.
func TestServeCallsEndWithDaemon(t *testing.T) {
	for _, tc := range []struct {
		name, service string
		configure     func(*daemon) error
	}{
		{"service", "svc:web", func(d *daemon) error { return d.backends[0].configureService() }},
		{"direct", "", (*daemon).configureDirectServe},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, api := newTestDaemon(t, tc.service)
			d.setEndpoints([]ServeEndpoint{{Proto: "tcp", Port: "22", Target: "22"}}...)
			started := make(chan struct{})
			blockOn(api, "serve", started)
			go func() {
				<-started
				d.cancel()
			}()
			err := returnsWithin(t, "the serve configuration",
				func() error { return tc.configure(d) })
			if err == nil {
				t.Error("cancelled serve configuration succeeded")
			}
		})
	}
}

// Drain's LocalAPI call ends when the daemon stops.
func TestDrainCallEndsWithDaemon(t *testing.T) {
	d, api := newTestDaemon(t, "svc:web")
	api.serviceHost(ipn.Prefs{AdvertiseServices: []string{"svc:web"}})
	started := make(chan struct{})
	blockOn(api, "prefs", started)
	go func() {
		<-started
		d.cancel()
	}()
	err := returnsWithin(t, "Drain", func() error { return d.Drain() })
	if err == nil {
		t.Error("cancelled drain succeeded")
	}
}

// Each Service of the config gets its own backend, in the config's order,
// and a daemon started later builds its own.
func TestBackendsPerDaemon(t *testing.T) {
	cfg := DaemonConfig{
		EndpointID: "test",
		StateDir:   t.TempDir(),
		Services: []Service{
			{Name: "svc:a", Endpoints: []ServeEndpoint{{Proto: "tcp", Port: "1", Target: "1"}}},
			{Name: "svc:b", Endpoints: []ServeEndpoint{{Proto: "tcp", Port: "2", Target: "2"}}},
		},
	}
	d1, err := newDaemon(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d1.cancel)
	d2, err := newDaemon(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d2.cancel)
	if len(d1.backends) != 2 || d1.backends[0].name != "svc:a" || d1.backends[1].name != "svc:b" {
		t.Fatalf("backends = %v, want svc:a, svc:b", d1.backends)
	}
	if d1.backends[0] == d2.backends[0] || d1.backends[0].d != d1 {
		t.Error("daemons share backends")
	}
	if d1.config.gate == nil {
		t.Error("no gate")
	}
}

// setEndpoints gives d endpoints as a container with tslink.serve.<port>
// labels has them: for direct serve, and for its Service, if it has one.
func (d *daemon) setEndpoints(endpoints ...ServeEndpoint) {
	d.config.Endpoints = endpoints
	for i := range d.config.Services {
		d.config.Services[i].Endpoints = endpoints
	}
	d.backends = newBackends(d)
}

// newMultiDaemon is newTestDaemon with several Services and short waits.
func newMultiDaemon(t *testing.T, services ...Service) (*daemon, *fakeLocalAPI) {
	t.Helper()
	d, api := newTestDaemon(t, "")
	d.config.Services = services
	d.backends = newBackends(d)
	setTiming(&d.config, func(tm *timing) {
		tm.controlSync = 0
		tm.certPollInterval = 10 * time.Millisecond
	})
	return d, api
}

func tcpService(name, port string) Service {
	return Service{Name: name, Endpoints: []ServeEndpoint{{Proto: "tcp", Port: port, Target: port}}}
}

func httpsService(name string) Service {
	return Service{
		Name:      name,
		Endpoints: []ServeEndpoint{{Proto: "https", Port: "443", Target: "1"}},
	}
}

// A Service whose configuration fails does not fail the start or hold back
// the others; it is retried until it succeeds, with a status warning until
// then.
func TestFailingBackendRetriedInPlace(t *testing.T) {
	d, api := newMultiDaemon(t, tcpService("svc:a", "1"), tcpService("svc:b", "2"))
	var mu sync.Mutex
	warnings := map[string]string{}
	d.config.Warn = func(key, message string) {
		mu.Lock()
		defer mu.Unlock()
		warnings[key] = message
	}
	warning := func() string {
		mu.Lock()
		defer mu.Unlock()
		return warnings["service/svc:b"]
	}
	api.failService("svc:b", "boom")
	if err := d.configureServing(); err != nil {
		t.Fatalf("configureServing = %v, want nil", err)
	}
	ops := api.operations()
	if !ran(ops, "advertise", "svc:a") || ran(ops, "advertise", "svc:b") {
		t.Fatalf("operations = %q, want svc:a advertised only", ops)
	}
	if w := warning(); !strings.Contains(w, "boom") {
		t.Errorf("warning = %q, want the failure", w)
	}
	api.failService("svc:b", "")
	waitFor(
		t,
		"svc:b advertised",
		func() bool { return ran(api.operations(), "advertise", "svc:b") },
	)
	waitFor(t, "the warning cleared", func() bool { return warning() == "" })
}

// Checks every Service needs still fail the start, before any is configured.
func TestSharedServiceChecksFailStart(t *testing.T) {
	t.Run("untagged", func(t *testing.T) {
		d, api := newMultiDaemon(t, tcpService("svc:a", "1"))
		api.mu.Lock()
		api.tags = nil
		api.mu.Unlock()
		if err := d.configureServing(); !errors.Is(err, errUntaggedServiceHost) {
			t.Errorf("configureServing = %v, want %v", err, errUntaggedServiceHost)
		}
		if ops := api.operations(); len(ops) != 0 {
			t.Errorf("operations = %q, want none", ops)
		}
	})
	t.Run("HTTPS without MagicDNS", func(t *testing.T) {
		d, api := newMultiDaemon(t, httpsService("svc:a"))
		api.mu.Lock()
		api.suffix = ""
		api.mu.Unlock()
		if err := d.configureServing(); !errors.Is(err, errNoMagicDNS) {
			t.Errorf("configureServing = %v, want %v", err, errNoMagicDNS)
		}
	})
}

// Reauthenticate configures the Services again, but a backend still waiting
// for its certificate is not started a second time.
func TestReauthenticateStartsNoSecondWait(t *testing.T) {
	d, api := newMultiDaemon(t, httpsService("svc:a"))
	if err := os.Mkdir(filepath.Join(d.config.StateDir, certsDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	failCert(api, "not yet")
	if err := d.configureServing(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "cert wait", d.only(t).certWaiting.Load)
	if err := d.configureServing(); err != nil {
		t.Fatal(err)
	}
	if !d.only(t).certWaiting.Load() {
		t.Error("wait ended")
	}
}

// A lease holder whose domain control refuses for certDomainGrace advertises
// its own Service only; another Service waiting for its lease stays
// unadvertised.
func TestDomainFallbackAdvertisesOwnServiceOnly(t *testing.T) {
	d, api := newMultiDaemon(t, httpsService("svc:a"), httpsService("svc:b"))
	setTiming(&d.config, func(tm *timing) { tm.certDomainGrace = 50 * time.Millisecond })
	dir := filepath.Join(d.config.StateDir, certsDirName)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	other, err := tryLease(dir, "b.example.ts.net", "other")
	if err != nil {
		t.Fatal(err)
	}
	defer other.release()
	failCert(api, `invalid domain "a.example.ts.net"; must be one of []`)
	if err := d.configureServing(); err != nil {
		t.Fatal(err)
	}
	waitFor(
		t,
		"svc:a advertised",
		func() bool { return ran(api.operations(), "advertise", "svc:a") },
	)
	if ran(api.operations(), "advertise", "svc:b") {
		t.Error("svc:b advertised while another replica holds its lease")
	}
}

// Services in the node's state the container does not name are removed from
// the serve config and the prefs; the rest is kept.
func TestPruneStaleServices(t *testing.T) {
	d, api := newMultiDaemon(t, tcpService("svc:a", "1"))
	api.mu.Lock()
	api.serve = &ipn.ServeConfig{
		TCP: map[uint16]*ipn.TCPPortHandler{80: {TCPForward: "127.0.0.1:80"}},
		Services: map[tailcfg.ServiceName]*ipn.ServiceConfig{
			"svc:a":   {TCP: map[uint16]*ipn.TCPPortHandler{1: {TCPForward: "127.0.0.1:1"}}},
			"svc:old": {TCP: map[uint16]*ipn.TCPPortHandler{2: {TCPForward: "127.0.0.1:2"}}},
		},
	}
	api.prefs.AdvertiseServices = []string{"svc:a", "svc:old"}
	api.mu.Unlock()

	d.pruneServices()

	sc := api.serveConfig()
	if sc.Services["svc:old"] != nil || sc.Services["svc:a"] == nil || sc.TCP[80] == nil {
		t.Errorf("serve config = %s, want svc:old gone, the rest kept", serveJSON(t, sc))
	}
	ops := api.operations()
	if !ran(ops, "unadvertise", "svc:old") || ran(ops, "unadvertise", "svc:a") {
		t.Errorf("operations = %q, want svc:old unadvertised only", ops)
	}
}

// With nothing stale, pruning writes nothing.
func TestPruneNothingStale(t *testing.T) {
	d, api := newMultiDaemon(t, tcpService("svc:a", "1"))
	d.pruneServices()
	if ops := api.operations(); len(ops) != 0 {
		t.Errorf("operations = %q, want none", ops)
	}
}

// A drained node is not pruned: the gate refuses every change, and the
// drain removed every advertised Service already.
func TestPruneSkippedWhenDrained(t *testing.T) {
	d, api := newMultiDaemon(t, tcpService("svc:a", "1"))
	api.mu.Lock()
	api.prefs.AdvertiseServices = []string{"svc:old"}
	api.mu.Unlock()
	d.config.gate.markDrained()
	d.pruneServices()
	if ops := api.operations(); len(ops) != 0 {
		t.Errorf("operations = %q, want none", ops)
	}
}
