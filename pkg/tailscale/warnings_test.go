package tailscale

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Lines as tailscaled 1.102.5 logs them: feature/acme/cert.go logs through
// logger.WithPrefix(b.Logger(), fmt.Sprintf("cert(%q): ", domain)), and an
// ACME problem as tempfork/acme's Error.Error, "<status> <type>: <detail>".
// The detail is Boulder's (wfe2/wfe.go) for an order that names a certificate
// another order already replaces.
const (
	renewalBlockedLine = `2026/10/07 12:00:00 cert("web.example.ts.net"): async renewal failed: ` +
		`getCertPem: 409 urn:ietf:params:acme:error:alreadyReplaced: Error creating new order :: ` +
		`cannot indicate an order replaces certificate with serial "04a1b2", ` +
		`which already has a replacement order`
	syncRenewalBlockedLine = `2026/10/07 12:00:00 cert("web.example.ts.net"): getCertPEM: ` +
		`409 urn:ietf:params:acme:error:alreadyReplaced: already has a replacement order`
	gotCertLine = `2026/10/07 12:00:00 cert("web.example.ts.net"): got cert`
)

// recordedWarnings collects what a daemon reports through DaemonConfig.Warn.
type recordedWarnings struct {
	mu sync.Mutex
	m  map[string]string
}

func (r *recordedWarnings) warn(key, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if message == "" {
		delete(r.m, key)
		return
	}
	r.m[key] = message
}

func (r *recordedWarnings) get(key string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.m[key]
}

// newWarningDaemon returns a test daemon recording its warnings, with the
// node's log limiter reset.
func newWarningDaemon(t *testing.T) (*Daemon, *recordedWarnings) {
	t.Helper()
	old := nodeWarnings
	nodeWarnings = &warnLimiter{}
	t.Cleanup(func() { nodeWarnings = old })
	rec := &recordedWarnings{m: map[string]string{}}
	d := newTestDaemon(t, &fakeCLI{}, "svc:web")
	d.config.Warn = rec.warn
	return d, rec
}

func TestRenewalBlockedWarning(t *testing.T) {
	const key = "renewal-blocked/web.example.ts.net"
	for _, line := range []string{renewalBlockedLine, syncRenewalBlockedLine} {
		d, rec := newWarningDaemon(t)
		d.handleLine("2026/10/07 12:00:00 cert(\"web.example.ts.net\"): starting async renewal")
		if got := rec.get(key); got != "" {
			t.Fatalf("warned without a refused renewal: %q", got)
		}
		d.handleLine(line)
		got := rec.get(key)
		for _, want := range []string{"web.example.ts.net", "alreadyReplaced", "7 days"} {
			if !strings.Contains(got, want) {
				t.Errorf("warning %q lacks %q", got, want)
			}
		}
		// A certificate issued ends it
		d.handleLine(gotCertLine)
		if got := rec.get(key); got != "" {
			t.Errorf("warning kept after a certificate was issued: %q", got)
		}
	}
}

// Without the domain, as for a line from elsewhere, nothing is reported.
func TestRenewalBlockedNeedsDomain(t *testing.T) {
	d, rec := newWarningDaemon(t)
	d.handleLine("409 urn:ietf:params:acme:error:alreadyReplaced: somewhere")
	if len(rec.m) != 0 {
		t.Errorf("warnings = %q", rec.m)
	}
}

func TestWarnLimiter(t *testing.T) {
	var l warnLimiter
	now := time.Now()
	if !l.allow("a", time.Hour, now) {
		t.Fatal("first warning suppressed")
	}
	if l.allow("a", time.Hour, now.Add(59*time.Minute)) {
		t.Error("repeated within the hour")
	}
	if !l.allow("b", time.Hour, now) {
		t.Error("another key suppressed")
	}
	if !l.allow("a", time.Hour, now.Add(61*time.Minute)) {
		t.Error("suppressed after the hour")
	}
}

func TestCertExpiryWarning(t *testing.T) {
	const domain, key = "web.example.ts.net", "cert-expiry/web.example.ts.net"
	d, rec := newWarningDaemon(t)
	dir := t.TempDir()
	now := time.Now()

	d.checkCertExpiry(dir, domain, now) // no certificate yet
	if got := rec.get(key); got != "" {
		t.Fatalf("warned without a certificate: %q", got)
	}

	writeCert(t, dir, domain, domain, now.Add(30*24*time.Hour))
	d.checkCertExpiry(dir, domain, now)
	if got := rec.get(key); got != "" {
		t.Fatalf("warned 30 days before expiry: %q", got)
	}

	notAfter := now.Add(10 * 24 * time.Hour)
	writeCert(t, dir, domain, domain, notAfter)
	d.checkCertExpiry(dir, domain, now)
	got := rec.get(key)
	for _, want := range []string{domain, notAfter.UTC().Format(time.DateOnly)} {
		if !strings.Contains(got, want) {
			t.Errorf("warning %q lacks %q", got, want)
		}
	}

	// Renewed: the warning goes
	writeCert(t, dir, domain, domain, now.Add(90*24*time.Hour))
	d.checkCertExpiry(dir, domain, now)
	if got := rec.get(key); got != "" {
		t.Errorf("warning kept after renewal: %q", got)
	}
}

// The watch checks at once and then every certCheckInterval, until the daemon
// stops.
func TestWatchCertExpiry(t *testing.T) {
	setDuration(t, &certCheckInterval, 10*time.Millisecond)
	const domain, key = "web.example.ts.net", "cert-expiry/web.example.ts.net"
	d, rec := newWarningDaemon(t)
	dir := filepath.Join(d.config.StateDir, certsDirName)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	d.wg.Go(func() { d.watchCertExpiry(dir, domain) })
	time.Sleep(30 * time.Millisecond)
	if got := rec.get(key); got != "" {
		t.Fatalf("warned without a certificate: %q", got)
	}
	writeCert(t, dir, domain, domain, time.Now().Add(24*time.Hour))
	waitFor(t, "the warning", func() bool { return rec.get(key) != "" })
	d.cancel()
	d.wg.Wait()
}
