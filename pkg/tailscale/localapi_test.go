package tailscale

import (
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"tailscale.com/client/local"
)

// fakeLocalAPI serves tailscaled's LocalAPI on a Unix socket and answers each
// path with the JSON set for it; a path without a reply fails, as a request
// to a tailscaled that is not running would.
type fakeLocalAPI struct {
	mu       sync.Mutex
	replies  map[string]string
	requests []string
	socket   string
}

func newFakeLocalAPI(t *testing.T) *fakeLocalAPI {
	t.Helper()
	// Short path: Unix socket paths are limited to 108 bytes.
	//nolint:usetesting // t.TempDir is too long for a socket path
	dir, err := os.MkdirTemp("", "lapi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	f := &fakeLocalAPI{replies: map[string]string{}, socket: filepath.Join(dir, "s")}
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "unix", f.socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: f}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	}()
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Error(err)
		}
	})
	return f
}

func (f *fakeLocalAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.URL.RequestURI())
	body, ok := f.replies[r.URL.Path]
	f.mu.Unlock()
	if !ok {
		http.Error(w, "tailscaled is not running", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := io.WriteString(w, body); err != nil {
		return
	}
}

// set makes path answer with body; an empty body makes it fail.
func (f *fakeLocalAPI) set(path, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if body == "" {
		delete(f.replies, path)
		return
	}
	f.replies[path] = body
}

// paths returns the paths, with their queries, requested so far.
func (f *fakeLocalAPI) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// client returns a LocalAPI client for the fake.
func (f *fakeLocalAPI) client() *local.Client {
	return &local.Client{Socket: f.socket, UseSocketOnly: true}
}

const (
	statusPath = "/localapi/v0/status"
	prefsPath  = "/localapi/v0/prefs"
	servePath  = "/localapi/v0/serve-config"
)
