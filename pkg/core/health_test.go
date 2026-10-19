package core

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matchory/tslink/pkg/tailscale"
)

// healthEndpoint returns an endpoint whose readiness is *state, with its
// readiness endpoint on a loopback port of the test's own namespace, and the
// endpoint's URL. calls counts how often readiness was computed.
func healthEndpoint(t *testing.T, state *atomic.Value, calls *atomic.Int32) (*Endpoint, string) {
	t.Helper()
	old := listenInNetNS
	listenInNetNS = func(_, addr string) (net.Listener, error) { return net.Listen("tcp", addr) }
	t.Cleanup(func() { listenInNetNS = old })

	// A free port, which the endpoint then listens on
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}

	e := &Endpoint{
		ID:         "0123456789abcdef",
		dataDir:    t.TempDir(),
		sandboxKey: "/run/netns/x",
		services:   []string{"svc:ai"},
	}
	e.readinessFn = func(context.Context) tailscale.ReadinessReport {
		calls.Add(1)
		return tailscale.ReadinessReport{State: state.Load().(tailscale.Readiness)}
	}
	e.startHealth(&ContainerInfo{HealthPort: port})
	t.Cleanup(e.stopHealth)
	return e, "http://127.0.0.1:" + strconv.Itoa(port)
}

// get returns the status code and the state in the body of a request.
func get(t *testing.T, method, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	var r struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(body, &r)
	return resp.StatusCode, r.State
}

// tryGet sends a GET to url and returns the status code, or the error of a
// request that got no response.
func tryGet(t *testing.T, url string) (int, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// /ready answers 503 until the node is ready, then 200, and stays 200 once
// it was: a plugin restart or a briefly lost connection to control must not
// make Swarm replace the task. Draining and leaving end it.
func TestHealthLatchesReady(t *testing.T) {
	var state atomic.Value
	var calls atomic.Int32
	state.Store(tailscale.ReadyAwaitingApproval)
	e, url := healthEndpoint(t, &state, &calls)

	if code, got := get(
		t,
		http.MethodGet,
		url+"/ready",
	); code != 503 ||
		got != "awaiting-approval" {
		t.Errorf("before ready: %d %q, want 503 awaiting-approval", code, got)
	}
	state.Store(tailscale.Ready)
	e.clearReadinessCache()
	if code, got := get(t, http.MethodGet, url+"/ready"); code != 200 || got != "ready" {
		t.Errorf("ready: %d %q, want 200 ready", code, got)
	}
	if _, err := os.Stat(ReadyPath(e.dataDir, e.ID)); err != nil {
		t.Errorf("latch not recorded: %v", err)
	}

	// Latched: a later state does not count; it is still asked for, in the
	// background, so the answer can show each Service's current state
	state.Store(tailscale.ReadyStarting)
	e.clearReadinessCache()
	if code, got := get(t, http.MethodGet, url+"/ready"); code != 200 || got != "ready" {
		t.Errorf("latched: %d %q, want 200 ready", code, got)
	}

	// A new Endpoint for the same ID, as after a plugin restart, is latched
	e2 := &Endpoint{ID: e.ID, dataDir: e.dataDir}
	if !e2.latched() {
		t.Error("latch lost for a recovered endpoint")
	}

	e.mu.Lock()
	e.leaving = true
	e.mu.Unlock()
	if code, got := get(t, http.MethodGet, url+"/ready"); code != 503 || got != "drained" {
		t.Errorf("leaving: %d %q, want 503 drained", code, got)
	}

	e.removeStatus()
	if _, err := os.Stat(ReadyPath(e.dataDir, e.ID)); !os.IsNotExist(err) {
		t.Errorf("latch left after removeStatus: %v", err)
	}
}

// A container asking every second does not make tslink ask tailscaled every
// second.
func TestHealthCachesReadiness(t *testing.T) {
	var state atomic.Value
	var calls atomic.Int32
	state.Store(tailscale.ReadyStarting)
	_, url := healthEndpoint(t, &state, &calls)
	for range 5 {
		get(t, http.MethodGet, url+"/ready")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("readiness computed %d times for 5 requests, want 1", n)
	}
}

// The endpoint answers /ready only, to GET and HEAD.
func TestHealthServesReadyOnly(t *testing.T) {
	var state atomic.Value
	var calls atomic.Int32
	state.Store(tailscale.ReadyStarting)
	_, url := healthEndpoint(t, &state, &calls)
	if code, _ := get(t, http.MethodGet, url+"/"); code != 404 {
		t.Errorf("GET /: %d, want 404", code)
	}
	if code, _ := get(t, http.MethodPost, url+"/ready"); code != 405 {
		t.Errorf("POST /ready: %d, want 405", code)
	}
	if code, _ := get(t, http.MethodHead, url+"/ready"); code != 503 {
		t.Errorf("HEAD /ready: %d, want 503", code)
	}
}

// Readiness from the endpoint's own state, before tailscaled is asked: a
// start that failed for good, a drained backend and a leaving container.
func TestCurrentReadiness(t *testing.T) {
	e := &Endpoint{ID: "0123456789abcdef", dataDir: t.TempDir()}
	if got := e.currentReadiness(t.Context()).State; got != tailscale.ReadyStarting {
		t.Errorf("no supervisor: %q, want starting", got)
	}
	e.writeStatus(&ContainerInfo{}, StatusFailed, 1, nil)
	if got := e.currentReadiness(t.Context()).State; got != tailscale.ReadyFailed {
		t.Errorf("failed start: %q, want failed", got)
	}
	e.mu.Lock()
	e.leaving = true
	e.mu.Unlock()
	if got := e.currentReadiness(t.Context()).State; got != tailscale.ReadyDrained {
		t.Errorf("leaving: %q, want drained", got)
	}
}

// Without the label, nothing listens; a port tslink cannot listen on is
// reported in the endpoint's status.
func TestHealthOptInAndListenFailure(t *testing.T) {
	var listened atomic.Bool
	old := listenInNetNS
	listenInNetNS = func(string, string) (net.Listener, error) {
		listened.Store(true)
		return nil, os.ErrPermission
	}
	t.Cleanup(func() { listenInNetNS = old })

	e := &Endpoint{ID: "0123456789abcdef", dataDir: t.TempDir(), sandboxKey: "/run/netns/x"}
	e.startHealth(&ContainerInfo{})
	if listened.Load() {
		t.Error("listened without tslink.health")
	}
	e.writeStatus(&ContainerInfo{}, StatusRunning, 0, nil)
	e.startHealth(&ContainerInfo{HealthPort: 9002})
	e.statusMu.Lock()
	_, warned := e.warnings[healthWarning]
	e.statusMu.Unlock()
	if !warned {
		t.Error("no status warning for a port tslink cannot listen on")
	}
}

// A container cannot exhaust the plugin's file descriptors or goroutines by
// opening many idle connections: the listener caps concurrent connections,
// and closing them frees their slot at once, without waiting for the read
// header timeout.
func TestHealthLimitsConnections(t *testing.T) {
	var state atomic.Value
	var calls atomic.Int32
	state.Store(tailscale.Ready)
	_, url := healthEndpoint(t, &state, &calls)
	addr := strings.TrimPrefix(url, "http://")

	before := runtime.NumGoroutine()

	const flood = 200
	var mu sync.Mutex
	var conns []net.Conn
	var wg sync.WaitGroup
	for range flood {
		wg.Go(func() {
			c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
			if err != nil {
				return // the kernel's own backlog limit, not under test here
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		})
	}
	wg.Wait()
	t.Cleanup(func() {
		for _, c := range conns {
			_ = c.Close()
		}
	})

	time.Sleep(100 * time.Millisecond) // let accepted connections' goroutines start
	if grown := runtime.NumGoroutine() - before; grown > maxHealthConns+5 {
		t.Errorf("goroutines grew by %d, want at most %d", grown, maxHealthConns+5)
	}

	for _, c := range conns {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	}
	conns = nil

	// While the server drains the flood's closed connections from the
	// listen backlog, a new connection can be reset: retry until the deadline
	deadline := time.Now().Add(1 * time.Second)
	var code int
	var err error
	for time.Now().Before(deadline) {
		if code, err = tryGet(t, url+"/ready"); code == http.StatusOK {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code != http.StatusOK {
		t.Errorf("GET after flood closed: %d (%v), want 200", code, err)
	}
}

// A client that half-closes after its request, as busybox nc does, still
// gets the node's readiness: computing it does not depend on the request's
// context, which the server cancels when the client stops sending.
func TestHealthHalfClosedClient(t *testing.T) {
	var state atomic.Value
	var calls atomic.Int32
	state.Store(tailscale.Ready)
	e, url := healthEndpoint(t, &state, &calls)
	e.readinessFn = func(ctx context.Context) tailscale.ReadinessReport {
		time.Sleep(100 * time.Millisecond) // as asking tailscaled takes a moment
		if ctx.Err() != nil {
			return tailscale.ReadinessReport{State: tailscale.ReadyStarting}
		}
		return tailscale.ReadinessReport{State: tailscale.Ready}
	}
	c, err := net.Dial("tcp", strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := io.WriteString(c, "GET /ready HTTP/1.0\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if err := c.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	answer, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(answer), `"state":"ready"`) {
		t.Errorf("half-closed client got %q, want ready", answer)
	}
}

// A start that reaches startHealth after Stop listens on nothing: Stop has
// stopped the readiness endpoint already, and nothing would close a new one.
// Guards the window between RunTailscale's unlock and startHealth.
func TestHealthNotStartedAfterStop(t *testing.T) {
	var listened atomic.Bool
	old := listenInNetNS
	listenInNetNS = func(_, addr string) (net.Listener, error) {
		listened.Store(true)
		return net.Listen("tcp", addr)
	}
	t.Cleanup(func() { listenInNetNS = old })

	e := &Endpoint{ID: "0123456789abcdef", dataDir: t.TempDir(), sandboxKey: "/run/netns/x"}
	e.Stop()
	e.startHealth(&ContainerInfo{HealthPort: 0x7fff})
	t.Cleanup(e.stopHealth)
	if listened.Load() {
		t.Error("readiness endpoint started after Stop")
	}
}

// clearReadinessCache makes the next request compute readiness again.
func (e *Endpoint) clearReadinessCache() {
	e.mu.RLock()
	h := e.health
	e.mu.RUnlock()
	if h != nil {
		h.cacheMu.Lock()
		h.report = tailscale.ReadinessReport{}
		h.cacheMu.Unlock()
	}
}

// getBody returns the body of a GET request.
func getBody(t *testing.T, url string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(body))
}

// The readiness answer keeps the service field of a single Service, adds the
// state of each Service, and after the latch shows them live.
func TestServeReadyKeepsServiceField(t *testing.T) {
	var state atomic.Value
	var calls atomic.Int32
	state.Store(tailscale.Ready)
	e, url := healthEndpoint(t, &state, &calls)
	var report atomic.Value
	report.Store(tailscale.ReadinessReport{
		State:    tailscale.Ready,
		Services: map[string]tailscale.Readiness{"svc:ai": tailscale.Ready},
	})
	e.mu.Lock()
	e.readinessFn = func(context.Context) tailscale.ReadinessReport {
		return report.Load().(tailscale.ReadinessReport)
	}
	e.mu.Unlock()
	want := `{"state":"ready","service":"svc:ai","services":{"svc:ai":"ready"}}`
	if body := getBody(t, url+"/ready"); body != want {
		t.Errorf("body = %s, want %s", body, want)
	}
	report.Store(tailscale.ReadinessReport{
		State:    tailscale.ReadyAwaitingApproval,
		Services: map[string]tailscale.Readiness{"svc:ai": tailscale.ReadyAwaitingApproval},
	})
	e.clearReadinessCache()
	// Latched, the answer comes from the cache, which a request refreshes in
	// the background
	want = `{"state":"ready","service":"svc:ai","services":{"svc:ai":"awaiting-approval"}}`
	var body string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if body = getBody(t, url+"/ready"); body == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("after the latch, body = %s, want %s", body, want)
}

// A latched endpoint answers at once, without waiting for tailscaled: a
// tailscaled slow to answer must not fail the container's healthcheck.
func TestHealthLatchedDoesNotWait(t *testing.T) {
	var state atomic.Value
	var calls atomic.Int32
	state.Store(tailscale.Ready)
	e, url := healthEndpoint(t, &state, &calls)
	if code, _ := get(t, http.MethodGet, url+"/ready"); code != 200 {
		t.Fatalf("ready: %d, want 200", code)
	}

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	e.mu.Lock()
	e.readinessFn = func(context.Context) tailscale.ReadinessReport {
		<-release // as a tailscaled that does not answer
		return tailscale.ReadinessReport{State: tailscale.ReadyStarting}
	}
	e.mu.Unlock()
	e.clearReadinessCache()
	start := time.Now()
	for range 3 {
		if code, got := get(t, http.MethodGet, url+"/ready"); code != 200 || got != "ready" {
			t.Errorf("latched: %d %q, want 200 ready", code, got)
		}
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("latched answers took %v, want no wait for tailscaled", took)
	}
}
