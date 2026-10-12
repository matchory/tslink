package tailscale

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/ipn"
)

// fakeTailscaled stands in for tailscaled: each start runs sleep in its
// place, with the daemon's CLI calls going to cli, or fails while fail is set.
type fakeTailscaled struct {
	cli *fakeCLI
	api *fakeLocalAPI // Every daemon's LocalAPI

	mu      sync.Mutex
	fail    error
	starts  int
	daemons []*Daemon
	onStart func(*Daemon)
}

func (f *fakeTailscaled) start(d *Daemon) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	if f.fail != nil {
		return f.fail
	}
	d.runCLI = f.cli.run
	d.lc = f.api.client()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		return err
	}
	d.mu.Lock()
	d.cmd = cmd
	d.running = true
	d.mu.Unlock()
	d.watchProcess()
	if f.onStart != nil {
		f.onStart(d)
	}
	f.daemons = append(f.daemons, d)
	return nil
}

func (f *fakeTailscaled) setFail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = err
}

func (f *fakeTailscaled) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts
}

// crash kills the tailscaled started last.
func (f *fakeTailscaled) crash(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	d := f.daemons[len(f.daemons)-1]
	f.mu.Unlock()
	d.mu.RLock()
	defer d.mu.RUnlock()
	if err := d.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
}

// newFakeSupervisor returns a supervisor with short timings whose daemons
// run fakeTailscaled, and the path of its network namespace.
func newFakeSupervisor(t *testing.T) (*DaemonSupervisor, *fakeTailscaled, string) {
	t.Helper()
	setDuration(t, &initialBackoff, time.Millisecond)
	setDuration(t, &maxBackoff, 5*time.Millisecond)
	setDuration(t, &healthCheckInterval, 5*time.Millisecond)
	setDuration(t, &maxStartupJitter, time.Nanosecond)

	netns := filepath.Join(t.TempDir(), "netns")
	if err := os.WriteFile(netns, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewDaemonSupervisor(DaemonConfig{
		EndpointID: "0123456789abcdef",
		StateDir:   t.TempDir(),
		NetNSPath:  netns,
		AuthKey:    func() (string, error) { return "tskey-auth-test", nil },
	})
	f := &fakeTailscaled{cli: &fakeCLI{}, api: newFakeLocalAPI(t)}
	s.start = f.start
	// Registered after setDuration, so it runs first: the loop reads them
	t.Cleanup(func() {
		if err := s.Stop(); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return s, f, netns
}

// hasStatus reports whether the supervisor's status is want.
func hasStatus(s *DaemonSupervisor, want SupervisorStatus) bool {
	status, _ := s.Status()
	return status == want
}

func TestSupervisorRestartsCrashedDaemon(t *testing.T) {
	s, f, _ := newFakeSupervisor(t)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if status, restarts := s.Status(); status != StatusRunning || restarts != 1 {
		t.Fatalf("after Start: %s, %d starts; want running, 1", status, restarts)
	}
	first := s.GetDaemon()

	f.crash(t)
	waitFor(
		t,
		"a restart",
		func() bool { return f.startCount() == 2 && hasStatus(s, StatusRunning) },
	)
	if _, restarts := s.Status(); restarts != 2 {
		t.Errorf("restart count = %d, want 2", restarts)
	}
	if s.GetDaemon() == first {
		t.Error("the crashed daemon is still current")
	}
}

// A failed first start fails Start, but the supervisor keeps trying: the
// endpoint stops it if it gives up on the container.
func TestSupervisorRetriesFailedFirstStart(t *testing.T) {
	s, f, _ := newFakeSupervisor(t)
	f.setFail(errors.New("socket timeout"))
	if err := s.Start(); err == nil {
		t.Fatal("Start succeeded although tailscaled did not")
	}
	waitFor(t, "a retry", func() bool { return f.startCount() >= 2 })
	f.setFail(nil)
	waitFor(t, "a successful start", func() bool { return hasStatus(s, StatusRunning) })
}

// Cancelling the context of a start in progress, as Leave does, aborts the
// start and its login at once, not after the login timeout.
func TestSupervisorStartCancelled(t *testing.T) {
	s, f, _ := newFakeSupervisor(t)
	f.api.loggedOut()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	inLogin := make(chan struct{})
	var once sync.Once
	s.start = func(d *Daemon) error {
		if err := f.start(d); err != nil {
			return err
		}
		once.Do(func() { close(inLogin) })
		// Without a key or a node key, tailscaled keeps needing a login
		if err := d.logIn(false, false); err != nil {
			if stopErr := d.Stop(); stopErr != nil {
				t.Errorf("Stop: %v", stopErr)
			}
			return err
		}
		return nil
	}

	done := make(chan error, 1)
	go func() { done <- s.StartContext(ctx) }()
	<-inLogin
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("StartContext succeeded although its context was cancelled")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling did not abort the start")
	}
	if got := f.startCount(); got != 1 {
		t.Errorf("%d starts, want 1: a cancelled start is not retried", got)
	}
}

func TestSupervisorCrashLoopCoolsDown(t *testing.T) {
	setDuration(t, &crashLoopCooldown, 200*time.Millisecond)
	s, f, _ := newFakeSupervisor(t)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < crashLoopThreshold; i++ {
		f.crash(t)
		waitFor(
			t,
			"a restart",
			func() bool { return f.startCount() == i+1 && hasStatus(s, StatusRunning) },
		)
	}
	f.crash(t)
	waitFor(t, "the crash loop", func() bool { return hasStatus(s, StatusCrashLoop) })
	if s.GetDaemon() != nil {
		t.Error("a daemon is current during the cooldown")
	}
	if got := f.startCount(); got != crashLoopThreshold {
		t.Errorf("%d starts at the crash loop, want %d", got, crashLoopThreshold)
	}

	// After the cooldown it starts again, with the crash count reset
	deadline := time.Now().Add(time.Second)
	for !hasStatus(s, StatusRunning) {
		if time.Now().After(deadline) {
			t.Fatal("did not start again after the cooldown")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if s.isCrashLoop() {
		t.Error("crash times not reset after the cooldown")
	}
}

func TestSupervisorStopsDuringWait(t *testing.T) {
	for name, long := range map[string]*time.Duration{
		"backoff":  &initialBackoff,
		"cooldown": &crashLoopCooldown,
	} {
		t.Run(name, func(t *testing.T) {
			s, f, _ := newFakeSupervisor(t)
			setDuration(t, long, time.Hour)
			if name == "cooldown" {
				s.crashTimes = make([]time.Time, crashLoopThreshold-1)
				for i := range s.crashTimes {
					s.crashTimes[i] = time.Now()
				}
			}
			if err := s.Start(); err != nil {
				t.Fatal(err)
			}
			f.crash(t)
			waitFor(t, "the crash", func() bool {
				s.mu.RLock()
				defer s.mu.RUnlock()
				return len(s.crashTimes) == crashLoopThreshold || name == "backoff" &&
					len(s.crashTimes) == 1
			})

			start := time.Now()
			if err := s.Stop(); err != nil {
				t.Fatal(err)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Errorf("Stop took %v", elapsed)
			}
			if got := f.startCount(); got != 1 {
				t.Errorf("%d starts, want 1", got)
			}
		})
	}
}

// The container is gone with its network namespace: nothing to restart.
func TestSupervisorGivesUpWithoutNetNS(t *testing.T) {
	s, f, netns := newFakeSupervisor(t)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(netns); err != nil {
		t.Fatal(err)
	}
	f.crash(t)
	waitFor(t, "the failed state", func() bool { return hasStatus(s, StatusFailed) })
	time.Sleep(20 * time.Millisecond)
	if got := f.startCount(); got != 1 {
		t.Errorf("%d starts after the namespace went, want 1", got)
	}

	s2, _, netns2 := newFakeSupervisor(t)
	if err := os.Remove(netns2); err != nil {
		t.Fatal(err)
	}
	if err := s2.Start(); err == nil {
		t.Error("Start succeeded without a network namespace")
	}
}

// A node control no longer knows, as after its device was deleted, logs in
// again with the auth key, as a new device, and again after a failed attempt.
func TestSupervisorReauthenticatesNodeNotFound(t *testing.T) {
	for name, loginErr := range map[string]string{
		"succeeds": "",
		"fails":    "register request: node not found",
	} {
		t.Run(name, func(t *testing.T) {
			s, f, _ := newFakeSupervisor(t)
			f.api.loggedIn(ipn.Prefs{ControlURL: defaultControlURL})
			f.api.startErr = loginErr
			f.onStart = func(d *Daemon) {
				d.handleLine("2026/10/08 12:00:00 control: " + nodeNotFoundLog)
			}
			if err := s.Start(); err != nil {
				t.Fatal(err)
			}
			wantLogins := 1
			if loginErr != "" {
				wantLogins = 2 // retried after a backoff
			}
			waitFor(t, "a login", func() bool { return countReauths(f.api) >= wantLogins })
			d := s.GetDaemon()
			waitFor(t, "the node found", func() bool { return d.nodeNotFound.Load() == 0 })
			for _, k := range f.api.keys() {
				if k != "tskey-auth-test" {
					t.Errorf("login got key %q, want the auth key", k)
				}
			}
		})
	}
}

// countReauths returns how often tailscaled was asked to log in as a new
// device.
func countReauths(api *fakeLocalAPI) int {
	n := 0
	for _, r := range api.paths() {
		if r == "POST /localapi/v0/login-interactive" {
			n++
		}
	}
	return n
}

func TestSupervisorBackoff(t *testing.T) {
	s := NewDaemonSupervisor(DaemonConfig{})
	t.Cleanup(s.cancel)
	want := []time.Duration{
		initialBackoff, 2 * initialBackoff, 4 * initialBackoff, 8 * initialBackoff,
	}
	for i, w := range want {
		if got := s.nextBackoff(); got != w {
			t.Errorf("backoff %d = %v, want %v", i, got, w)
		}
	}
	for range 20 {
		if got := s.nextBackoff(); got > maxBackoff {
			t.Fatalf("backoff %v exceeds %v", got, maxBackoff)
		}
	}
	if got := s.nextBackoff(); got != maxBackoff {
		t.Errorf("backoff = %v, want the cap %v", got, maxBackoff)
	}
	s.resetBackoff()
	if got := s.nextBackoff(); got != initialBackoff {
		t.Errorf("after reset: %v, want %v", got, initialBackoff)
	}
}

// A changed port or address restarts tailscaled, so it advertises the new
// one, and does not count as a crash.
func TestSupervisorRestartsOnChangedHostEndpoints(t *testing.T) {
	setDuration(t, &hostEndpointsCheckInterval, 10*time.Millisecond)
	s, f, _ := newFakeSupervisor(t)
	var mu sync.Mutex
	eps := []netip.AddrPort{netip.MustParseAddrPort("10.10.0.2:61367")}
	s.cfg.HostEndpoints = func() ([]netip.AddrPort, error) {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(eps), nil
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	eps = []netip.AddrPort{netip.MustParseAddrPort("10.10.0.2:61400")}
	mu.Unlock()
	// startDaemon only sets s.daemon once the fake's start returned and
	// recorded the count, so poll on the daemon actually advertising the new
	// endpoints, not merely on the count: between the two, GetDaemon can
	// still be nil or still be the old daemon.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := s.GetDaemon(); got != nil && slices.Equal(got.HostEndpoints(), eps) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := f.startCount(); got != 2 {
		t.Fatalf("%d starts, want 2", got)
	}
	got := s.GetDaemon()
	if got == nil {
		t.Fatal("no current daemon after the restart")
	}
	if !slices.Equal(got.HostEndpoints(), eps) {
		t.Errorf("restarted daemon advertises %v, want %v", got.HostEndpoints(), eps)
	}
	s.mu.RLock()
	crashes := len(s.crashTimes)
	s.mu.RUnlock()
	if crashes != 0 {
		t.Errorf("%d crashes recorded, want 0", crashes)
	}
}

// Unchanged endpoints, and none at all, never restart it.
func TestSupervisorKeepsDaemonWithoutHostEndpoints(t *testing.T) {
	setDuration(t, &hostEndpointsCheckInterval, 10*time.Millisecond)
	s, f, _ := newFakeSupervisor(t)
	s.cfg.HostEndpoints = func() ([]netip.AddrPort, error) { return nil, nil }
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if n := f.startCount(); n != 1 {
		t.Errorf("%d starts, want 1", n)
	}
}

// An error reading them (iptables busy, the rule missing until the next
// sync) is no change.
func TestSupervisorIgnoresHostEndpointErrors(t *testing.T) {
	setDuration(t, &hostEndpointsCheckInterval, 10*time.Millisecond)
	s, f, _ := newFakeSupervisor(t)
	var calls atomic.Int32
	s.cfg.HostEndpoints = func() ([]netip.AddrPort, error) {
		if calls.Add(1) == 1 {
			return []netip.AddrPort{netip.MustParseAddrPort("10.10.0.2:61367")}, nil
		}
		return nil, errors.New("no external port mapped")
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if n := f.startCount(); n != 1 {
		t.Errorf("%d starts, want 1", n)
	}
}

// A drain under way means the node is leaving: restarting it for changed
// host endpoints would strand DrainAndWait waiting on a daemon that no
// longer exists (GetDaemon would read nil mid-restart).
func TestSupervisorSkipsRestartWhenDrained(t *testing.T) {
	setDuration(t, &hostEndpointsCheckInterval, 10*time.Millisecond)
	s, f, _ := newFakeSupervisor(t)
	var mu sync.Mutex
	eps := []netip.AddrPort{netip.MustParseAddrPort("10.10.0.2:61367")}
	s.cfg.HostEndpoints = func() ([]netip.AddrPort, error) {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(eps), nil
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	s.cfg.gate.drained.Store(true)
	mu.Lock()
	eps = []netip.AddrPort{netip.MustParseAddrPort("10.10.0.2:61400")}
	mu.Unlock()
	time.Sleep(200 * time.Millisecond)
	if n := f.startCount(); n != 1 {
		t.Errorf("%d starts while drained, want 1", n)
	}
}

func TestSupervisorCrashLoopWindow(t *testing.T) {
	s := NewDaemonSupervisor(DaemonConfig{})
	t.Cleanup(s.cancel)
	old := time.Now().Add(-2 * crashLoopWindow)
	for range crashLoopThreshold {
		s.crashTimes = append(s.crashTimes, old)
	}
	if s.isCrashLoop() {
		t.Error("crashes outside the window count")
	}
	for i := 1; i <= crashLoopThreshold; i++ {
		s.recordCrash()
		if got, want := s.isCrashLoop(), i == crashLoopThreshold; got != want {
			t.Errorf("after %d recent crashes: crash loop = %v, want %v", i, got, want)
		}
	}
	if len(s.crashTimes) > 10 {
		t.Errorf("%d crash times kept, want at most 10", len(s.crashTimes))
	}
}
