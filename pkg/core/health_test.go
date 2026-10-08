package core

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
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
		DataDir:    t.TempDir(),
		SandboxKey: "/run/netns/x",
		Service:    "svc:ai",
	}
	e.readinessFn = func(context.Context) tailscale.Readiness {
		calls.Add(1)
		return state.Load().(tailscale.Readiness)
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
	if _, err := os.Stat(ReadyPath(e.DataDir, e.ID)); err != nil {
		t.Errorf("latch not recorded: %v", err)
	}

	// Latched: a later state does not count, and is not even asked for
	state.Store(tailscale.ReadyStarting)
	e.clearReadinessCache()
	before := calls.Load()
	if code, got := get(t, http.MethodGet, url+"/ready"); code != 200 || got != "ready" {
		t.Errorf("latched: %d %q, want 200 ready", code, got)
	}
	if calls.Load() != before {
		t.Error("latched endpoint still asked tailscaled")
	}

	// A new Endpoint for the same ID, as after a plugin restart, is latched
	e2 := &Endpoint{ID: e.ID, DataDir: e.DataDir}
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
	if _, err := os.Stat(ReadyPath(e.DataDir, e.ID)); !os.IsNotExist(err) {
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
	e := &Endpoint{ID: "0123456789abcdef", DataDir: t.TempDir()}
	if got := e.currentReadiness(t.Context()); got != tailscale.ReadyStarting {
		t.Errorf("no supervisor: %q, want starting", got)
	}
	e.writeStatus(&ContainerInfo{}, StatusFailed, 1, nil)
	if got := e.currentReadiness(t.Context()); got != tailscale.ReadyFailed {
		t.Errorf("failed start: %q, want failed", got)
	}
	e.mu.Lock()
	e.leaving = true
	e.mu.Unlock()
	if got := e.currentReadiness(t.Context()); got != tailscale.ReadyDrained {
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

	e := &Endpoint{ID: "0123456789abcdef", DataDir: t.TempDir(), SandboxKey: "/run/netns/x"}
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

// A client that half-closes after its request, as busybox nc does, still
// gets the node's readiness: computing it does not depend on the request's
// context, which the server cancels when the client stops sending.
func TestHealthHalfClosedClient(t *testing.T) {
	var state atomic.Value
	var calls atomic.Int32
	state.Store(tailscale.Ready)
	e, url := healthEndpoint(t, &state, &calls)
	e.readinessFn = func(ctx context.Context) tailscale.Readiness {
		time.Sleep(100 * time.Millisecond) // as asking tailscaled takes a moment
		if ctx.Err() != nil {
			return tailscale.ReadyStarting
		}
		return tailscale.Ready
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
