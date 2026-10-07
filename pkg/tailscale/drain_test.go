package tailscale

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// A line as tailscaled logs it when control fetches its Service list.
const servicesFetchLine = "2026/10/07 12:00:00 c2n: GET /vip-services received"

// setDuration sets *p to v for the test.
func setDuration(t *testing.T, p *time.Duration, v time.Duration) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// newTestSupervisor returns a supervisor whose daemon runs the fake CLI and
// has advertised svc:web.
func newTestSupervisor(t *testing.T, cli *fakeCLI) (*DaemonSupervisor, *Daemon) {
	t.Helper()
	s := NewDaemonSupervisor(DaemonConfig{EndpointID: "0123456789abcdef", Service: "svc:web"})
	t.Cleanup(s.cancel)
	d := newTestDaemon(t, cli, "svc:web")
	d.config.gate = s.cfg.gate
	s.daemon = d
	s.cfg.gate.advertised.Store(true)
	return s, d
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

// ran reports whether calls contain the command args.
func ran(calls [][]string, args ...string) bool {
	return slices.ContainsFunc(calls, func(c []string) bool { return slices.Equal(c, args) })
}

func TestHandleLineCountsServicesFetches(t *testing.T) {
	d := newTestDaemon(t, &fakeCLI{}, "")
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
	setDuration(t, &drainAckFloor, 50*time.Millisecond)
	setDuration(t, &drainAckTimeout, 5*time.Second)
	cli := &fakeCLI{}
	s, d := newTestSupervisor(t, cli)
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
	waitFor(t, "the drain", func() bool { return ran(cli.snapshot(), "serve", "drain", "svc:web") })
	time.Sleep(100 * time.Millisecond)
	if ran(cli.snapshot(), "logout") {
		t.Fatal("logged out before control fetched the drain")
	}
	d.handleLine(servicesFetchLine)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("DrainAndWait did not return after control fetched the drain")
	}
	want := [][]string{{"serve", "drain", "svc:web"}, {"logout"}}
	if got := cli.snapshot(); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("calls = %q, want %q", got, want)
	}
	if elapsed := time.Since(start); elapsed < drainAckFloor {
		t.Errorf("returned after %v, want at least %v", elapsed, drainAckFloor)
	}
}

// The floor holds even if control fetches the drain at once.
func TestDrainAndWaitFloor(t *testing.T) {
	setDuration(t, &drainAckFloor, 150*time.Millisecond)
	cli := &fakeCLI{}
	s, d := newTestSupervisor(t, cli)
	cli.out = ""
	d.runCLI = func(ctx context.Context, c cliCall) (cliOutput, error) {
		out, err := cli.run(ctx, c)
		d.handleLine(servicesFetchLine) // as fast as it gets
		return out, err
	}
	start := time.Now()
	s.DrainAndWait("svc:web")
	if elapsed := time.Since(start); elapsed < drainAckFloor {
		t.Errorf("returned after %v, want at least %v", elapsed, drainAckFloor)
	}
}

// Without word from control, leaving goes on after drainAckTimeout.
func TestDrainAndWaitTimesOut(t *testing.T) {
	setDuration(t, &drainAckFloor, 10*time.Millisecond)
	setDuration(t, &drainAckTimeout, 100*time.Millisecond)
	cli := &fakeCLI{}
	s, _ := newTestSupervisor(t, cli)
	start := time.Now()
	s.DrainAndWait("svc:web")
	if elapsed := time.Since(start); elapsed < drainAckTimeout || elapsed > time.Second {
		t.Errorf("returned after %v, want about %v", elapsed, drainAckTimeout)
	}
	if want := [][]string{
		{"serve", "drain", "svc:web"},
	}; !slices.EqualFunc(
		cli.snapshot(),
		want,
		slices.Equal,
	) {
		t.Errorf("calls = %q, want %q", cli.snapshot(), want)
	}
}

// A backend drained at the stop signal is not drained again, and leaving
// waits for that drain's acknowledgement, counted from the drain.
func TestDrainAndWaitAfterStopSignalDrain(t *testing.T) {
	setDuration(t, &drainAckFloor, 10*time.Millisecond)
	setDuration(t, &drainAckTimeout, 5*time.Second)
	cli := &fakeCLI{}
	s, d := newTestSupervisor(t, cli)
	if err := s.Drain("svc:web"); err != nil {
		t.Fatal(err)
	}
	d.handleLine(servicesFetchLine)

	start := time.Now()
	s.DrainAndWait("svc:web")
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("waited %v for a drain control had fetched already", elapsed)
	}
	if n := len(cli.snapshot()); n != 1 {
		t.Errorf("ran %d commands, want the one drain: %q", n, cli.snapshot())
	}
}

// A backend never advertised, as a replica drained while it waited for its
// certificate, gives control nothing to fetch: leaving does not wait.
func TestDrainAndWaitNeverAdvertised(t *testing.T) {
	setDuration(t, &drainAckFloor, 10*time.Millisecond)
	setDuration(t, &drainAckTimeout, 5*time.Second)
	cli := &fakeCLI{}
	s, _ := newTestSupervisor(t, cli)
	s.cfg.gate.advertised.Store(false)
	start := time.Now()
	s.DrainAndWait("svc:web")
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("waited %v for a backend never advertised", elapsed)
	}
	if !ran(cli.snapshot(), "serve", "drain", "svc:web") {
		t.Errorf("not drained: %q", cli.snapshot())
	}
}

// Advertising the Service, and a node state that may advertise it from a
// previous run, mark the backend advertised.
func TestAdvertisedMarked(t *testing.T) {
	cli := &fakeCLI{}
	d := newTestDaemon(t, cli, "svc:web")
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

	d = newTestDaemon(t, cli, "svc:web")
	d.config.Endpoints = []ServeEndpoint{{Proto: "tcp", Port: "22", Target: "22"}}
	if err := d.configureService(); err != nil {
		t.Fatal(err)
	}
	if !d.config.gate.advertised.Load() {
		t.Error("configureService did not mark the backend advertised")
	}

	d = newTestDaemon(t, cli, "svc:web")
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

// failingCert returns a tailscale CLI that fails "tailscale cert" with msg on
// its standard error, as tailscale does, and passes other calls to cli.
func failingCert(cli *fakeCLI, msg string) func(context.Context, cliCall) (cliOutput, error) {
	return func(ctx context.Context, c cliCall) (cliOutput, error) {
		if slices.Contains(c.args, "cert") {
			return cliOutput{stderr: msg + "\n"}, errors.New("exit status 1")
		}
		return cli.run(ctx, c)
	}
}

// newCertDaemon returns a daemon serving svc:web over HTTPS with a shared
// certificate directory, and that directory.
func newCertDaemon(t *testing.T, cli *fakeCLI) (*Daemon, string) {
	t.Helper()
	d := newTestDaemon(t, cli, "svc:web")
	d.config.Endpoints = []ServeEndpoint{{Proto: "https", Port: "443", Target: "8080"}}
	dir := filepath.Join(d.config.StateDir, certsDirName)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return d, dir
}

const certDomain = "api.example.ts.net"

// runAfterCert runs configureServiceAfterCert and returns a channel closed
// when it returns.
func runAfterCert(d *Daemon, dir string) <-chan struct{} {
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
	setDuration(t, &certPollInterval, 10*time.Millisecond)
	cli := &fakeCLI{}
	d, dir := newCertDaemon(t, cli)
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
	if want := [][]string{
		{"serve", "drain", "svc:web"},
	}; !slices.EqualFunc(
		cli.snapshot(),
		want,
		slices.Equal,
	) {
		t.Errorf("calls = %q, want only the drain", cli.snapshot())
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

	d, err := NewDaemon(s.cfg) // as startDaemon after a crash
	if err != nil {
		t.Fatal(err)
	}
	cli := &fakeCLI{}
	d.runCLI = cli.run
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
	if calls := cli.snapshot(); len(calls) != 0 {
		t.Errorf("drained backend ran %q", calls)
	}
}

// A drain waits for a configuration in progress, so it is not undone by it.
func TestDrainAfterConfigureInProgress(t *testing.T) {
	cli := &fakeCLI{}
	d := newTestDaemon(t, cli, "svc:web")
	d.config.Endpoints = []ServeEndpoint{{Proto: "tcp", Port: "22", Target: "22"}}
	started, release := make(chan struct{}), make(chan struct{})
	d.runCLI = func(ctx context.Context, c cliCall) (cliOutput, error) {
		if slices.Contains(c.args, "--tcp=22") {
			close(started)
			<-release
		}
		return cli.run(ctx, c)
	}
	configured := make(chan error)
	go func() { configured <- d.configureService() }()
	<-started
	drained := make(chan error)
	go func() { drained <- d.Drain("svc:web") }()
	time.Sleep(20 * time.Millisecond)
	close(release)
	if err := <-configured; err != nil {
		t.Fatal(err)
	}
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	calls := cli.snapshot()
	if last := calls[len(calls)-1]; !slices.Equal(last, []string{"serve", "drain", "svc:web"}) {
		t.Errorf("calls = %q, want the drain last", calls)
	}
}

// The lease holder configures the Service unadvertised and advertises it
// only once the certificate exists.
func TestLeaseHolderAdvertisedOnlyWithCert(t *testing.T) {
	setDuration(t, &certPollInterval, 10*time.Millisecond)
	cli := &fakeCLI{}
	d, dir := newCertDaemon(t, cli)
	d.runCLI = failingCert(cli, "not issued yet")

	done := runAfterCert(d, dir)
	waitFor(
		t,
		"the serve configuration",
		func() bool { return ran(cli.snapshot(), "serve", "drain", "svc:web") },
	)
	time.Sleep(50 * time.Millisecond)
	if ran(cli.snapshot(), "serve", "advertise", "svc:web") {
		t.Fatal("lease holder advertised before its certificate existed")
	}
	writeCert(t, dir, certDomain, certDomain, time.Now().Add(time.Hour))
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("still waiting after the certificate was issued")
	}
	want := [][]string{
		{"serve", "--service=svc:web", "--https=443", "127.0.0.1:8080"},
		{"serve", "drain", "svc:web"}, // "serve --service" advertises by itself
		{"serve", "advertise", "svc:web"},
	}
	if got := cli.snapshot(); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("calls =\n  %q\nwant\n  %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(dir, certDomain+".lease")); !os.IsNotExist(err) {
		t.Errorf("lease kept: %v", err)
	}
}

// Should control grant the Service's name only to hosts advertising it, the
// lease holder advertises after certDomainGrace rather than wait forever.
func TestLeaseHolderAdvertisesWhenDomainRefused(t *testing.T) {
	setDuration(t, &certPollInterval, 10*time.Millisecond)
	setDuration(t, &certDomainGrace, 50*time.Millisecond)
	cli := &fakeCLI{}
	d, dir := newCertDaemon(t, cli)
	d.runCLI = failingCert(cli, `invalid domain "api.example.ts.net"; must be one of []`)

	done := runAfterCert(d, dir)
	waitFor(
		t,
		"the advertisement",
		func() bool { return ran(cli.snapshot(), "serve", "advertise", "svc:web") },
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
	setDuration(t, &certPollInterval, 10*time.Millisecond)
	cli := &fakeCLI{}
	d, dir := newCertDaemon(t, cli)
	d.runCLI = failingCert(cli, "not issued yet")

	done := runAfterCert(d, dir)
	waitFor(
		t,
		"the serve configuration",
		func() bool { return ran(cli.snapshot(), "serve", "drain", "svc:web") },
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
	if ran(cli.snapshot(), "serve", "advertise", "svc:web") {
		t.Errorf("drained lease holder advertised: %q", cli.snapshot())
	}
	if _, err := os.Stat(filepath.Join(dir, certDomain+".lease")); !os.IsNotExist(err) {
		t.Errorf("lease kept: %v", err)
	}
}
