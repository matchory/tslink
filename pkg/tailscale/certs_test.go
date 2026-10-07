package tailscale

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
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

// "tailscale cert" runs through the CLI seam on the daemon's socket, without
// logging its output, which holds the private key.
func TestRequestCertRunsCLI(t *testing.T) {
	setDuration(t, &certPollInterval, 10*time.Millisecond)
	d, dir := newCertDaemon(t, &fakeCLI{})
	var calls []cliCall
	d.runCLI = func(_ context.Context, c cliCall) (cliOutput, error) {
		calls = append(calls, c)
		if len(calls) == 1 {
			return cliOutput{stderr: "not issued yet\n"}, errors.New("exit status 1")
		}
		writeCert(t, dir, certDomain, certDomain, time.Now().Add(time.Hour))
		return cliOutput{stdout: "certificate and key"}, nil
	}
	if err := d.requestCert(dir, certDomain); err != nil {
		t.Fatal(err)
	}
	want := []string{"--socket=" + testSocket, "cert", "--cert-file=-", "--key-file=-", certDomain}
	if len(calls) != 2 {
		t.Fatalf("ran %d commands, want 2", len(calls))
	}
	for _, c := range calls {
		if !slices.Equal(c.args, want) || c.prefix != "" {
			t.Errorf("call = %q with prefix %q, want %q without one", c.args, c.prefix, want)
		}
	}
}

func TestServiceDomain(t *testing.T) {
	cli := &fakeCLI{out: `{"MagicDNSSuffix":"example.ts.net"}`}
	d := newTestDaemon(t, cli, "svc:api")
	got, err := d.serviceDomain()
	if err != nil || got != "api.example.ts.net" {
		t.Errorf("serviceDomain() = %q, %v, want api.example.ts.net", got, err)
	}
	if want := [][]string{{"status", "--json"}}; !slices.EqualFunc(
		cli.snapshot(), want, slices.Equal,
	) {
		t.Errorf("calls = %q, want %q", cli.snapshot(), want)
	}

	cli.out = `{"MagicDNSSuffix":""}`
	if _, err := d.serviceDomain(); err == nil || !strings.Contains(err.Error(), "MagicDNS") {
		t.Errorf("without MagicDNS: err = %v, want an error naming MagicDNS", err)
	}
}
