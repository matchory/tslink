package leakcheck

import (
	"strings"
	"testing"
	"time"
)

func TestLeaks(t *testing.T) {
	// A goroutine waiting on a channel that nothing else can reach
	func() {
		ch := make(chan struct{})
		go func() { <-ch }()
	}()
	// Found once the goroutine has blocked
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(Leaks(), "TestLeaks") {
		if time.Now().After(deadline) {
			t.Fatal("leak not found")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Without the goroutineleak profile, as in a Go that lacks it, the check
// cannot run: it says so, and fails the tests, rather than panic.
func TestLeaksWithoutProfile(t *testing.T) {
	if got := leaks(nil); !strings.Contains(got, "not available") {
		t.Errorf("leaks(nil) = %q, want it to say the profile is not available", got)
	}
}
