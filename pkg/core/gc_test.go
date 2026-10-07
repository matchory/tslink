package core

import (
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/aaomidi/tslink/pkg/tailscale"
)

func TestCollectGarbage(t *testing.T) {
	data := t.TempDir()
	old := time.Now().Add(-time.Hour)
	mkdir := func(rel string, ephemeral bool, mtime time.Time) string {
		t.Helper()
		dir := filepath.Join(data, rel)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if ephemeral {
			if err := tailscale.MarkEphemeral(dir); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chtimes(dir, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	sock := func(name string, mtime time.Time) string {
		t.Helper()
		p := filepath.Join(data, "sock", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return p
	}

	dead := mkdir("by-stack/app/app_web.1.dead", true, old)
	inUse := mkdir("by-stack/app/app_web.2.live", true, old)
	fresh := mkdir("by-stack/app/app_web.3.new", true, time.Now())
	kept := mkdir("by-hostname/db", false, old) // not ephemeral: identity reused
	deadHost := mkdir("by-hostname/tmp", true, old)
	deadSock := sock("aaaaaaaaaaaa.sock", old)
	liveSock := sock("bbbbbbbbbbbb.sock", old)
	freshSock := sock("cccccccccccc.sock", time.Now())
	deadStatus := sock("../status/aaaaaaaaaaaa.json", old)
	liveStatus := sock("../status/bbbbbbbbbbbb.json", old)

	loggedOut := stubLogout(t, nil)

	CollectGarbage(
		context.Background(),
		&Config{DataDir: data},
		NewStateClaims(),
		map[string]bool{inUse: true},
		map[string]bool{"bbbbbbbbbbbb": true},
		time.Minute,
	)

	// Only the nodes whose state goes are logged out
	if got := loggedOut(); !slices.Equal(got, []string{deadHost, dead}) {
		t.Errorf("logged out %v, want %v", got, []string{deadHost, dead})
	}

	for _, p := range []string{dead, deadHost, deadSock, deadStatus} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s should be removed (err=%v)", p, err)
		}
	}
	for _, p := range []string{inUse, fresh, kept, liveSock, freshSock, liveStatus} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s should be kept: %v", p, err)
		}
	}
}

func TestCollectGarbageKeepsStateReusedByPersistentNetwork(t *testing.T) {
	// A hostname's state directory, marked while on an ephemeral network,
	// then used by a persistent one: once orphaned, its node must not be
	// logged out and deleted as ephemeral.
	data := t.TempDir()
	dir := filepath.Join(data, "by-hostname", "db")
	syncEphemeralMarker(dir, true)
	if !tailscale.IsMarkedEphemeral(dir) {
		t.Fatal("ephemeral network did not mark the directory")
	}
	syncEphemeralMarker(dir, false)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}

	loggedOut := stubLogout(t, nil)
	CollectGarbage(
		context.Background(),
		&Config{DataDir: data},
		NewStateClaims(),
		nil,
		nil,
		time.Minute,
	)

	if got := loggedOut(); len(got) != 0 {
		t.Errorf("logged out %v, want none", got)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("%s should be kept: %v", dir, err)
	}
}

func TestClaimStateDir(t *testing.T) {
	// Recovery must claim the directory before garbage collection runs; the
	// start that would claim it otherwise runs in the background.
	e := &Endpoint{ID: "0123456789abcdef", DataDir: "/data", claims: NewStateClaims()}
	e.ClaimStateDir(&ContainerInfo{Hostname: "app_web.1.abc", Stack: "app"})
	if got := e.GetStateDir(); got != "/data/by-stack/app/app_web.1.abc" {
		t.Errorf("StateDir = %q", got)
	}
	e.ClaimStateDir(&ContainerInfo{Hostname: "../x"})
	if got := e.GetStateDir(); got != "/data/by-stack/app/app_web.1.abc" {
		t.Errorf("invalid hostname changed StateDir to %q", got)
	}
}

func TestLeaveRemovesStateOfFailedStart(t *testing.T) {
	// A task whose Tailscale never started still claimed and created its
	// state directory; with an ephemeral key nothing will reuse it.
	data := t.TempDir()
	e := &Endpoint{
		ID:      "0123456789abcdef",
		DataDir: data,
		Network: &Network{AuthKey: "tskey-client-x?ephemeral=true"},
		claims:  NewStateClaims(),
	}
	e.ClaimStateDir(&ContainerInfo{Hostname: "app_web.1.abc", Stack: "app"})
	dir := e.GetStateDir()
	if err := tailscale.MarkEphemeral(dir); err != nil {
		t.Fatal(err)
	}
	if err := e.Leave(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("%s should be removed (err=%v)", dir, err)
	}
}

func TestStateDirClaimedOnce(t *testing.T) {
	// Two replicas with the same tslink.hostname on one node would share a
	// tailscaled.state, and with it a node key.
	claims := NewStateClaims()
	info := &ContainerInfo{Hostname: "dup", Stack: "app"}
	a := &Endpoint{ID: "aaaaaaaaaaaaaaaa", DataDir: "/data", Network: &Network{}, claims: claims}
	b := &Endpoint{ID: "bbbbbbbbbbbbbbbb", DataDir: "/data", Network: &Network{}, claims: claims}
	if err := a.ClaimStateDir(info); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := a.ClaimStateDir(info); err != nil {
		t.Errorf("claiming again for the same endpoint: %v", err)
	}
	if err := b.ClaimStateDir(info); err == nil {
		t.Fatal("second endpoint claimed a directory in use")
	}
	if err := a.Leave(); err != nil {
		t.Fatal(err)
	}
	if err := b.ClaimStateDir(info); err != nil {
		t.Errorf("claim after the first endpoint left: %v", err)
	}
	if err := b.Leave(); err != nil {
		t.Fatal(err)
	}
}

func TestCollectGarbageKeepsClaimedState(t *testing.T) {
	// Collection runs periodically, so an endpoint may claim a directory
	// after the caller listed the endpoints in use.
	data := t.TempDir()
	claims := NewStateClaims()
	e := &Endpoint{ID: "fedcba9876543210", DataDir: data, claims: claims}
	if err := e.ClaimStateDir(
		&ContainerInfo{Hostname: "app_web.4.claimed", Stack: "app"},
	); err != nil {
		t.Fatal(err)
	}
	defer e.releaseStateDir()
	dir := e.GetStateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := tailscale.MarkEphemeral(dir); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}

	loggedOut := stubLogout(t, nil)

	CollectGarbage(context.Background(), &Config{DataDir: data}, claims, nil, nil, time.Minute)

	if _, err := os.Stat(dir); err != nil {
		t.Errorf("claimed %s should be kept: %v", dir, err)
	}
	if got := loggedOut(); len(got) != 0 {
		t.Errorf("claimed node logged out: %v", got)
	}
}

// stubLogout replaces logoutNode for the test with one that runs fn, if not
// nil, and records the directories. It returns them, sorted.
func stubLogout(t *testing.T, fn func(ctx context.Context, dir string) error) func() []string {
	t.Helper()
	var mu sync.Mutex
	var dirs []string
	saved := logoutNode
	t.Cleanup(func() { logoutNode = saved })
	logoutNode = func(ctx context.Context, _, dir string) error {
		mu.Lock()
		dirs = append(dirs, dir)
		mu.Unlock()
		if fn != nil {
			return fn(ctx, dir)
		}
		return nil
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := slices.Clone(dirs)
		slices.Sort(out)
		return out
	}
}

// orphan creates the state directory of an ephemeral node no endpoint uses.
func orphan(t *testing.T, data, rel string) string {
	t.Helper()
	dir := filepath.Join(data, rel)
	if err := tailscale.MarkEphemeral(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(dir, "tailscaled.state"),
		[]byte("{}"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCollectGarbageLogsOutBeforeRemoving(t *testing.T) {
	// A node logged out frees its name, so a task with the same
	// tslink.hostname comes back under it rather than as <hostname>-1
	data := t.TempDir()
	dir := orphan(t, data, "by-stack/app/web")
	info := &ContainerInfo{Hostname: "web", Stack: "app"}
	claims := NewStateClaims()
	e := &Endpoint{ID: "0123456789abcdef", DataDir: data, Network: &Network{}, claims: claims}

	stubLogout(t, func(ctx context.Context, got string) error {
		if !tailscale.StateExists(got) {
			t.Error("state removed before the logout")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("logout without a deadline")
		}
		// No endpoint starts on the state while its node is logged out
		err := e.ClaimStateDir(info)
		if err == nil || !strings.Contains(err.Error(), "garbage collect") {
			t.Errorf("claim during the logout: %v", err)
		}
		// Control answers so when Tailscale deleted the node already
		return errors.New("404 node not found")
	})

	CollectGarbage(context.Background(), &Config{DataDir: data}, claims, nil, nil, time.Minute)

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("%s should be removed although the logout failed (err=%v)", dir, err)
	}
	if err := e.ClaimStateDir(info); err != nil {
		t.Errorf("claim after collection: %v", err)
	}
	e.releaseStateDir()
}

func TestCollectGarbageKeepsStateOnShutdown(t *testing.T) {
	// A logout the plugin's shutdown interrupts runs again on the next start
	data := t.TempDir()
	dir := orphan(t, data, "by-hostname/web")
	ctx, cancel := context.WithCancel(context.Background())
	stubLogout(t, func(ctx context.Context, _ string) error {
		cancel()
		<-ctx.Done()
		return ctx.Err()
	})

	claims := NewStateClaims()
	CollectGarbage(ctx, &Config{DataDir: data}, claims, nil, nil, time.Minute)

	if !tailscale.StateExists(dir) {
		t.Errorf("state of %s removed without a logout", dir)
	}
	if !claims.claimFree(dir, "0123456789abcdef") {
		t.Error("claim of garbage collection not released")
	}
}

func TestLogoutNodeWithoutState(t *testing.T) {
	// A start that failed before tailscaled ran left no node to log out, and
	// needs no Tailscale binaries
	if err := logoutNode(context.Background(), t.TempDir(), t.TempDir()); err != nil {
		t.Errorf("logoutNode: %v", err)
	}
}

func TestCollectGarbageBoundsLogouts(t *testing.T) {
	data := t.TempDir()
	for name := range strings.FieldsSeq("a b c d e f g h i j") {
		orphan(t, data, filepath.Join("by-hostname", name))
	}
	var running, most atomic.Int32
	loggedOut := stubLogout(t, func(context.Context, string) error {
		n := running.Add(1)
		defer running.Add(-1)
		for {
			m := most.Load()
			if n <= m || most.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		return nil
	})

	CollectGarbage(
		context.Background(),
		&Config{DataDir: data},
		NewStateClaims(),
		nil,
		nil,
		time.Minute,
	)

	if got := len(loggedOut()); got != 10 {
		t.Errorf("logged out %d nodes, want 10", got)
	}
	if m := most.Load(); m > gcLogoutConcurrency || m < 2 {
		t.Errorf("%d logouts at once, want 2 to %d", m, gcLogoutConcurrency)
	}
}

func TestRemoveDownloadCache(t *testing.T) {
	data := t.TempDir()
	cache := filepath.Join(data, "tailscale-bin", "1.80.0")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "tailscaled"), []byte("bin"), 0o600); err != nil {
		t.Fatal(err)
	}
	RemoveDownloadCache(data)
	if _, err := os.Stat(filepath.Join(data, "tailscale-bin")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("download cache still there: %v", err)
	}
	if _, err := os.Stat(data); err != nil {
		t.Errorf("data directory gone: %v", err)
	}

	// Without a cache there is nothing to do
	RemoveDownloadCache(data)
}

// certFiles writes files into a certificate directory and sets their times.
type certFiles struct {
	t   *testing.T
	dir string
}

// write writes name, last modified at mtime.
func (c certFiles) write(name string, data []byte, mtime time.Time) string {
	c.t.Helper()
	path := filepath.Join(c.dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		c.t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		c.t.Fatal(err)
	}
	return path
}

// cert writes domain's certificate, expiring at notAfter, and its key, both
// last modified at mtime. It returns their paths.
func (c certFiles) cert(domain string, notAfter, mtime time.Time) (string, string) {
	c.t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		c.t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: domain},
		DNSNames:     []string{domain},
		NotBefore:    notAfter.Add(-90 * 24 * time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		c.t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		c.t.Fatal(err)
	}
	keyPath := c.write(
		domain+".key",
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		mtime,
	)
	crtPath := c.write(
		domain+".crt",
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		mtime,
	)
	return crtPath, keyPath
}

func TestCollectCertificates(t *testing.T) {
	shared := t.TempDir()
	dir := filepath.Join(shared, "certs")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	c := certFiles{t, dir}
	now := time.Now()
	day := 24 * time.Hour
	issued := now.Add(-100 * day)

	expiredCrt, expiredKey := c.cert("web-1.example.ts.net", now.Add(-10*day), issued)
	recentCrt, recentKey := c.cert("web-2.example.ts.net", now.Add(-3*day), issued)
	validCrt, validKey := c.cert("svc.example.ts.net", now.Add(60*day), issued)
	// Renewal on another host: the new key is written, the certificate not yet
	renewingCrt, renewingKey := c.cert("web-3.example.ts.net", now.Add(-10*day), issued)
	if err := os.Chtimes(renewingKey, now, now); err != nil {
		t.Fatal(err)
	}
	// Written within the week, though expired: kept
	rewrittenCrt, rewrittenKey := c.cert(
		"web-4.example.ts.net",
		now.Add(-10*day),
		now.Add(-time.Hour),
	)

	oldKey := c.write("gone.example.ts.net.key", []byte("key"), now.Add(-2*day))
	freshKey := c.write("new.example.ts.net.key", []byte("key"), now.Add(-time.Hour))
	oldTmp := c.write(".tmp-123", []byte("x"), now.Add(-2*day))
	freshTmp := c.write(".tmp-456", []byte("x"), now.Add(-time.Hour))
	staleLease := c.write("old.example.ts.net.lease", []byte("ep\n"), now.Add(-2*time.Hour))
	liveLease := c.write("svc.example.ts.net.lease", []byte("ep\n"), now.Add(-30*time.Second))
	idleLease := c.write("idle.example.ts.net.lease", []byte("ep\n"), now.Add(-30*time.Minute))
	account := c.write(acmeAccountKey, []byte("account"), now.Add(-400*day))
	unknown := c.write("notes.txt", []byte("x"), now.Add(-400*day))
	tailscaledTmp := c.write("web-5.example.ts.net.crt.tmp123", []byte("x"), now.Add(-400*day))
	garbled := c.write("bad.example.ts.net.crt", []byte("not a certificate"), issued)
	garbledKey := c.write("bad.example.ts.net.key", []byte("key"), issued)

	CollectGarbage(
		context.Background(),
		&Config{DataDir: t.TempDir(), SharedDir: shared},
		NewStateClaims(),
		nil,
		nil,
		time.Minute,
	)

	for _, p := range []string{expiredCrt, expiredKey, oldKey, oldTmp, staleLease} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s should be removed (err=%v)", filepath.Base(p), err)
		}
	}
	for _, p := range []string{
		recentCrt, recentKey, validCrt, validKey, renewingCrt, renewingKey, rewrittenCrt, rewrittenKey,
		freshKey, freshTmp, liveLease, idleLease, account, unknown, tailscaledTmp, garbled, garbledKey,
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s should be kept: %v", filepath.Base(p), err)
		}
	}
}

// Hosts sharing the directory collect it at the same time: another may have
// removed a file first.
func TestCollectCertificatesRemovedElsewhere(t *testing.T) {
	dir := t.TempDir()
	c := certFiles{t, dir}
	now := time.Now()
	crt, key := c.cert(
		"web-1.example.ts.net",
		now.Add(-10*24*time.Hour),
		now.Add(-100*24*time.Hour),
	)
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	collectCertificates(dir, now)
	if _, err := os.Stat(crt); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("certificate whose key is gone should be removed (err=%v)", err)
	}

	removeFile(crt) // gone already
	collectCertificates(filepath.Join(dir, "missing"), now)
}
