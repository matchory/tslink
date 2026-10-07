package tailscale

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aaomidi/tslink/pkg/logger"
)

// A drained Service backend leaves the tailnet only once control has learned
// of the drain. "tailscale serve drain" only changes the node's preferences;
// tailscaled reports the change to control, which then fetches the node's
// Service list over c2n. A node logged out before that fetch is deleted with
// the drain unprocessed, and callers can keep the Service's VIP pointed at it
// for minutes.

// servicesFetchLog is what tailscaled logs when control fetches its Service
// list (ipn/ipnlocal/serve.go, handleC2NVIPServicesGet, in Tailscale 1.102).
const servicesFetchLog = "c2n: GET /vip-services received"

// How long leaving waits after a drain: at least drainAckFloor, and up to
// drainAckTimeout for control to fetch the drained Service list. Variables
// for tests.
var (
	drainAckFloor   = time.Second
	drainAckTimeout = 10 * time.Second
)

// errDrained stops configuring a Service backend that was drained meanwhile.
var errDrained = errors.New("service backend is drained")

// serviceGate keeps a drained Service backend drained. A supervisor's daemons
// share it, so it survives a restart of tailscaled.
type serviceGate struct {
	drained atomic.Bool // set by the first drain, even one that failed

	// Held while tslink changes the Service's serve configuration or
	// advertisement, so a drain cannot slip between a check and a change
	mu    sync.Mutex
	drain drainRecord // the drain that took effect; protected by mu
}

// drainRecord is a drain that took effect.
type drainRecord struct {
	at      time.Time
	daemon  *Daemon // the daemon that drained
	fetches int64   // its Service list fetches before the drain
}

// isDrained reports whether the Service backend was drained.
func (d *Daemon) isDrained() bool {
	return d.config.gate.drained.Load()
}

// Drain stops new connections to the node's backend for a Tailscale Service;
// existing connections continue. The backend stays drained: tslink does not
// advertise it again. Draining again does nothing once a drain took effect.
func (d *Daemon) Drain(service string) error {
	g := d.config.gate
	g.drained.Store(true)
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.drain.at.IsZero() {
		return nil
	}

	fetches, _ := d.servicesFetches()
	if err := d.runServe(context.Background(), "serve-drain", "drain", service); err != nil {
		return err
	}
	g.drain = drainRecord{at: time.Now(), daemon: d, fetches: fetches}
	return nil
}

// runServe runs "tailscale serve <args>", as drain and advertise.
func (d *Daemon) runServe(ctx context.Context, prefix string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := d.tailscale(
		ctx,
		prefix,
		append([]string{"--socket=" + d.socketPath, serveCmd}, args...)...)
	if err != nil {
		return fmt.Errorf(
			"tailscale serve %s failed: %w (output: %s)",
			args[0],
			err,
			strings.TrimSpace(out),
		)
	}
	return nil
}

// waitServicesFetch waits until control has fetched the Service list more than
// after times, or until deadline. It reports whether it did.
func (d *Daemon) waitServicesFetch(after int64, deadline time.Time) bool {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for {
		fetches, next := d.servicesFetches()
		if fetches > after {
			return true
		}
		select {
		case <-next:
		case <-timer.C:
			return false
		}
	}
}

// DrainAndWait drains the node's backend for service, unless that was done,
// as when the container got its stop signal, and waits for control to learn
// of it: until control fetched the drained Service list, at least
// drainAckFloor and at most drainAckTimeout after the drain.
func (s *DaemonSupervisor) DrainAndWait(service string) {
	id := s.cfg.EndpointID[:min(12, len(s.cfg.EndpointID))]
	if err := s.Drain(service); err != nil {
		logger.Warnf("Endpoint %s: failed to drain %s before leaving: %v", id, service, err)
	}

	g := s.cfg.gate
	g.mu.Lock()
	drain := g.drain
	g.mu.Unlock()
	d := s.GetDaemon()
	if drain.at.IsZero() || d == nil {
		return
	}
	after := drain.fetches
	if d != drain.daemon {
		// tailscaled restarted since: any fetch of the new one is after the drain
		after = 0
	}

	logger.Infof("Endpoint %s: %s drained, waiting for control to fetch it", id, service)
	acked := d.waitServicesFetch(after, drain.at.Add(drainAckTimeout))
	if wait := time.Until(drain.at.Add(drainAckFloor)); wait > 0 {
		time.Sleep(wait)
	}
	if acked {
		logger.Infof("Endpoint %s: control fetched the drained %s, %v after the drain",
			id, service, time.Since(drain.at).Round(time.Millisecond))
		return
	}
	logger.Warnf("Endpoint %s: control did not fetch the drained %s within %v, leaving anyway",
		id, service, drainAckTimeout)
}
