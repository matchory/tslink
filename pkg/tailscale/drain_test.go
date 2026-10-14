package tailscale

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"tailscale.com/ipn"
)

// A line as tailscaled logs it when control fetches its Service list.
const servicesFetchLine = "2026/10/07 12:00:00 c2n: GET /vip-services received"

// setTiming gives cfg its own copy of its timing, changed by set.
func setTiming(cfg *DaemonConfig, set func(*timing)) {
	tm := cfg.times()
	set(&tm)
	cfg.timing = &tm
}

// newTestSupervisor returns a supervisor whose daemon has advertised
// svc:web, and the daemon's fake LocalAPI.
func newTestSupervisor(t *testing.T) (*DaemonSupervisor, *daemon, *fakeLocalAPI) {
	t.Helper()
	s := NewDaemonSupervisor(DaemonConfig{EndpointID: "0123456789abcdef", Service: "svc:web"})
	t.Cleanup(s.cancel)
	d, api := newTestDaemon(t, "svc:web")
	api.serviceHost(ipn.Prefs{AdvertiseServices: []string{"svc:web"}})
	d.config.gate = s.cfg.gate
	s.daemon = d
	s.cfg.gate.advertised.Store(true)
	return s, d, api
}

// waitFor fails the test unless cond holds within a second.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestHandleLineCountsServicesFetches(t *testing.T) {
	d, _ := newTestDaemon(t, "")
	n, next := d.servicesFetches()
	d.handleLine("2026/10/07 12:00:00 c2n: GET /debug/netmap received")
	if got, _ := d.servicesFetches(); got != n {
		t.Fatalf("another c2n request counted as a Service list fetch")
	}
	d.handleLine(servicesFetchLine)
	select {
	case <-next:
	default:
		t.Fatal("fetch did not wake waiters")
	}
	if got, _ := d.servicesFetches(); got != n+1 {
		t.Errorf("fetches = %d, want %d", got, n+1)
	}
}

// Leaving drains the backend, waits for control to fetch the drained Service
// list, and only then logs out.
func TestDrainAndWaitWaitsForControl(t *testing.T) {
	s, d, api := newTestSupervisor(t)
	setTiming(
		&s.cfg,
		func(tm *timing) { tm.drainAckFloor = 50 * time.Millisecond; tm.drainAckTimeout = 5 * time.Second },
	)
	loggedOut := func() bool {
		return slices.Contains(api.paths(), "POST /localapi/v0/logout")
	}
	// A fetch before the drain is not an acknowledgement of it
	d.handleLine(servicesFetchLine)

	start := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.DrainAndWait("svc:web")
		if err := d.Logout(); err != nil {
			t.Error(err)
		}
	}()
	waitFor(t, "the drain", func() bool { return ran(api.operations(), "unadvertise", "svc:web") })
	time.Sleep(100 * time.Millisecond)
	if loggedOut() {
		t.Fatal("logged out before control fetched the drain")
	}
	d.handleLine(servicesFetchLine)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("DrainAndWait did not return after control fetched the drain")
	}
	want := [][]string{{"unadvertise", "svc:web"}}
	if got := api.operations(); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("operations = %q, want %q", got, want)
	}
	if !loggedOut() {
		t.Error("not logged out after control fetched the drain")
	}
	if elapsed := time.Since(start); elapsed < s.cfg.times().drainAckFloor {
		t.Errorf("returned after %v, want at least %v", elapsed, s.cfg.times().drainAckFloor)
	}
}

// A backend drained outside tslink, as by "tailscale serve drain" from the
// host before Docker stops, is no longer advertised: leaving neither drains it
// again nor waits for a fetch that drain would never cause, and it stays
// drained.
func TestDrainAndWaitAfterExternalDrain(t *testing.T) {
	s, d, api := newTestSupervisor(t)
	setTiming(
		&s.cfg,
		func(tm *timing) { tm.drainAckFloor = 10 * time.Millisecond; tm.drainAckTimeout = 2 * time.Second },
	)
	api.serviceHost(ipn.Prefs{})

	start := time.Now()
	s.DrainAndWait("svc:web")
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("waited %v after a drain outside tslink", elapsed)
	}
	if ops := api.operations(); len(ops) != 0 {
		t.Errorf("drained again: %q", ops)
	}
	if !s.Drained() {
		t.Error("backend not marked drained")
	}
	if err := d.advertise("svc:web"); !errors.Is(err, errDrained) {
		t.Errorf("advertise = %v, want errDrained", err)
	}
}

// The floor holds even if control fetches the drain at once.
func TestDrainAndWaitFloor(t *testing.T) {
	s, d, api := newTestSupervisor(t)
	setTiming(&s.cfg, func(tm *timing) { tm.drainAckFloor = 150 * time.Millisecond })
	api.setBefore(func(context.Context, string) {
		d.handleLine(servicesFetchLine) // as fast as it gets
	})
	start := time.Now()
	s.DrainAndWait("svc:web")
	if elapsed := time.Since(start); elapsed < s.cfg.times().drainAckFloor {
		t.Errorf("returned after %v, want at least %v", elapsed, s.cfg.times().drainAckFloor)
	}
}

// Without word from control, leaving goes on after drainAckTimeout.
func TestDrainAndWaitTimesOut(t *testing.T) {
	s, _, api := newTestSupervisor(t)
	setTiming(&s.cfg, func(tm *timing) {
		tm.drainAckFloor = 10 * time.Millisecond
		tm.drainAckTimeout = 100 * time.Millisecond
	})
	start := time.Now()
	s.DrainAndWait("svc:web")
	if elapsed := time.Since(
		start,
	); elapsed < s.cfg.times().drainAckTimeout ||
		elapsed > time.Second {
		t.Errorf("returned after %v, want about %v", elapsed, s.cfg.times().drainAckTimeout)
	}
	if want := [][]string{
		{"unadvertise", "svc:web"},
	}; !slices.EqualFunc(
		api.operations(),
		want,
		slices.Equal,
	) {
		t.Errorf("operations = %q, want %q", api.operations(), want)
	}
}

// A backend drained at the stop signal is not drained again, and leaving
// waits for that drain's acknowledgement, counted from the drain.
func TestDrainAndWaitAfterStopSignalDrain(t *testing.T) {
	s, d, api := newTestSupervisor(t)
	setTiming(
		&s.cfg,
		func(tm *timing) { tm.drainAckFloor = 10 * time.Millisecond; tm.drainAckTimeout = 5 * time.Second },
	)
	if err := s.Drain("svc:web"); err != nil {
		t.Fatal(err)
	}
	d.handleLine(servicesFetchLine)

	start := time.Now()
	s.DrainAndWait("svc:web")
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("waited %v for a drain control had fetched already", elapsed)
	}
	if n := len(api.operations()); n != 1 {
		t.Errorf("ran %d operations, want the one drain: %q", n, api.operations())
	}
}

// A backend never advertised, as a replica drained while it waited for its
// certificate, gives control nothing to fetch: leaving does not wait.
func TestDrainAndWaitNeverAdvertised(t *testing.T) {
	s, _, api := newTestSupervisor(t)
	setTiming(
		&s.cfg,
		func(tm *timing) { tm.drainAckFloor = 10 * time.Millisecond; tm.drainAckTimeout = 5 * time.Second },
	)
	s.cfg.gate.advertised.Store(false)
	start := time.Now()
	s.DrainAndWait("svc:web")
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("waited %v for a backend never advertised", elapsed)
	}
	if !ran(api.operations(), "unadvertise", "svc:web") {
		t.Errorf("not drained: %q", api.operations())
	}
}

// Advertising the Service, and a node state that may advertise it from a
// previous run, mark the backend advertised.
func TestAdvertisedMarked(t *testing.T) {
	d, _ := newTestDaemon(t, "svc:web")
	d.config.Endpoints = []ServeEndpoint{{Proto: "tcp", Port: "22", Target: "22"}}
	if err := d.configureServiceBackend(false); err != nil {
		t.Fatal(err)
	}
	if d.config.gate.advertised.Load() {
		t.Error("configured unadvertised, but marked advertised")
	}
	if err := d.advertise("svc:web"); err != nil {
		t.Fatal(err)
	}
	if !d.config.gate.advertised.Load() {
		t.Error("advertise did not mark the backend advertised")
	}

	d, _ = newTestDaemon(t, "svc:web")
	d.config.Endpoints = []ServeEndpoint{{Proto: "tcp", Port: "22", Target: "22"}}
	if err := d.configureService(); err != nil {
		t.Fatal(err)
	}
	if !d.config.gate.advertised.Load() {
		t.Error("configureService did not mark the backend advertised")
	}

	d, _ = newTestDaemon(t, "svc:web")
	d.noteSurvivingState()
	if d.config.gate.advertised.Load() {
		t.Error("marked advertised without node state")
	}
	if err := os.WriteFile(
		filepath.Join(d.config.StateDir, "tailscaled.state"), []byte("{}"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	d.noteSurvivingState()
	if !d.config.gate.advertised.Load() {
		t.Error("node state from a previous run not taken as advertised")
	}
}

// With tailscaled down there is nothing to wait for, but the backend stays drained.
func TestDrainAndWaitWithoutDaemon(t *testing.T) {
	s := NewDaemonSupervisor(DaemonConfig{EndpointID: "0123456789abcdef", Service: "svc:web"})
	t.Cleanup(s.cancel)
	start := time.Now()
	s.DrainAndWait("svc:web")
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("waited %v without a daemon", elapsed)
	}
	if !s.cfg.gate.drained.Load() {
		t.Error("backend not marked drained")
	}
}

// failCert has tailscaled refuse certificate requests with msg.
func failCert(api *fakeLocalAPI, msg string) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.cert = func() error { return errors.New(msg) }
}

// newCertDaemon returns a daemon serving svc:web over HTTPS with a shared
// certificate directory, that directory and its fake LocalAPI.
func newCertDaemon(t *testing.T) (*daemon, string, *fakeLocalAPI) {
	t.Helper()
	d, api := newTestDaemon(t, "svc:web")
	d.config.Endpoints = []ServeEndpoint{{Proto: "https", Port: "443", Target: "8080"}}
	dir := filepath.Join(d.config.StateDir, certsDirName)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return d, dir, api
}

const certDomain = "api.example.ts.net"

// runAfterCert runs configureServiceAfterCert and returns a channel closed
// when it returns.
func runAfterCert(d *daemon, dir string) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.configureServiceAfterCert(dir, certDomain)
	}()
	return done
}

// A replica drained while another issues the certificate stays drained once
// the certificate appears, and stops waiting.
func TestDrainedWaitingReplicaNotAdvertised(t *testing.T) {
	d, dir, api := newCertDaemon(t)
	setTiming(&d.config, func(tm *timing) { tm.certPollInterval = 10 * time.Millisecond })
	other, err := tryLease(dir, certDomain, "other")
	if err != nil {
		t.Fatal(err)
	}
	defer other.release()

	done := runAfterCert(d, dir)
	time.Sleep(30 * time.Millisecond)
	if err := d.Drain("svc:web"); err != nil {
		t.Fatal(err)
	}
	writeCert(t, dir, certDomain, certDomain, time.Now().Add(time.Hour))
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("still waiting for the certificate after the drain")
	}
	// Not advertised, there was nothing to drain
	if ops := api.operations(); len(ops) != 0 {
		t.Errorf("operations = %q, want none", ops)
	}
}

// A drain outlives tailscaled: a daemon the supervisor starts afterwards
// neither configures nor advertises the Service.
func TestDrainSurvivesDaemonRestart(t *testing.T) {
	dir := t.TempDir()
	s := NewDaemonSupervisor(DaemonConfig{
		EndpointID: "0123456789abcdef",
		StateDir:   dir,
		SocketPath: filepath.Join(dir, "sock"),
		Service:    "svc:web",
		Endpoints:  []ServeEndpoint{{Proto: "https", Port: "443", Target: "8080"}},
	})
	t.Cleanup(s.cancel)
	if err := s.Drain("svc:web"); err == nil {
		t.Fatal("Drain without tailscaled succeeded")
	}

	d, err := newDaemon(s.cfg) // as startDaemon after a crash
	if err != nil {
		t.Fatal(err)
	}
	api := newFakeLocalAPI(t)
	api.serviceHost(ipn.Prefs{})
	d.lc = api.client()
	if err := d.configureServiceWhenCertified(); err != nil {
		t.Errorf(
			"configureServiceWhenCertified = %v, want nil: a drain must not fail the start",
			err,
		)
	}
	if err := d.configureService(); !errors.Is(err, errDrained) {
		t.Errorf("configureService = %v, want errDrained", err)
	}
	if err := d.advertise("svc:web"); !errors.Is(err, errDrained) {
		t.Errorf("advertise = %v, want errDrained", err)
	}
	if ops := api.operations(); len(ops) != 0 {
		t.Errorf("drained backend ran %q", ops)
	}
}

// A drain is not undone by a configuration in progress: should its serve
// call not end when cancelled, the drain waits for it, and the configuration
// stops before the next call.
func TestDrainAfterConfigureInProgress(t *testing.T) {
	d, api := newTestDaemon(t, "svc:web")
	api.serviceHost(ipn.Prefs{AdvertiseServices: []string{"svc:web"}})
	d.config.Endpoints = []ServeEndpoint{{Proto: "tcp", Port: "22", Target: "22"}}
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	// tailscaled finishes the change, though its client gave up
	api.setBefore(func(_ context.Context, op string) {
		if op == "serve" {
			once.Do(func() {
				close(started)
				<-release
			})
		}
	})
	configured := make(chan error)
	go func() { configured <- d.configureService() }()
	<-started
	drained := make(chan error)
	go func() { drained <- d.Drain("svc:web") }()
	time.Sleep(20 * time.Millisecond)
	close(release)
	if err := <-configured; !errors.Is(err, errDrained) {
		t.Fatalf("configureService = %v, want errDrained", err)
	}
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	ops := api.operations()
	if len(ops) == 0 || !slices.Equal(ops[len(ops)-1], []string{"unadvertise", "svc:web"}) {
		t.Errorf("operations = %q, want the drain last", ops)
	}
}

// The lease holder configures the Service unadvertised and advertises it
// only once the certificate exists.
func TestLeaseHolderAdvertisedOnlyWithCert(t *testing.T) {
	d, dir, api := newCertDaemon(t)
	setTiming(&d.config, func(tm *timing) { tm.certPollInterval = 10 * time.Millisecond })
	failCert(api, "not issued yet")

	done := runAfterCert(d, dir)
	waitFor(
		t,
		"the serve configuration",
		func() bool { return ran(api.operations(), "serve") },
	)
	time.Sleep(50 * time.Millisecond)
	if ran(api.operations(), "advertise", "svc:web") {
		t.Fatal("lease holder advertised before its certificate existed")
	}
	writeCert(t, dir, certDomain, certDomain, time.Now().Add(time.Hour))
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("still waiting after the certificate was issued")
	}
	want := [][]string{{"serve"}, {"advertise", "svc:web"}}
	if got := api.operations(); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("operations = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(dir, certDomain+".lease")); !os.IsNotExist(err) {
		t.Errorf("lease kept: %v", err)
	}
}

// Should control grant the Service's name only to hosts advertising it, the
// lease holder advertises after certDomainGrace rather than wait forever.
func TestLeaseHolderAdvertisesWhenDomainRefused(t *testing.T) {
	d, dir, api := newCertDaemon(t)
	setTiming(&d.config, func(tm *timing) {
		tm.certPollInterval = 10 * time.Millisecond
		tm.certDomainGrace = 50 * time.Millisecond
	})
	failCert(api, `invalid domain "api.example.ts.net"; must be one of []`)

	done := runAfterCert(d, dir)
	waitFor(
		t,
		"the advertisement",
		func() bool { return ran(api.operations(), "advertise", "svc:web") },
	)
	writeCert(t, dir, certDomain, certDomain, time.Now().Add(time.Hour))
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("still waiting after the certificate was issued")
	}
}

// A lease holder drained while it waits for the certificate stops waiting,
// is not advertised, and releases the lease.
func TestDrainedLeaseHolderStops(t *testing.T) {
	d, dir, api := newCertDaemon(t)
	setTiming(&d.config, func(tm *timing) { tm.certPollInterval = 10 * time.Millisecond })
	failCert(api, "not issued yet")

	done := runAfterCert(d, dir)
	waitFor(
		t,
		"the serve configuration",
		func() bool { return ran(api.operations(), "serve") },
	)
	if err := d.Drain("svc:web"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("drained lease holder still waiting for the certificate")
	}
	writeCert(t, dir, certDomain, certDomain, time.Now().Add(time.Hour))
	if ran(api.operations(), "advertise", "svc:web") {
		t.Errorf("drained lease holder advertised: %q", api.operations())
	}
	if _, err := os.Stat(filepath.Join(dir, certDomain+".lease")); !os.IsNotExist(err) {
		t.Errorf("lease kept: %v", err)
	}
}

// A drain does not wait for the serve configuration in progress: it cancels
// it, and nothing is configured or advertised after the drain.
func TestDrainCancelsConfigureInProgress(t *testing.T) {
	d, api := newTestDaemon(t, "svc:web")
	api.serviceHost(ipn.Prefs{AdvertiseServices: []string{"svc:web"}})
	d.config.Endpoints = []ServeEndpoint{
		{Proto: "tcp", Port: "22", Target: "22"},
		{Proto: "tcp", Port: "23", Target: "23"},
	}
	started := make(chan struct{})
	blockOn(api, "serve", started)
	configured := make(chan error, 1)
	go func() { configured <- d.configureService() }()
	<-started

	if err := returnsWithin(t, "Drain", func() error {
		return d.Drain("svc:web")
	}); err != nil {
		t.Fatal(err)
	}
	if err := returnsWithin(t, "the configuration", func() error {
		return <-configured
	}); !errors.Is(err, errDrained) {
		t.Errorf("configureService = %v, want errDrained", err)
	}
	want := [][]string{{"unadvertise", "svc:web"}}
	if got := api.operations(); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("operations = %q, want only the drain", got)
	}
	if err := d.advertise("svc:web"); !errors.Is(err, errDrained) {
		t.Errorf("advertise after the drain = %v, want errDrained", err)
	}
}

// A drain that cancels the advertisement in progress leaves the
// configuration drained, not failed: a failure would fail the daemon's start
// and stop it, and with it the drain.
func TestDrainCancelsAdvertiseInProgress(t *testing.T) {
	d, api := newTestDaemon(t, "svc:web")
	d.config.Endpoints = []ServeEndpoint{{Proto: "tcp", Port: "22", Target: "22"}}
	started := make(chan struct{})
	blockOn(api, "prefs", started)
	configured := make(chan error, 1)
	go func() { configured <- d.configureService() }()
	<-started

	if err := returnsWithin(t, "Drain", func() error {
		return d.Drain("svc:web")
	}); err != nil {
		t.Fatal(err)
	}
	if err := returnsWithin(t, "the configuration", func() error {
		return <-configured
	}); !errors.Is(err, errDrained) {
		t.Errorf("configureService = %v, want errDrained", err)
	}
}

// A drain while the supervisor has no daemon, as while it restarts
// tailscaled, still cancels the serve configuration of the starting daemon.
func TestSupervisorDrainCancelsConfigureWithoutDaemon(t *testing.T) {
	d, api := newTestDaemon(t, "svc:web")
	s := NewDaemonSupervisor(d.config)
	d.config.gate = s.cfg.gate
	d.config.Endpoints = []ServeEndpoint{{Proto: "tcp", Port: "22", Target: "22"}}
	started := make(chan struct{})
	blockOn(api, "serve", started)
	configured := make(chan error, 1)
	go func() { configured <- d.configureService() }()
	<-started

	if err := s.Drain("svc:web"); err == nil {
		t.Error("Drain without a daemon succeeded")
	}
	if err := returnsWithin(t, "the configuration", func() error {
		return <-configured
	}); !errors.Is(err, errDrained) {
		t.Errorf("configureService = %v, want errDrained", err)
	}
}
