package core

import (
	"encoding/json"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"
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

	e.startFn = func(*ContainerInfo) error { return permanentError{errors.New("invalid hostname")} }
	e.RunTailscale(&ContainerInfo{Hostname: "../x"})
	if st := read(); st.State != StatusFailed {
		t.Errorf("after a permanent error: %+v", st)
	}
}
