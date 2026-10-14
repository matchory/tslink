package tailscale

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLinkCertsDir(t *testing.T) {
	stateDir, shared := t.TempDir(), filepath.Join(t.TempDir(), "certs")
	if err := LinkCertsDir(stateDir, shared); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(stateDir, certsDirName)
	if target, err := os.Readlink(link); err != nil || target != shared {
		t.Fatalf("Readlink = %q, %v; want %q", target, err, shared)
	}
	key, err := os.ReadFile(filepath.Join(shared, acmeAccountKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(key)
	if block == nil || block.Type != "EC PRIVATE KEY" {
		t.Fatalf("account key is not an EC PRIVATE KEY PEM block")
	}
	if _, err := x509.ParseECPrivateKey(block.Bytes); err != nil {
		t.Fatalf("account key does not parse: %v", err)
	}

	// Again: the link and the key stay
	if err := LinkCertsDir(stateDir, shared); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(
		filepath.Join(shared, acmeAccountKeyFile),
	); !bytes.Equal(
		again,
		key,
	) {
		t.Error("account key was replaced")
	}
}

func TestLinkCertsDirKeepsDirectory(t *testing.T) {
	stateDir := t.TempDir()
	own := filepath.Join(stateDir, certsDirName)
	if err := os.Mkdir(own, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := LinkCertsDir(stateDir, filepath.Join(t.TempDir(), "certs")); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Lstat(own); err != nil || !st.IsDir() {
		t.Errorf("certs directory was replaced: %v", err)
	}
}

func TestLinkCertsDirDisabled(t *testing.T) {
	stateDir := t.TempDir()
	if err := LinkCertsDir(stateDir, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(stateDir, certsDirName)); !os.IsNotExist(err) {
		t.Errorf("certs link created without a shared directory: %v", err)
	}
}

func TestEnsureAccountKeyRace(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if err := ensureAccountKey(dir); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != acmeAccountKeyFile {
		t.Errorf("directory holds %v, want only the account key", entries)
	}
}

// writeCert writes a self-signed certificate for name, valid until notAfter,
// under domain's file names in dir.
func writeCert(t *testing.T, dir, domain, name string, notAfter time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    notAfter.Add(-90 * 24 * time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	crt := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(filepath.Join(dir, domain+".crt"), crt, 0o600); err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(filepath.Join(dir, domain+".key"), keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestValidCert(t *testing.T) {
	const domain = "web.example.ts.net"
	now := time.Now()
	tests := []struct {
		name     string
		certName string
		notAfter time.Time
		want     bool
	}{
		{"valid", domain, now.Add(time.Hour), true},
		{"expired", domain, now.Add(-time.Hour), false},
		{"other name", "api.example.ts.net", now.Add(time.Hour), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeCert(t, dir, domain, tt.certName, tt.notAfter)
			if got := validCert(dir, domain, now); got != tt.want {
				t.Errorf("validCert = %v, want %v", got, tt.want)
			}
		})
	}

	t.Run("missing", func(t *testing.T) {
		if validCert(t.TempDir(), domain, now) {
			t.Error("validCert = true for an empty directory")
		}
	})

	t.Run("key of another certificate", func(t *testing.T) {
		dir, other := t.TempDir(), t.TempDir()
		writeCert(t, dir, domain, domain, now.Add(time.Hour))
		writeCert(t, other, domain, domain, now.Add(time.Hour))
		key, err := os.ReadFile(filepath.Join(other, domain+".key"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, domain+".key"), key, 0o600); err != nil {
			t.Fatal(err)
		}
		if validCert(dir, domain, now) {
			t.Error("validCert = true for a mismatched pair")
		}
	})
}

func TestCertLease(t *testing.T) {
	const domain = "web.example.ts.net"
	dir := t.TempDir()

	first, err := tryLease(dir, domain, "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tryLease(dir, domain, "b"); !errors.Is(err, errLeaseHeld) {
		t.Fatalf("second tryLease = %v, want errLeaseHeld", err)
	}
	first.release()

	second, err := tryLease(dir, domain, "b")
	if err != nil {
		t.Fatalf("tryLease after release = %v", err)
	}
	// The holder died: its lease goes stale
	stale := time.Now().Add(-certLeaseStale - time.Minute)
	close(second.stop)
	<-second.done
	if err := os.Chtimes(second.path, stale, stale); err != nil {
		t.Fatal(err)
	}
	third, err := tryLease(dir, domain, "c")
	if err != nil {
		t.Fatalf("tryLease of a stale lease = %v", err)
	}
	holder, err := os.ReadFile(third.path)
	if err != nil || string(holder) != "c\n" {
		t.Errorf("lease holder = %q, %v; want c", holder, err)
	}
	third.release()
	if _, err := os.Stat(third.path); !os.IsNotExist(err) {
		t.Errorf("lease file left after release: %v", err)
	}
}

func TestServesHTTPS(t *testing.T) {
	if servesHTTPS([]ServeEndpoint{{Proto: "http"}, {Proto: "tcp"}}) {
		t.Error("servesHTTPS = true without an https endpoint")
	}
	if !servesHTTPS([]ServeEndpoint{{Proto: "http"}, {Proto: "https"}}) {
		t.Error("servesHTTPS = false with an https endpoint")
	}
}

func TestServesWeb(t *testing.T) {
	if servesWeb([]ServeEndpoint{{Proto: "tcp"}}) {
		t.Error("servesWeb = true for TCP only")
	}
	for _, proto := range []string{"http", "https"} {
		if !servesWeb([]ServeEndpoint{{Proto: "tcp"}, {Proto: proto}}) {
			t.Errorf("servesWeb = false with an %s endpoint", proto)
		}
	}
}

// The certificate is requested through the LocalAPI, which leaves it in
// tailscaled's certificate directory, until it is there.
func TestRequestCertUsesLocalAPI(t *testing.T) {
	cli := &fakeCLI{}
	d, dir := newCertDaemon(t, cli)
	setTiming(&d.config, func(tm *timing) { tm.certPollInterval = 10 * time.Millisecond })
	api := newFakeLocalAPI(t)
	d.lc = api.client()
	// Written here: the fake LocalAPI answers off the test goroutine
	issued := t.TempDir()
	writeCert(t, issued, certDomain, certDomain, time.Now().Add(time.Hour))
	requests := 0
	api.cert = func() error {
		requests++
		if requests == 1 {
			return errors.New("not issued yet")
		}
		for _, ext := range []string{".key", ".crt"} {
			if err := os.Rename(
				filepath.Join(issued, certDomain+ext),
				filepath.Join(dir, certDomain+ext),
			); err != nil {
				return err
			}
		}
		return nil
	}
	if err := d.requestCert(dir, certDomain); err != nil {
		t.Fatal(err)
	}
	want := "GET " + certPath + certDomain + "?type=pair&min_validity=0s"
	if got := api.paths(); len(got) != 2 || got[0] != want || got[1] != want {
		t.Errorf("requests = %q, want %q twice", got, want)
	}
	if calls := cli.snapshot(); len(calls) != 0 {
		t.Errorf("ran %q, want no tailscale command", calls)
	}
}

func TestServiceDomain(t *testing.T) {
	api := newFakeLocalAPI(t)
	api.set(statusPath, `{"CurrentTailnet":{"MagicDNSSuffix":"example.ts.net"}}`)
	d := newTestDaemon(t, &fakeCLI{}, "svc:api")
	d.lc = api.client()
	got, err := d.serviceDomain()
	if err != nil || got != "api.example.ts.net" {
		t.Errorf("serviceDomain() = %q, %v, want api.example.ts.net", got, err)
	}
	if want := []string{"GET " + statusPath + "?peers=false"}; !slices.Equal(api.paths(), want) {
		t.Errorf("requests = %q, want %q", api.paths(), want)
	}

	api.set(statusPath, `{"CurrentTailnet":{"MagicDNSSuffix":""}}`)
	if _, err := d.serviceDomain(); err == nil || !strings.Contains(err.Error(), "MagicDNS") {
		t.Errorf("without MagicDNS: err = %v, want an error naming MagicDNS", err)
	}
}

// The domain names files in the certificate directory, which the plugin
// writes and deletes as root. A Service name from a label or a MagicDNS
// suffix from control that is not a DNS name must not name a path elsewhere.
func TestServiceDomainRejectsNonDNSNames(t *testing.T) {
	for _, c := range []struct{ service, suffix string }{
		{"svc:x/../../../run/x", "example.ts.net"},
		{"svc:..", "example.ts.net"},
		{"svc:api", "x/../../../run"},
		{"svc:api", "example.ts.net/.."},
		{"svc:api", ".."},
		{"svc:a b", "example.ts.net"},
		{"svc:", "example.ts.net"},
	} {
		suffix, err := json.Marshal(c.suffix)
		if err != nil {
			t.Fatal(err)
		}
		api := newFakeLocalAPI(t)
		api.set(statusPath, `{"CurrentTailnet":{"MagicDNSSuffix":`+string(suffix)+`}}`)
		d := newTestDaemon(t, &fakeCLI{}, c.service)
		d.lc = api.client()
		if got, err := d.serviceDomain(); err == nil {
			t.Errorf("serviceDomain() for %q in %q = %q, want an error", c.service, c.suffix, got)
		}
	}

	// Control: the same setup accepts a DNS name, so the cases above fail on
	// the name alone
	api := newFakeLocalAPI(t)
	api.set(statusPath, `{"CurrentTailnet":{"MagicDNSSuffix":"example.ts.net"}}`)
	d := newTestDaemon(t, &fakeCLI{}, "svc:api")
	d.lc = api.client()
	if _, err := d.serviceDomain(); err != nil {
		t.Errorf("control: serviceDomain() = %v", err)
	}
}

// newDomainDaemon is newCertDaemon with a LocalAPI that gives the Service
// the domain web.example.ts.net.
func newDomainDaemon(t *testing.T, cli *fakeCLI) (*daemon, string) {
	t.Helper()
	d, dir := newCertDaemon(t, cli)
	api := newFakeLocalAPI(t)
	api.set(statusPath, `{"CurrentTailnet":{"MagicDNSSuffix":"example.ts.net"}}`)
	d.lc = api.client()
	return d, dir
}

const webDomain = "web.example.ts.net"

// Configuring the Service again, as Reauthenticate does, starts no second
// certificate expiry watcher.
func TestCertExpiryWatcherStartsOnce(t *testing.T) {
	d, dir := newDomainDaemon(t, &fakeCLI{})
	writeCert(t, dir, webDomain, webDomain, time.Now().Add(90*24*time.Hour))
	var mu sync.Mutex
	checks := 0
	d.config.Warn = func(key, _ string) {
		if key == "cert-expiry/"+webDomain {
			mu.Lock()
			checks++
			mu.Unlock()
		}
	}
	for range 2 {
		if err := d.configureServiceWhenCertified(); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Stop(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if checks != 1 {
		t.Errorf("certificate checked %d times, want once: one watcher, checking at once", checks)
	}
}

// Configuring the Service again while it waits for its certificate starts
// no second waiter.
func TestCertWaiterStartsOnce(t *testing.T) {
	fake := &fakeCLI{}
	d, dir := newDomainDaemon(t, fake)
	setTiming(&d.config, func(tm *timing) { tm.certPollInterval = 10 * time.Millisecond })
	other, err := tryLease(dir, webDomain, "other")
	if err != nil {
		t.Fatal(err)
	}
	defer other.release()
	for range 2 {
		if err := d.configureServiceWhenCertified(); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(30 * time.Millisecond)
	writeCert(t, dir, webDomain, webDomain, time.Now().Add(90*24*time.Hour))
	time.Sleep(100 * time.Millisecond)
	if err := d.Stop(); err != nil {
		t.Fatal(err)
	}
	serves := 0
	for _, c := range fake.snapshot() {
		if slices.Contains(c, "--https=443") {
			serves++
		}
	}
	if serves != 1 {
		t.Errorf("Service configured %d times once the certificate existed, want once", serves)
	}
}

// The configuration once the certificate exists is one of the daemon's
// goroutines: when Stop returns, it has ended, and released its lease.
func TestStopWaitsForConfigurationAfterCert(t *testing.T) {
	cli := &fakeCLI{}
	d, dir := newDomainDaemon(t, cli)
	started := make(chan struct{})
	var once sync.Once
	d.runCLI = func(ctx context.Context, c cliCall) (cliOutput, error) {
		if slices.Contains(c.args, "--https=443") {
			once.Do(func() { close(started) })
			<-ctx.Done()
			time.Sleep(100 * time.Millisecond) // The command takes a moment to exit
			return cliOutput{}, ctx.Err()
		}
		return cli.run(ctx, c)
	}
	if err := d.configureServiceWhenCertified(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("the Service was not configured to issue its certificate")
	}
	if err := d.Stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, webDomain+".lease")); !os.IsNotExist(err) {
		t.Errorf("Stop returned before the configuration ended: lease kept (%v)", err)
	}
}

// Once the wait for the certificate ended, configuring the Service again
// without a valid certificate, as Reauthenticate does, waits for it again.
func TestCertWaiterStartsAgainAfterItEnded(t *testing.T) {
	fake := &fakeCLI{}
	d, dir := newDomainDaemon(t, fake)
	setTiming(&d.config, func(tm *timing) { tm.certPollInterval = 10 * time.Millisecond })
	other, err := tryLease(dir, webDomain, "other")
	if err != nil {
		t.Fatal(err)
	}
	defer other.release()
	serves := func() int {
		n := 0
		for _, c := range fake.snapshot() {
			if slices.Contains(c, "--https=443") {
				n++
			}
		}
		return n
	}
	waitServes := func(want int) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for serves() < want {
			if time.Now().After(deadline) {
				t.Fatalf("Service configured %d times, want %d", serves(), want)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	for i := 1; i <= 2; i++ {
		writeCert(t, dir, webDomain, webDomain, time.Now().Add(-time.Hour))
		if err := d.configureServiceWhenCertified(); err != nil {
			t.Fatal(err)
		}
		writeCert(t, dir, webDomain, webDomain, time.Now().Add(90*24*time.Hour))
		waitServes(i)
		for d.certWaiting.Load() {
			time.Sleep(time.Millisecond)
		}
	}
	if err := d.Stop(); err != nil {
		t.Fatal(err)
	}
}
