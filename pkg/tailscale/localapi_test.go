package tailscale

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

// fakeLocalAPI serves tailscaled's LocalAPI on a Unix socket and answers each
// path with the JSON set for it; a path without a reply fails, as a request
// to a tailscaled that is not running would.
type fakeLocalAPI struct {
	mu       sync.Mutex
	replies  map[string]string
	requests []string
	socket   string

	// tailscaled's login, for what the replies do not answer
	prefs     ipn.Prefs
	state     ipn.State
	nodeKey   key.NodePublic
	authKeys  []string          // The keys Start got
	startErr  string            // The error the next Start reports on the bus
	failed    bool              // The last Start failed: no login follows
	lastKey   string            // The key the last Start got
	logoutErr string            // What a logout fails with
	watchers  []chan ipn.Notify // The IPN bus watchers
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
	return serveFakeLocalAPI(t, filepath.Join(dir, "s"))
}

// serveFakeLocalAPI serves a fake LocalAPI on socket.
func serveFakeLocalAPI(t *testing.T, socket string) *fakeLocalAPI {
	t.Helper()
	f := &fakeLocalAPI{replies: map[string]string{}, socket: socket}
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
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
	body, ok := f.replies[r.URL.Path]
	f.mu.Unlock()
	if ok && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, body); err != nil {
			return
		}
		return
	}
	switch r.Method + " " + r.URL.Path {
	case "GET /localapi/v0/watch-ipn-bus":
		f.watch(w, r)
	case "PATCH " + prefsPath:
		var mp ipn.MaskedPrefs
		if err := json.NewDecoder(r.Body).Decode(&mp); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.prefs.ApplyEdits(&mp)
		prefs := f.prefs
		f.mu.Unlock()
		f.reply(w, &prefs)
	case "POST /localapi/v0/start":
		var opts ipn.Options
		if err := json.NewDecoder(r.Body).Decode(&opts); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.start(opts.AuthKey)
		w.WriteHeader(http.StatusNoContent)
	case "POST /localapi/v0/login-interactive":
		f.mu.Lock()
		// Without a key or a node key, a login needs a browser
		if !f.failed && (f.lastKey != "" || !f.nodeKey.IsZero()) {
			f.nodeKey = key.NewNode().Public()
			f.setState(ipn.Running)
		}
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case "POST /localapi/v0/logout":
		f.mu.Lock()
		if msg := f.logoutErr; msg != "" {
			f.mu.Unlock()
			http.Error(w, msg, http.StatusInternalServerError)
			return
		}
		f.nodeKey = key.NodePublic{}
		f.setState(ipn.NeedsLogin)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		f.mu.Lock()
		running := f.state != ipn.NoState
		f.mu.Unlock()
		switch {
		case running && r.URL.Path == statusPath:
			f.reply(w, f.status())
		case running && r.URL.Path == prefsPath:
			f.mu.Lock()
			prefs := f.prefs
			f.mu.Unlock()
			f.reply(w, &prefs)
		default:
			http.Error(w, "tailscaled is not running", http.StatusInternalServerError)
		}
	}
}

func (f *fakeLocalAPI) reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		return
	}
}

// loggedOut makes the fake a running tailscaled that needs a login.
func (f *fakeLocalAPI) loggedOut() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = ipn.NeedsLogin
}

// loggedIn makes the fake a running tailscaled logged in with prefs.
func (f *fakeLocalAPI) loggedIn(prefs ipn.Prefs) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prefs, f.state, f.nodeKey = prefs, ipn.Running, key.NewNode().Public()
}

// start logs in with authKey, or with the node key it has without one.
func (f *fakeLocalAPI) start(authKey string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authKeys = append(f.authKeys, authKey)
	f.lastKey = authKey
	if f.startErr != "" {
		msg := f.startErr
		f.startErr, f.failed = "", true
		f.notify(ipn.Notify{ErrMessage: &msg})
		return
	}
	f.failed = false
	if authKey == "" && f.nodeKey.IsZero() {
		f.setState(ipn.NeedsLogin)
		return
	}
	if f.nodeKey.IsZero() {
		f.nodeKey = key.NewNode().Public()
	}
	f.setState(ipn.Running)
}

// setState notes the state and tells the watchers, with the node key. The
// caller holds f.mu.
func (f *fakeLocalAPI) setState(st ipn.State) {
	f.state = st
	n := ipn.Notify{State: &st}
	if !f.nodeKey.IsZero() {
		n.SelfChange = &tailcfg.Node{Key: f.nodeKey}
	}
	f.notify(n)
}

// notify sends n to every watcher. The caller holds f.mu.
func (f *fakeLocalAPI) notify(n ipn.Notify) {
	for _, c := range f.watchers {
		select {
		case c <- n:
		default:
		}
	}
}

func (f *fakeLocalAPI) status() *ipnstate.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &ipnstate.Status{
		BackendState: f.state.String(),
		HaveNodeKey:  !f.nodeKey.IsZero(),
		Self:         &ipnstate.PeerStatus{PublicKey: f.nodeKey},
	}
}

// watch streams the IPN bus until the client goes.
func (f *fakeLocalAPI) watch(w http.ResponseWriter, r *http.Request) {
	c := make(chan ipn.Notify, 16)
	f.mu.Lock()
	f.watchers = append(f.watchers, c)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.watchers = slices.DeleteFunc(f.watchers, func(w chan ipn.Notify) bool { return w == c })
		f.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	enc := json.NewEncoder(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case n := <-c:
			if enc.Encode(n) != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

// keys returns the auth keys Start got.
func (f *fakeLocalAPI) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.authKeys)
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

// paths returns the requests so far: method, path and query.
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
