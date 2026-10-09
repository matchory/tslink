package core

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matchory/tslink/pkg/tailscale"
)

func shortRetries(t *testing.T) {
	t.Helper()
	initial, maxDelay := startRetryInitial, startRetryMax
	startRetryInitial, startRetryMax = time.Millisecond, 4*time.Millisecond
	t.Cleanup(func() { startRetryInitial, startRetryMax = initial, maxDelay })
}

func TestRunTailscaleRetriesTransientFailures(t *testing.T) {
	shortRetries(t)
	var calls atomic.Int32
	e := &Endpoint{ID: "0123456789abcdef", DataDir: t.TempDir()}
	e.startFn = func(*ContainerInfo) error {
		if calls.Add(1) < 4 {
			return errors.New("control plane unreachable")
		}
		return nil
	}
	e.RunTailscale(&ContainerInfo{Hostname: "web"})
	if got := calls.Load(); got != 4 {
		t.Errorf("start called %d times, want 4", got)
	}
}

func TestRunTailscaleStopsOnPermanentError(t *testing.T) {
	shortRetries(t)
	var calls atomic.Int32
	e := &Endpoint{ID: "0123456789abcdef", DataDir: t.TempDir()}
	e.startFn = func(*ContainerInfo) error {
		calls.Add(1)
		return permanentError{errors.New("stack mismatch")}
	}
	e.RunTailscale(&ContainerInfo{Hostname: "web"})
	if got := calls.Load(); got != 1 {
		t.Errorf("start called %d times, want 1", got)
	}
}

func TestRunTailscaleStopsRetryingAfterLeave(t *testing.T) {
	shortRetries(t)
	var calls atomic.Int32
	e := &Endpoint{ID: "0123456789abcdef", DataDir: t.TempDir()}
	e.startFn = func(*ContainerInfo) error {
		if calls.Add(1) == 2 {
			if err := e.Leave(); err != nil {
				t.Errorf("Leave: %v", err)
			}
		}
		return errors.New("control plane unreachable")
	}
	done := make(chan struct{})
	go func() {
		e.RunTailscale(&ContainerInfo{Hostname: "web"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunTailscale kept retrying after Leave")
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("start called %d times, want 2", got)
	}
}

// Once Leave has begun, a late trigger (a connect event, the watchdog) must
// not start Tailscale again: nothing would stop it before DeleteEndpoint.
func TestRunTailscaleRefusesAfterLeave(t *testing.T) {
	shortRetries(t)
	var calls atomic.Int32
	e := &Endpoint{ID: "0123456789abcdef", DataDir: t.TempDir()}
	e.startFn = func(*ContainerInfo) error {
		calls.Add(1)
		return nil
	}
	if err := e.Leave(); err != nil {
		t.Fatalf("Leave: %v", err)
	}
	e.RunTailscale(&ContainerInfo{Hostname: "web"})
	if got := calls.Load(); got != 0 {
		t.Errorf("start called %d times after Leave, want 0", got)
	}
}

// A start queued before DeleteEndpoint runs after Stop: it must start
// nothing, since nothing would stop it again.
func TestRunTailscaleRefusesAfterStop(t *testing.T) {
	shortRetries(t)
	var calls atomic.Int32
	e := &Endpoint{ID: "0123456789abcdef", DataDir: t.TempDir()}
	e.startFn = func(*ContainerInfo) error {
		calls.Add(1)
		return nil
	}
	if err := e.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	e.RunTailscale(&ContainerInfo{Hostname: "web"})
	if got := calls.Load(); got != 0 {
		t.Errorf("start called %d times after Stop, want 0", got)
	}
}

// Leave while a start is in flight, after it claimed and marked its state
// directory: the start goes on, recreates the directory as NewDaemon does and
// may have registered the node before it fails. Once it returns, the run
// logs the ephemeral node out and removes its state; until then, the
// directory stays claimed.
func TestRunTailscaleCleansUpStartCancelledByLeave(t *testing.T) {
	shortRetries(t)
	loggedOut := stubLogout(t, nil)
	claims := NewStateClaims()
	e := &Endpoint{
		ID:      "0123456789abcdef",
		DataDir: t.TempDir(),
		Network: &Network{AuthKey: "tskey-client-x?ephemeral=true"},
		claims:  claims,
	}
	var dir string
	e.startFn = func(info *ContainerInfo) error {
		var err error
		if dir, err = e.prepareStateDir(info, e.Network); err != nil {
			return err
		}
		if err := e.Leave(); err != nil {
			t.Errorf("Leave: %v", err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Error(err)
		}
		state := filepath.Join(dir, "tailscaled.state")
		if err := os.WriteFile(state, []byte("{}"), 0o600); err != nil {
			t.Error(err)
		}
		if _, ok := claims.claim(dir, "fedcba9876543210"); ok {
			t.Error("Leave released the state directory of a start in flight")
		}
		return errors.New("login cancelled")
	}
	e.RunTailscale(&ContainerInfo{Hostname: "app_web.1.abc", Stack: "app"})

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("%s should be removed (err=%v)", dir, err)
	}
	if got := loggedOut(); !slices.Equal(got, []string{dir}) {
		t.Errorf("logged out %v, want %s", got, dir)
	}
	if _, ok := claims.claim(dir, "fedcba9876543210"); !ok {
		t.Error("state directory still claimed after the run ended")
	}
}

// A start that reaches tailscaled after Leave began starts none.
func TestStartSupervisorRefusesWhileLeaving(t *testing.T) {
	e := &Endpoint{ID: "0123456789abcdef", DataDir: t.TempDir(), leaving: true}
	s := tailscale.NewDaemonSupervisor(tailscale.DaemonConfig{EndpointID: e.ID})
	if _, err := e.startSupervisor(s); err == nil {
		t.Error("started a supervisor for a leaving endpoint")
	}
	if status, _ := s.Status(); status != tailscale.StatusStopped {
		t.Errorf("supervisor %s, want stopped", status)
	}
}

func TestRunTailscaleRunsOnce(t *testing.T) {
	shortRetries(t)
	var calls atomic.Int32
	release := make(chan struct{})
	e := &Endpoint{ID: "0123456789abcdef", DataDir: t.TempDir()}
	e.startFn = func(*ContainerInfo) error {
		calls.Add(1)
		<-release
		return nil
	}
	done := make(chan struct{})
	go func() {
		e.RunTailscale(&ContainerInfo{Hostname: "web"})
		close(done)
	}()
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	// A second trigger (Docker event and Join both racing) must not start
	// a second tailscaled for the same endpoint.
	e.RunTailscale(&ContainerInfo{Hostname: "web"})
	close(release)
	<-done
	if got := calls.Load(); got != 1 {
		t.Errorf("start called %d times, want 1", got)
	}
}

func TestLogoutRetriesTransientFailures(t *testing.T) {
	shortRetries(t)
	var calls int
	err := logoutWithRetry(func() error {
		calls++
		if calls < 3 {
			return errors.New("use of closed network connection")
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Errorf("got err=%v after %d calls, want success after 3", err, calls)
	}

	calls = 0
	if err := logoutWithRetry(
		func() error { calls++; return errors.New("down") },
	); err == nil ||
		calls != logoutAttempts {
		t.Errorf("got err=%v after %d calls, want error after %d", err, calls, logoutAttempts)
	}
}

func TestRunTailscaleWritesStatus(t *testing.T) {
	shortRetries(t)
	data := t.TempDir()
	e := &Endpoint{ID: "0123456789abcdef", DataDir: data}
	read := func() EndpointStatusFile {
		t.Helper()
		var st EndpointStatusFile
		b, err := os.ReadFile(StatusPath(data, e.ID))
		if err != nil {
			t.Fatalf("read status: %v", err)
		}
		if err := json.Unmarshal(b, &st); err != nil {
			t.Fatal(err)
		}
		return st
	}

	var calls int
	e.startFn = func(*ContainerInfo) error {
		calls++
		if calls == 1 {
			return errors.New("control plane unreachable")
		}
		if st := read(); st.State != StatusRetrying || st.Attempts != 1 || st.Error == "" {
			t.Errorf("after a failure: %+v", st)
		}
		return nil
	}
	e.RunTailscale(&ContainerInfo{Hostname: "web", Stack: "app"})
	if st := read(); st.State != StatusRunning || st.Hostname != "web" || st.Stack != "app" {
		t.Errorf("after success: %+v", st)
	}

	if err := e.Leave(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(StatusPath(data, e.ID)); !os.IsNotExist(err) {
		t.Errorf("status should be removed on Leave (err=%v)", err)
	}

	// A new endpoint: one that left starts nothing
	e = &Endpoint{ID: e.ID, DataDir: data}
	e.startFn = func(*ContainerInfo) error { return permanentError{errors.New("invalid hostname")} }
	e.RunTailscale(&ContainerInfo{Hostname: "../x"})
	if st := read(); st.State != StatusFailed {
		t.Errorf("after a permanent error: %+v", st)
	}
}
