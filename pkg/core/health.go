package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/matchory/tslink/pkg/logger"
	"github.com/matchory/tslink/pkg/netutil"
	"github.com/matchory/tslink/pkg/tailscale"
)

// HealthLabel is the container label that turns on the readiness endpoint on
// a loopback port of the container's own, for a Docker healthcheck such as
// wget -q -O- http://127.0.0.1:<port>/ready.
const HealthLabel = "tslink.health"

// healthWarning is the status warning key for a readiness endpoint tslink
// cannot serve.
const healthWarning = "health"

// readinessCacheTTL is how long a computed readiness answers requests: a
// container asking every second does not make tslink ask tailscaled as often.
const readinessCacheTTL = 2 * time.Second

// listenInNetNS opens the readiness endpoint's listener; tests replace it.
var listenInNetNS = netutil.ListenInNetNS

// health is an endpoint's readiness endpoint.
type health struct {
	srv *http.Server

	mu    sync.Mutex // serializes computing readiness, and protects the cache
	state tailscale.Readiness
	at    time.Time
}

// ReadyPath returns the file that records that an endpoint has been ready.
func ReadyPath(dataDir, endpointID string) string {
	return filepath.Join(dataDir, "status", endpointID[:12]+".ready")
}

// startHealth serves the readiness endpoint on the loopback port the
// container's tslink.health label names, in the container's network
// namespace, unless it serves already. Only the container's own processes
// reach it. A port tslink cannot listen on is reported in the status.
func (e *Endpoint) startHealth(info *ContainerInfo) {
	if info.HealthPort == 0 {
		return
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(info.HealthPort))
	err := e.listenHealth(addr)
	if err != nil {
		logger.Warnf("Endpoint %s: cannot serve %s on %s: %v", e.ID[:12], HealthLabel, addr, err)
		e.setWarning(
			healthWarning,
			fmt.Sprintf("cannot serve %s on %s: %v", HealthLabel, addr, err),
		)
		return
	}
	e.setWarning(healthWarning, "")
}

// listenHealth starts the readiness endpoint on addr, unless it runs
// already. It holds mu, so two starts do not both listen; statusMu, never held
// with mu, is the caller's.
func (e *Endpoint) listenHealth(addr string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.health != nil || e.SandboxKey == "" {
		return nil
	}
	ln, err := listenInNetNS(e.SandboxKey, addr)
	if err != nil {
		return err
	}
	h := &health{}
	mux := http.NewServeMux()
	mux.HandleFunc("/ready", e.serveReady)
	h.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      15 * time.Second,
		MaxHeaderBytes:    4 << 10,
	}
	e.health = h
	go func() {
		if err := h.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Warnf("Endpoint %s: readiness endpoint stopped: %v", e.ID[:12], err)
		}
	}()
	logger.Infof("Endpoint %s: serving readiness on %s in the container", e.ID[:12], addr)
	return nil
}

// stopHealth stops the readiness endpoint.
func (e *Endpoint) stopHealth() {
	e.mu.Lock()
	h := e.health
	e.health = nil
	e.mu.Unlock()
	if h == nil {
		return
	}
	if err := h.srv.Close(); err != nil {
		logger.Warnf("Endpoint %s: failed to stop the readiness endpoint: %v", e.ID[:12], err)
	}
}

// serveReady answers 200 once the node is ready, and from then on until the
// backend is drained or the container leaves, so a plugin restart does not
// make Swarm replace the task; 503 otherwise. The body names the state.
func (e *Endpoint) serveReady(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Not the request's context: the server cancels it when a client stops
	// sending, as busybox nc does after its request, and the answer is shared
	// with other requests through the cache. tailscaled's queries time out
	// on their own.
	state := e.readiness(context.WithoutCancel(r.Context()))
	code := http.StatusServiceUnavailable
	if state == tailscale.Ready {
		code = http.StatusOK
	}
	e.mu.RLock()
	service := e.Service
	e.mu.RUnlock()
	body, err := json.Marshal(struct {
		State   tailscale.Readiness `json:"state"`
		Service string              `json:"service,omitempty"`
	}{state, service})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if r.Method == http.MethodGet {
		if _, err := w.Write(append(body, '\n')); err != nil {
			logger.Debugf("Endpoint %s: readiness answer not sent: %v", e.ID[:12], err)
		}
	}
}

// readiness returns the endpoint's latched or current readiness. Draining
// and leaving count first: they end the latch.
func (e *Endpoint) readiness(ctx context.Context) tailscale.Readiness {
	if e.drainedOrLeaving() {
		return tailscale.ReadyDrained
	}
	if e.latched() {
		return tailscale.Ready
	}
	e.mu.RLock()
	h := e.health
	e.mu.RUnlock()
	if h == nil {
		return e.currentReadiness(ctx)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state == "" || time.Since(h.at) >= readinessCacheTTL {
		h.state, h.at = e.currentReadiness(ctx), time.Now()
	}
	if h.state == tailscale.Ready {
		e.latch()
	}
	return h.state
}

// clearReadinessCache makes the next request compute readiness again.
func (e *Endpoint) clearReadinessCache() {
	e.mu.RLock()
	h := e.health
	e.mu.RUnlock()
	if h != nil {
		h.mu.Lock()
		h.state = ""
		h.mu.Unlock()
	}
}

// drainedOrLeaving reports whether the endpoint's backend is drained or its
// container is leaving.
func (e *Endpoint) drainedOrLeaving() bool {
	e.mu.RLock()
	leaving, supervisor := e.leaving, e.supervisor
	e.mu.RUnlock()
	return leaving || (supervisor != nil && supervisor.Drained())
}

// currentReadiness computes the endpoint's readiness now: from its own state
// first, then from tailscaled.
func (e *Endpoint) currentReadiness(ctx context.Context) tailscale.Readiness {
	if e.drainedOrLeaving() {
		return tailscale.ReadyDrained
	}
	e.statusMu.Lock()
	failed := e.status != nil && e.status.State == StatusFailed
	e.statusMu.Unlock()
	if failed {
		return tailscale.ReadyFailed
	}
	e.mu.RLock()
	supervisor, fn := e.supervisor, e.readinessFn
	e.mu.RUnlock()
	if fn != nil {
		return fn(ctx)
	}
	if supervisor == nil {
		return tailscale.ReadyStarting
	}
	return supervisor.Readiness(ctx)
}

// latched reports whether the endpoint has been ready. The record outlives
// the plugin, so recovery finds it.
func (e *Endpoint) latched() bool {
	_, err := os.Stat(ReadyPath(e.DataDir, e.ID))
	return err == nil
}

// latch records that the endpoint has been ready.
func (e *Endpoint) latch() {
	path := ReadyPath(e.DataDir, e.ID)
	err := os.MkdirAll(filepath.Dir(path), 0o700)
	if err == nil {
		err = os.WriteFile(path, nil, 0o600)
	}
	if err != nil {
		logger.Warnf("Endpoint %s: failed to record readiness: %v", e.ID[:12], err)
	}
}
