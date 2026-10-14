package ctxtime

import (
	"context"
	"testing"
	"time"
)

// returnsWithinASecond fails the test unless fn returns within a second.
func returnsWithinASecond(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("%s did not return within a second", what)
	}
}

// Sleep waits for its duration, or until ctx is done.
func TestSleep(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	if !Sleep(ctx, time.Millisecond) {
		t.Error("sleep with a live context reported cancellation")
	}
	cancel()
	var slept bool
	returnsWithinASecond(t, "sleep with a cancelled context", func() {
		slept = Sleep(ctx, 3*time.Second)
	})
	if slept {
		t.Error("sleep with a cancelled context reported a full sleep")
	}
}

// Poll calls its function until it reports done, or until ctx is done.
func TestPoll(t *testing.T) {
	calls := 0
	if !Poll(t.Context(), time.Millisecond, func() bool { calls++; return calls == 3 }) {
		t.Error("poll with a live context reported cancellation")
	}
	if calls != 3 {
		t.Errorf("poll called its function %d times, want 3", calls)
	}
	ctx, cancel := context.WithCancel(t.Context())
	calls = 0
	var done bool
	returnsWithinASecond(t, "poll with a cancelled context", func() {
		done = Poll(ctx, 3*time.Second, func() bool { calls++; cancel(); return false })
	})
	if done || calls != 1 {
		t.Errorf(
			"poll with a cancelled context = %v after %d calls, want false after 1",
			done,
			calls,
		)
	}
}
