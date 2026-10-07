package tailscale

import "testing"

func TestLoginWatch(t *testing.T) {
	var w loginWatch
	// A single NeedsLogin may be a transient state during startup
	if w.observe(true) {
		t.Fatal("logged in again after one check")
	}
	if !w.observe(true) {
		t.Fatal("did not log in again after two checks")
	}
	if w.observe(false) || w.observe(true) {
		t.Fatal("Running must reset the count")
	}

	// After a failed login, wait longer before the next attempt
	w = loginWatch{}
	w.observe(true)
	w.observe(true)
	w.failed()
	checks := 0
	for !w.observe(true) {
		checks++
		if checks > 100 {
			t.Fatal("never retried")
		}
	}
	if checks < needsLoginChecks+1 {
		t.Errorf("retried after %d checks, want a longer wait after a failure", checks)
	}
}
