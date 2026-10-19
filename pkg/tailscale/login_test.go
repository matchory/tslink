package tailscale

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tailscale.com/ipn"
)

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

// A node control named <hostname>-N, because another node holds the
// hostname, as an orphan of a registration control answered late, is
// reported after the login; a later login under the hostname clears it.
func TestLogInWarnsOfSuffixedName(t *testing.T) {
	tests := []struct {
		hostname, dnsName string
		warns             bool
	}{
		{"web", "web-1.example.ts.net.", true},
		{"web", "web.example.ts.net.", false},
		{"web-1", "web-1.example.ts.net.", false},
		{"web-1", "web-1-1.example.ts.net.", true},
		{"web", "web-a.example.ts.net.", false},
		{"web", "webserver-1.example.ts.net.", false},
	}
	for _, tt := range tests {
		t.Run(tt.hostname+" as "+tt.dnsName, func(t *testing.T) {
			old := nodeWarnings
			nodeWarnings = &warnLimiter{}
			t.Cleanup(func() { nodeWarnings = old })
			rec := &recordedWarnings{m: map[string]string{}}
			api := newFakeLocalAPI(t)
			api.loggedOut()
			api.dnsName = tt.dnsName
			d := loginDaemon(t, api, DaemonConfig{Hostname: tt.hostname, Warn: rec.warn})
			if err := d.logIn(true, false); err != nil {
				t.Fatal(err)
			}
			got := rec.get(nodeNameWarning)
			if (got != "") != tt.warns {
				t.Fatalf("warning = %q, want warned = %v", got, tt.warns)
			}
			label, _, _ := strings.Cut(tt.dnsName, ".")
			if tt.warns && (!strings.Contains(got, `"`+label+`"`) ||
				!strings.Contains(got, `"`+tt.hostname+`"`)) {
				t.Errorf("warning = %q, want it to name %q and %q", got, label, tt.hostname)
			}

			api.mu.Lock()
			api.dnsName = tt.hostname + ".example.ts.net."
			api.mu.Unlock()
			if err := d.logIn(true, true); err != nil {
				t.Fatal(err)
			}
			if got := rec.get(nodeNameWarning); got != "" {
				t.Errorf("warning left after a login under the hostname: %q", got)
			}
		})
	}
}

// A node still logged in, as after a plugin restart, which loses the
// endpoint's warnings, is reported too.
func TestBringUpLoggedInWarnsOfSuffixedName(t *testing.T) {
	rec := &recordedWarnings{m: map[string]string{}}
	api := newFakeLocalAPI(t)
	api.loggedIn(ipn.Prefs{ControlURL: defaultControlURL})
	api.dnsName = "web-1.example.ts.net."
	d := loginDaemon(t, api, DaemonConfig{Hostname: "web", Warn: rec.warn})
	state := filepath.Join(d.config.StateDir, "tailscaled.state")
	if err := os.WriteFile(state, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.bringUp(); err != nil {
		t.Fatal(err)
	}
	if keys := api.keys(); keys != nil {
		t.Fatalf("logged in with keys %q, want the settings applied only", keys)
	}
	if rec.get(nodeNameWarning) == "" {
		t.Error("no warning")
	}
}
