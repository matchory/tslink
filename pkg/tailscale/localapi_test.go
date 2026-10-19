package tailscale

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/views"
	"tailscale.com/util/mak"
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
	prefs    ipn.Prefs
	state    ipn.State
	nodeKey  key.NodePublic
	authKeys []string // The keys Start got
	startErr string   // The error the next Start reports on the bus
	failed   bool     // The last Start failed: no login follows
	// How long control takes to answer a Start's registration; zero is at once
	startDelay time.Duration
	lastKey    string            // The key the last Start got
	logoutErr  string            // What a logout fails with
	watchers   []chan ipn.Notify // The IPN bus watchers

	// cert answers a certificate request: an error fails it
	cert func() error

	// The node as its status shows it
	tags    []string
	dnsName string
	suffix  string

	serve *ipn.ServeConfig // The serve config last set
	etag  int              // Its version, bumped by every set
	ops   [][]string       // The serve config sets and (un)advertisements

	// before runs ahead of an operation ("serve" or "prefs") with its
	// request's context, unless nil; the request fails if the context ended
	before func(ctx context.Context, op string)
	fail   map[string]string // What an operation fails with

	// What a serve config set adding that Service fails with
	failSvc map[string]string
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
	if strings.HasPrefix(r.URL.Path, certPath) {
		f.serveCert(w)
		return
	}
	if !f.admit(w, r) {
		return
	}
	switch r.Method + " " + r.URL.Path {
	case "GET " + servePath:
		f.getServe(w)
	case "POST " + servePath:
		f.setServe(w, r)
	case "GET /localapi/v0/watch-ipn-bus":
		f.watch(w, r)
	case "PATCH " + prefsPath:
		var mp ipn.MaskedPrefs
		if err := json.NewDecoder(r.Body).Decode(&mp); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		before := f.prefs.AdvertiseServices
		f.prefs.ApplyEdits(&mp)
		f.noteAdvertised(before, f.prefs.AdvertiseServices)
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
			f.run()
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

// certPath is the LocalAPI's path of a domain's certificate, without the
// domain.
const certPath = "/localapi/v0/cert/"

// serveCert answers a certificate request as tailscaled does: the key, then
// the certificate, which the client tells apart by "--\n--".
func (f *fakeLocalAPI) serveCert(w http.ResponseWriter) {
	f.mu.Lock()
	cert := f.cert
	f.mu.Unlock()
	if cert == nil {
		http.Error(w, "no certificate", http.StatusInternalServerError)
		return
	}
	if err := cert(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := io.WriteString(w, "-----END KEY--\n-----BEGIN CERTIFICATE--\n"); err != nil {
		return
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
	f.run()
}

// run gets the node running, startDelay later. The caller holds f.mu.
func (f *fakeLocalAPI) run() {
	if f.startDelay > 0 {
		// Control answers the registration late
		f.setState(ipn.Starting)
		time.AfterFunc(f.startDelay, func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.setState(ipn.Running)
		})
		return
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
	st := &ipnstate.Status{
		BackendState: f.state.String(),
		HaveNodeKey:  !f.nodeKey.IsZero(),
		Self:         &ipnstate.PeerStatus{PublicKey: f.nodeKey, DNSName: f.dnsName},
	}
	if f.tags != nil {
		tags := views.SliceOf(f.tags)
		st.Self.Tags = &tags
	}
	if f.suffix != "" {
		st.CurrentTailnet = &ipnstate.TailnetStatus{MagicDNSSuffix: f.suffix}
	}
	return st
}

// serviceHost makes the fake a running, tagged node of the tailnet
// example.ts.net, with prefs, as a Service backend is.
func (f *fakeLocalAPI) serviceHost(prefs ipn.Prefs) {
	f.loggedIn(prefs)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tags, f.dnsName, f.suffix = []string{"tag:web"}, "node.example.ts.net.", "example.ts.net"
}

// admit runs the before hook and the failure set for the request's
// operation, if it is one, and reports whether to go on with it.
func (f *fakeLocalAPI) admit(w http.ResponseWriter, r *http.Request) bool {
	var op string
	switch r.Method + " " + r.URL.Path {
	case "POST " + servePath:
		op = "serve"
	case "PATCH " + prefsPath:
		op = "prefs"
	default:
		return true
	}
	f.mu.Lock()
	before, msg := f.before, f.fail[op]
	f.mu.Unlock()
	if before != nil {
		before(r.Context(), op)
		if r.Context().Err() != nil {
			http.Error(w, "cancelled", http.StatusServiceUnavailable)
			return false
		}
	}
	if msg != "" {
		http.Error(w, msg, http.StatusInternalServerError)
		return false
	}
	return true
}

// getServe answers with the serve config and its ETag.
func (f *fakeLocalAPI) getServe(w http.ResponseWriter) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Etag", strconv.Itoa(f.etag))
	sc := f.serve
	if sc == nil {
		sc = new(ipn.ServeConfig)
	}
	f.reply(w, sc)
}

// setServe sets the serve config, unless it changed since the client read it.
func (f *fakeLocalAPI) setServe(w http.ResponseWriter, r *http.Request) {
	var sc ipn.ServeConfig
	if err := json.NewDecoder(r.Body).Decode(&sc); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if m := r.Header.Get("If-Match"); m != "" && m != strconv.Itoa(f.etag) {
		http.Error(w, "serve config changed", http.StatusPreconditionFailed)
		return
	}
	for name := range sc.Services {
		had := f.serve != nil && f.serve.Services[name] != nil
		if msg := f.failSvc[string(name)]; msg != "" && !had {
			http.Error(w, msg, http.StatusInternalServerError)
			return
		}
	}
	f.serve = &sc
	f.etag++
	f.ops = append(f.ops, []string{"serve"})
	w.WriteHeader(http.StatusOK)
}

// noteAdvertised records the Services a prefs edit advertised or stopped
// advertising. The caller holds f.mu.
func (f *fakeLocalAPI) noteAdvertised(before, after []string) {
	for _, s := range after {
		if !slices.Contains(before, s) {
			f.ops = append(f.ops, []string{"advertise", s})
		}
	}
	for _, s := range before {
		if !slices.Contains(after, s) {
			f.ops = append(f.ops, []string{"unadvertise", s})
		}
	}
}

// operations returns the serve config sets and (un)advertisements so far.
func (f *fakeLocalAPI) operations() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ops)
}

// serveConfig returns the serve config last set, or nil.
func (f *fakeLocalAPI) serveConfig() *ipn.ServeConfig {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.serve
}

// failOp makes op ("serve" or "prefs") fail with msg; an empty msg ends that.
func (f *fakeLocalAPI) failOp(op, msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mak.Set(&f.fail, op, msg)
}

// failService makes a serve config set that adds the Service name fail with
// msg; an empty msg ends that.
func (f *fakeLocalAPI) failService(name, msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mak.Set(&f.failSvc, name, msg)
}

// setBefore sets the hook that runs ahead of each operation.
func (f *fakeLocalAPI) setBefore(fn func(ctx context.Context, op string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.before = fn
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
