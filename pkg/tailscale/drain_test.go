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
	s := NewDaemonSupervisor(
		DaemonConfig{EndpointID: "0123456789abcdef", Services: []Service{{Name: "svc:web"}}},
	)
	t.Cleanup(s.cancel)
	d, api := newTestDaemon(t, "svc:web")
	api.serviceHost(ipn.Prefs{AdvertiseServices: []string{"svc:web"}})
	d.config.gate = s.cfg.gate
	s.daemon = d
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
		s.DrainAndWait()
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
	s.DrainAndWait()
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("waited %v after a drain outside tslink", elapsed)
	}
	if ops := api.operations(); len(ops) != 0 {
		t.Errorf("drained again: %q", ops)
	}
	if !s.Drained() {
		t.Error("backend not marked drained")
	}
	if err := d.only(t).advertise("svc:web"); !errors.Is(err, errDrained) {
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
	s.DrainAndWait()
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
	s.DrainAndWait()
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
	if err := s.Drain(); err != nil {
		t.Fatal(err)
	}
	d.handleLine(servicesFetchLine)

	start := time.Now()
	s.DrainAndWait()
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
	api.serviceHost(ipn.Prefs{})
	start := time.Now()
	s.DrainAndWait()
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("waited %v for a backend never advertised", elapsed)
	}
	if ops := api.operations(); len(ops) != 0 {
		t.Errorf("operations = %q, want none", ops)
	}
}

// With tailscaled down there is nothing to wait for, but the backend stays drained.
func TestDrainAndWaitWithoutDaemon(t *testing.T) {
	s := NewDaemonSupervisor(
		DaemonConfig{EndpointID: "0123456789abcdef", Services: []Service{{Name: "svc:web"}}},
	)
	t.Cleanup(s.cancel)
	start := time.Now()
	s.DrainAndWait()
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
	d.setEndpoints([]ServeEndpoint{{Proto: "https", Port: "443", Target: "8080"}}...)
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
		d.backends[0].configureServiceAfterCert(dir, certDomain)
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
	if err := d.Drain(); err != nil {
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
		Services: []Service{{
			Name:      "svc:web",
			Endpoints: []ServeEndpoint{{Proto: "https", Port: "443", Target: "8080"}},
		}},
	})
	t.Cleanup(s.cancel)
	if err := s.Drain(); err == nil {
		t.Fatal("Drain without tailscaled succeeded")
	}

	d, err := newDaemon(s.cfg) // as startDaemon after a crash
	if err != nil {
		t.Fatal(err)
	}
	api := newFakeLocalAPI(t)
	api.serviceHost(ipn.Prefs{})
	d.lc = api.client()
	if err := d.only(t).configureServiceWhenCertified(); err != nil {
		t.Errorf(
			"configureServiceWhenCertified = %v, want nil: a drain must not fail the start",
			err,
		)
	}
	if err := d.only(t).configureService(); !errors.Is(err, errDrained) {
		t.Errorf("configureService = %v, want errDrained", err)
	}
	if err := d.only(t).advertise("svc:web"); !errors.Is(err, errDrained) {
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
	d.setEndpoints([]ServeEndpoint{{Proto: "tcp", Port: "22", Target: "22"}}...)
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
	go func() { configured <- d.only(t).configureService() }()
	<-started
	drained := make(chan error)
	go func() { drained <- d.Drain() }()
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
	if err := d.Drain(); err != nil {
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
	d.setEndpoints([]ServeEndpoint{
		{Proto: "tcp", Port: "22", Target: "22"},
		{Proto: "tcp", Port: "23", Target: "23"},
	}...)
	started := make(chan struct{})
	blockOn(api, "serve", started)
	configured := make(chan error, 1)
	go func() { configured <- d.only(t).configureService() }()
	<-started

	if err := returnsWithin(t, "Drain", func() error {
		return d.Drain()
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
	if err := d.only(t).advertise("svc:web"); !errors.Is(err, errDrained) {
		t.Errorf("advertise after the drain = %v, want errDrained", err)
	}
}

// A drain that cancels the advertisement in progress leaves the
// configuration drained, not failed: a failure would fail the daemon's start
// and stop it, and with it the drain.
func TestDrainCancelsAdvertiseInProgress(t *testing.T) {
	d, api := newTestDaemon(t, "svc:web")
	d.setEndpoints([]ServeEndpoint{{Proto: "tcp", Port: "22", Target: "22"}}...)
	started := make(chan struct{})
	blockOn(api, "prefs", started)
	configured := make(chan error, 1)
	go func() { configured <- d.only(t).configureService() }()
	<-started

	if err := returnsWithin(t, "Drain", func() error {
		return d.Drain()
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
	d.setEndpoints([]ServeEndpoint{{Proto: "tcp", Port: "22", Target: "22"}}...)
	started := make(chan struct{})
	blockOn(api, "serve", started)
	configured := make(chan error, 1)
	go func() { configured <- d.only(t).configureService() }()
	<-started

	if err := s.Drain(); err == nil {
		t.Error("Drain without a daemon succeeded")
	}
	if err := returnsWithin(t, "the configuration", func() error {
		return <-configured
	}); !errors.Is(err, errDrained) {
		t.Errorf("configureService = %v, want errDrained", err)
	}
}

// A drain stops advertising every Service of the node in one prefs edit,
// also one its container does not name.
func TestDrainClearsEveryAdvertisedService(t *testing.T) {
	d, api := newMultiDaemon(t, tcpService("svc:a", "1"), tcpService("svc:b", "2"))
	api.serviceHost(ipn.Prefs{AdvertiseServices: []string{"svc:a", "svc:b", "svc:other"}})
	if err := d.Drain(); err != nil {
		t.Fatal(err)
	}
	ops := api.operations()
	for _, s := range []string{"svc:a", "svc:b", "svc:other"} {
		if !ran(ops, "unadvertise", s) {
			t.Errorf("operations = %q, want %s unadvertised", ops, s)
		}
	}
	edits := 0
	for _, r := range api.paths() {
		if r == "PATCH "+prefsPath {
			edits++
		}
	}
	if edits != 1 {
		t.Errorf("%d prefs edits, want 1", edits)
	}
	if g := d.config.gate; g.removal.daemon != d {
		t.Errorf("removal record = %+v, want the drain to wait for", g.removal)
	}
}

// Draining a node whose Services were all drained outside tslink records
// nothing to wait for.
func TestDrainNothingAdvertised(t *testing.T) {
	d, api := newMultiDaemon(t, tcpService("svc:a", "1"))
	if err := d.Drain(); err != nil {
		t.Fatal(err)
	}
	if ops := api.operations(); len(ops) != 0 {
		t.Errorf("operations = %q, want none", ops)
	}
	if rec := d.config.gate.removal; rec.daemon != nil {
		t.Errorf("removal record = %+v, want none", rec)
	}
}

// A drain while one backend holds the certificate lease and another waits
// for it ends both waits; neither is advertised, and the lease is released.
func TestDrainWhileLeaseHeldAndWaiting(t *testing.T) {
	d, api := newMultiDaemon(t, httpsService("svc:a"), httpsService("svc:b"))
	dir := filepath.Join(d.config.StateDir, certsDirName)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	failCert(api, "not yet")
	other, err := tryLease(dir, "b.example.ts.net", "other")
	if err != nil {
		t.Fatal(err)
	}
	defer other.release()
	if err := d.configureServing(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both waiting", func() bool {
		return d.backends[0].certWaiting.Load() && d.backends[1].certWaiting.Load()
	})
	if err := d.Drain(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "waits ended", func() bool {
		return !d.backends[0].certWaiting.Load() && !d.backends[1].certWaiting.Load()
	})
	for _, s := range []string{"svc:a", "svc:b"} {
		if ran(api.operations(), "advertise", s) {
			t.Errorf("%s advertised after the drain", s)
		}
	}
	lease, err := tryLease(dir, "a.example.ts.net", "x")
	if err != nil {
		t.Fatalf("lease of svc:a still held: %v", err)
	}
	lease.release()
}

// newPruningSupervisor returns a supervisor whose container names no
// Service, its daemon advertising svc:old from an earlier start, and the
// daemon's fake LocalAPI.
func newPruningSupervisor(t *testing.T) (*DaemonSupervisor, *daemon, *fakeLocalAPI) {
	t.Helper()
	s := NewDaemonSupervisor(DaemonConfig{EndpointID: "0123456789abcdef"})
	t.Cleanup(s.cancel)
	d, api := newTestDaemon(t, "")
	api.serviceHost(ipn.Prefs{AdvertiseServices: []string{"svc:old"}})
	d.config.gate = s.cfg.gate
	s.daemon = d
	setTiming(
		&s.cfg,
		func(tm *timing) { tm.drainAckFloor = 10 * time.Millisecond; tm.drainAckTimeout = 5 * time.Second },
	)
	return s, d, api
}

// A node whose container names no Service needs no drain, unless Services
// it no longer names were removed at start: leaving then waits for control
// to fetch that removal, so callers are not left pointed at a node gone.
func TestDrainAndWaitAfterPrune(t *testing.T) {
	s, d, api := newPruningSupervisor(t)
	if s.NeedsDrain() {
		t.Fatal("a node naming no Service needs a drain before pruning")
	}
	d.pruneServices()
	if !ran(api.operations(), "unadvertise", "svc:old") {
		t.Fatalf("operations = %q, want svc:old unadvertised", api.operations())
	}
	if !s.NeedsDrain() {
		t.Fatal("no drain needed after removing an advertised Service")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.DrainAndWait()
	}()
	select {
	case <-done:
		t.Fatal("left before control fetched the removal")
	case <-time.After(100 * time.Millisecond):
	}
	d.handleLine(servicesFetchLine)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("DrainAndWait did not return after control fetched the removal")
	}
}

// A removal control fetched already is not waited for again.
func TestDrainAndWaitAfterFetchedPrune(t *testing.T) {
	s, d, _ := newPruningSupervisor(t)
	d.pruneServices()
	d.handleLine(servicesFetchLine)
	start := time.Now()
	s.DrainAndWait()
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("waited %v for a removal control fetched already", elapsed)
	}
}

// A removal that failed leaves Services the container does not name
// advertised: the node is drained before it goes.
func TestNeedsDrainAfterFailedPrune(t *testing.T) {
	s, d, api := newPruningSupervisor(t)
	api.failOp("prefs", "boom")
	d.pruneServices()
	if !s.NeedsDrain() {
		t.Fatal("no drain needed after a failed removal")
	}
	api.failOp("prefs", "")
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.DrainAndWait()
	}()
	waitFor(t, "the drain", func() bool { return ran(api.operations(), "unadvertise", "svc:old") })
	d.handleLine(servicesFetchLine)
	<-done
}

// advertiseTiming shortens the wait for control's fetch of an advertisement.
func advertiseTiming(d *daemon) {
	setTiming(&d.config, func(tm *timing) {
		tm.advertiseAckTimeout = 50 * time.Millisecond
		tm.advertiseNudgeGap = 10 * time.Millisecond
	})
}

// After a plugin restart, control was seen not to fetch a re-advertised
// Service list at all, leaving the host unrouted: without a fetch, the
// Service is advertised anew, which makes control fetch it, until it does.
func TestAdvertiseAgainUntilControlFetches(t *testing.T) {
	d, api := newTestDaemon(t, "svc:web")
	advertiseTiming(d)
	if err := d.only(t).advertise("svc:web"); err != nil {
		t.Fatal(err)
	}
	advertised := func(n int) func() bool {
		return func() bool {
			c := 0
			for _, op := range api.operations() {
				if slices.Equal(op, []string{"advertise", "svc:web"}) {
					c++
				}
			}
			return c >= n
		}
	}
	waitFor(t, "the Service advertised anew", advertised(2))
	d.handleLine(servicesFetchLine)
	n := len(api.operations())
	time.Sleep(200 * time.Millisecond)
	if got := api.operations(); len(got) != n {
		t.Errorf("advertised again after control fetched: %q", got[n:])
	}
	want := [][]string{
		{"advertise", "svc:web"},
		{"unadvertise", "svc:web"},
		{"advertise", "svc:web"},
	}
	if got := api.operations(); !slices.EqualFunc(got[:3], want, slices.Equal) {
		t.Errorf("operations = %q, want %q first", got, want)
	}
}

// An advertisement control fetched is left alone.
func TestAdvertiseFetchedIsNotRepeated(t *testing.T) {
	d, api := newTestDaemon(t, "svc:web")
	advertiseTiming(d)
	if err := d.only(t).advertise("svc:web"); err != nil {
		t.Fatal(err)
	}
	d.handleLine(servicesFetchLine)
	time.Sleep(200 * time.Millisecond)
	want := [][]string{{"advertise", "svc:web"}}
	if got := api.operations(); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("operations = %q, want %q", got, want)
	}
}

// A drained backend is not advertised anew while control has not fetched.
func TestAdvertiseNotRepeatedOnceDrained(t *testing.T) {
	d, api := newTestDaemon(t, "svc:web")
	advertiseTiming(d)
	if err := d.only(t).advertise("svc:web"); err != nil {
		t.Fatal(err)
	}
	if err := d.Drain(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	want := [][]string{{"advertise", "svc:web"}, {"unadvertise", "svc:web"}}
	if got := api.operations(); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("operations = %q, want %q", got, want)
	}
}

// Advertising anew gives up after advertiseNudges attempts.
func TestAdvertiseAgainGivesUp(t *testing.T) {
	d, api := newTestDaemon(t, "svc:web")
	advertiseTiming(d)
	if err := d.only(t).advertise("svc:web"); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		c := 0
		for _, op := range api.operations() {
			if slices.Equal(op, []string{"advertise", "svc:web"}) {
				c++
			}
		}
		return c
	}
	want := advertiseNudges + 1
	deadline := time.Now().Add(5 * time.Second)
	for count() < want && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// Longer than another attempt would take
	time.Sleep(300 * time.Millisecond)
	if c := count(); c != want {
		t.Errorf("advertised %d times, want %d", c, want)
	}
}

// The tests below each reproduce an interleaving the drain gate model
// (test/models/drain.tla) found to violate one of its properties, I1 to I12
// in its history, with the trace the model's counterexample took.

// leaveAsync runs s.DrainAndWait and returns a channel closed once it
// returned.
func leaveAsync(s *DaemonSupervisor) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.DrainAndWait()
	}()
	return done
}

// assertWaiting fails the test if done is closed within 200 ms: Leave did not
// wait for control to fetch what it drained.
func assertWaiting(t *testing.T, done chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
		t.Fatalf("left without waiting for control to fetch %s", what)
	case <-time.After(200 * time.Millisecond):
	}
}

// assertLeft fails the test unless done is closed within a second.
func assertLeft(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("DrainAndWait did not return after control fetched the drain")
	}
}

// newPlainSupervisor returns a supervisor of svc:web whose daemon's prefs
// advertise advertised, with nothing tslink did recorded, and the daemon's
// fake LocalAPI.
func newPlainSupervisor(
	t *testing.T,
	advertised ...string,
) (*DaemonSupervisor, *daemon, *fakeLocalAPI) {
	t.Helper()
	s := NewDaemonSupervisor(
		DaemonConfig{EndpointID: "0123456789abcdef", Services: []Service{{Name: "svc:web"}}},
	)
	t.Cleanup(s.cancel)
	setTiming(
		&s.cfg,
		func(tm *timing) { tm.drainAckFloor = 10 * time.Millisecond; tm.drainAckTimeout = 5 * time.Second },
	)
	d, api := newTestDaemon(t, "svc:web")
	d.setEndpoints([]ServeEndpoint{{Proto: "tcp", Port: "22", Target: "22"}}...)
	api.serviceHost(ipn.Prefs{AdvertiseServices: advertised})
	d.config.gate = s.cfg.gate
	s.daemon = d
	return s, d, api
}

// I1: a drain while tailscaled restarts marks the gate drained and fails; the
// next tailscaled starts on node state that still advertises the Service.
// It stops advertising it as it starts (NoDaemonAdvertisesDrained, S2b).
//
// Guards: G7
func TestDrainedDaemonStartsDrained(t *testing.T) {
	s, d, api := newPlainSupervisor(t, "svc:web")
	s.daemon = nil
	if err := s.Drain(); err == nil {
		t.Fatal("drained without a daemon")
	}
	d.drainAtStart()
	if !ran(api.operations(), "unadvertise", "svc:web") {
		t.Errorf(
			"operations = %q, want svc:web unadvertised as tailscaled starts",
			api.operations(),
		)
	}
}

// I2: a fetch that control made before the drain's edit, and that tslink
// counted after the drain read the count, passed for control's fetch of the
// drained list. Only a fetch tailscaled logged after the edit counts
// (LeaveWaitsForDrain, S3).
//
// Guards: G7
func TestFetchBeforeDrainEditIsNoAck(t *testing.T) {
	s, d, api := newPlainSupervisor(t, "svc:web")
	api.setBefore(func(_ context.Context, op string) {
		if op == "prefs" {
			d.handleLine(servicesFetchLine)
		}
	})
	done := leaveAsync(s)
	waitFor(t, "the drain", func() bool { return ran(api.operations(), "unadvertise", "svc:web") })
	assertWaiting(t, done, "the drain")
	d.handleLine(servicesFetchLine)
	assertLeft(t, done)
}

// I3: a drain in the gap between readvertise's removal and its new
// advertisement found nothing advertised and did not wait, though control
// had not fetched the removal (NoHiddenDrain, S6).
//
// Guards: G7
func TestDrainInReadvertiseGapWaits(t *testing.T) {
	s, d, api := newPlainSupervisor(t, "svc:web")
	readvertised := make(chan error, 1)
	go func() { readvertised <- d.only(t).readvertise("svc:web", time.Hour) }()
	waitFor(
		t,
		"the removal",
		func() bool { return ran(api.operations(), "unadvertise", "svc:web") },
	)
	done := leaveAsync(s)
	assertWaiting(t, done, "readvertise's removal")
	d.handleLine(servicesFetchLine)
	assertLeft(t, done)
	s.cancel()
	d.cancel()
	<-readvertised
}

// I4: an advertisement the drain cancelled, whose request tailscaled carried
// out after the drain's edit, advertised the drained Service again. It is
// removed again (DrainedStaysDrained, S1), and Leave waits for control to
// fetch that removal.
//
// Guards: G7
func TestLateAdvertisementAfterDrainUndone(t *testing.T) {
	s, d, api := newPlainSupervisor(t, "svc:web")
	done := leaveAsync(s)
	waitFor(t, "the drain", func() bool { return ran(api.operations(), "unadvertise", "svc:web") })
	// The cancelled request, carried out late
	if _, err := d.lc.EditPrefs(t.Context(), &ipn.MaskedPrefs{
		AdvertiseServicesSet: true,
		AdvertiseServices:    []string{"svc:web"},
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the drain again", func() bool {
		n := 0
		for _, op := range api.operations() {
			if slices.Equal(op, []string{"unadvertise", "svc:web"}) {
				n++
			}
		}
		return n == 2
	})
	assertWaiting(t, done, "the second drain")
	d.handleLine(servicesFetchLine)
	assertLeft(t, done)
}

// I5: the drain's edit timed out while tailscaled carried it out: no drain
// was recorded, and Leave did not wait (LeaveWaitsForDrain, S3).
//
// Guards: G7
func TestTimedOutDrainEditWaitedFor(t *testing.T) {
	s, d, api := newPlainSupervisor(t, "svc:web")
	api.failAfterApplying("timeout")
	done := leaveAsync(s)
	waitFor(t, "the drain", func() bool { return ran(api.operations(), "unadvertise", "svc:web") })
	assertWaiting(t, done, "the drain")
	d.handleLine(servicesFetchLine)
	assertLeft(t, done)
}

// I6: tailscaled exited after the drain took effect, before control fetched
// it, and Leave stopped waiting. It waits for the next tailscaled, which
// starts drained, until the same deadline (LeaveWaitsForDrain, S3).
//
// Guards: G7
func TestDrainWaitFollowsRestart(t *testing.T) {
	s, d, api := newPlainSupervisor(t, "svc:web")
	done := leaveAsync(s)
	waitFor(t, "the drain", func() bool { return ran(api.operations(), "unadvertise", "svc:web") })
	// tailscaled exits; the supervisor starts the next one
	s.mu.Lock()
	s.daemon = nil
	s.mu.Unlock()
	d.cancel()
	assertWaiting(t, done, "the drain")
	next, nextAPI := newTestDaemon(t, "svc:web")
	nextAPI.serviceHost(ipn.Prefs{})
	next.config.gate = s.cfg.gate
	next.drainAtStart()
	s.mu.Lock()
	s.daemon = next
	s.mu.Unlock()
	assertWaiting(t, done, "the drain")
	next.handleLine(servicesFetchLine)
	assertLeft(t, done)
}

// I7: a Service in the node's prefs that tslink did not know it advertised,
// as one a cancelled advertisement added, was drained without a wait
// (LeaveWaitsForDrain, S3).
//
// Guards: G7
func TestDrainOfUnrecordedAdvertisementWaits(t *testing.T) {
	s, d, api := newPlainSupervisor(t, "svc:web")
	done := leaveAsync(s)
	waitFor(t, "the drain", func() bool { return ran(api.operations(), "unadvertise", "svc:web") })
	assertWaiting(t, done, "the drain")
	d.handleLine(servicesFetchLine)
	assertLeft(t, done)
}

// I8: Leave while a restarted tailscaled was not yet published found no
// daemon to drain and stopped one that advertised the Service from its state.
// It waits for that daemon to drain as it starts, and for control's fetch
// (NoLeaveWhileAdvertising, S8).
//
// Guards: G7
func TestLeaveDuringRestartWaitsForNextDaemon(t *testing.T) {
	s, d, api := newPlainSupervisor(t, "svc:web")
	s.cfg.StateDir = d.config.StateDir
	if err := os.WriteFile(
		filepath.Join(s.cfg.StateDir, "tailscaled.state"), []byte("{}"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	s.daemon = nil
	done := leaveAsync(s)
	assertWaiting(t, done, "the drain")
	d.drainAtStart()
	if !ran(api.operations(), "unadvertise", "svc:web") {
		t.Fatalf(
			"operations = %q, want svc:web unadvertised as tailscaled starts",
			api.operations(),
		)
	}
	s.mu.Lock()
	s.daemon = d
	s.mu.Unlock()
	assertWaiting(t, done, "the drain")
	d.handleLine(servicesFetchLine)
	assertLeft(t, done)
}

// I10: the certificate lease holder configured the Service unadvertised,
// removing it from the prefs, and a drain then found nothing advertised and
// did not wait (NoHiddenDrain, S6).
//
// Guards: G7
func TestLeaseHolderRemovalNotHidden(t *testing.T) {
	s, d, api := newPlainSupervisor(t, "svc:web")
	if err := d.only(t).configureServiceBackend(false); err != nil {
		t.Fatal(err)
	}
	if !ran(api.operations(), "unadvertise", "svc:web") {
		t.Fatalf("operations = %q, want svc:web unadvertised", api.operations())
	}
	done := leaveAsync(s)
	assertWaiting(t, done, "the lease holder's removal")
	d.handleLine(servicesFetchLine)
	assertLeft(t, done)
}

// I11: pruneServices' edit timed out while tailscaled carried it out: no
// removal was recorded, and Leave did not wait for control to fetch it
// (PrunedWaitedFor, S5).
//
// Guards: G7
func TestTimedOutPruneWaitedFor(t *testing.T) {
	s, d, api := newPruningSupervisor(t)
	api.failAfterApplying("timeout")
	d.pruneServices()
	api.failAfterApplying("")
	if !ran(api.operations(), "unadvertise", "svc:old") {
		t.Fatalf("operations = %q, want svc:old unadvertised", api.operations())
	}
	done := leaveAsync(s)
	assertWaiting(t, done, "the removal")
	d.handleLine(servicesFetchLine)
	assertLeft(t, done)
}

// I12: I2 for pruneServices: a fetch made before its edit, counted after it
// read the count, passed for control's fetch of the removal
// (PrunedWaitedFor, S5).
//
// Guards: G7
func TestFetchBeforePruneEditIsNoAck(t *testing.T) {
	s, d, api := newPruningSupervisor(t)
	api.setBefore(func(_ context.Context, op string) {
		if op == "prefs" {
			d.handleLine(servicesFetchLine)
		}
	})
	d.pruneServices()
	api.setBefore(nil)
	done := leaveAsync(s)
	assertWaiting(t, done, "the removal")
	d.handleLine(servicesFetchLine)
	assertLeft(t, done)
}
