package tailscale

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/moby/moby/client/pkg/stringid"

	"github.com/matchory/tslink/pkg/logger"
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

// errDrained stops configuring a Service backend that was drained meanwhile.
var errDrained = errors.New("service backend is drained")

// serviceGate keeps a drained Service backend drained. A supervisor's daemons
// share it, so it survives a restart of tailscaled.
type serviceGate struct {
	drained atomic.Bool // set by the first drain, even one that failed

	// Set once tslink advertised the Service, or tailscaled started on node
	// state that may advertise it from before. A backend never advertised
	// gives control nothing to fetch when it is drained.
	advertised atomic.Bool

	// Held while tslink changes the Service's serve configuration or
	// advertisement, so a drain cannot slip between a check and a change
	mu    sync.Mutex
	drain drainRecord // the drain that took effect; protected by mu

	// Cancels the CLI calls of the change holding mu, so a drain need not
	// wait for them; nil while no change runs
	cancelMu sync.Mutex
	cancel   context.CancelFunc
}

// change runs fn, a change of d's Service, under the gate's lock, with the
// context of its CLI calls, which a drain cancels. Once the backend is
// drained, before fn or while it runs, it returns errDrained: a call the
// drain cancelled is no failure.
func (g *serviceGate) change(d *daemon, fn func(ctx context.Context) error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	ctx, cancel := context.WithCancel(d.ctx)
	defer cancel()
	g.setCancel(cancel)
	defer g.setCancel(nil)
	// Checked after the cancel func is in place: a drain that set drained
	// later finds it
	if g.drained.Load() {
		return errDrained
	}
	if err := fn(ctx); err != nil {
		if g.drained.Load() {
			return errDrained
		}
		return err
	}
	return nil
}

func (g *serviceGate) setCancel(cancel context.CancelFunc) {
	g.cancelMu.Lock()
	defer g.cancelMu.Unlock()
	g.cancel = cancel
}

// markDrained marks the backend drained, and cancels the CLI calls of the
// change in progress, if any.
func (g *serviceGate) markDrained() {
	g.drained.Store(true)
	g.cancelMu.Lock()
	defer g.cancelMu.Unlock()
	if g.cancel != nil {
		g.cancel()
	}
}

// drainRecord is a drain that took effect.
type drainRecord struct {
	at      time.Time
	daemon  *daemon // the daemon that drained
	fetches int64   // its Service list fetches before the drain

	// The Service was not advertised when tslink came to drain it, as after
	// "tailscale serve drain" from outside tslink: control has nothing to
	// fetch for this drain.
	external bool
}

// isDrained reports whether the Service backend was drained.
func (d *daemon) isDrained() bool {
	return d.config.gate.drained.Load()
}

// Drain stops new connections to the node's backend for a Tailscale Service;
// existing connections continue. The backend stays drained: tslink does not
// advertise it again. Draining again does nothing once a drain took effect,
// and neither does a drain of a Service the node's prefs no longer advertise,
// as one drained outside tslink. A serve configuration in progress is
// cancelled rather than waited for.
func (d *daemon) Drain(service string) error {
	g := d.config.gate
	g.markDrained()
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.drain.at.IsZero() {
		return nil
	}
	if d.notAdvertised(service) {
		logger.Infof("%s is not advertised for %s, drained outside tslink: not draining it again",
			service, d.config.EndpointID)
		g.drain = drainRecord{at: time.Now(), daemon: d, external: true}
		return nil
	}

	fetches, _ := d.servicesFetches()
	if err := d.runServe(d.ctx, "serve-drain", "drain", service); err != nil {
		return err
	}
	g.drain = drainRecord{at: time.Now(), daemon: d, fetches: fetches}
	return nil
}

// notAdvertised reports whether the node's prefs do not advertise service;
// false if tailscaled cannot tell.
func (d *daemon) notAdvertised(service string) bool {
	ctx, cancel := context.WithTimeout(d.ctx, 10*time.Second)
	defer cancel()
	prefs, err := d.lc.GetPrefs(ctx)
	if err != nil {
		logger.Debugf(
			"Reading prefs before draining %s for %s: %v",
			service,
			d.config.EndpointID,
			err,
		)
		return false
	}
	return !slices.Contains(prefs.AdvertiseServices, service)
}

// runServe runs "tailscale serve <args>", as drain and advertise.
func (d *daemon) runServe(ctx context.Context, prefix string, args ...string) error {
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
func (d *daemon) waitServicesFetch(after int64, deadline time.Time) bool {
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
// drainAckFloor and at most drainAckTimeout after the drain. A backend never
// advertised has nothing for control to fetch, and is not waited for.
func (s *DaemonSupervisor) DrainAndWait(service string) {
	id := stringid.TruncateID(s.cfg.EndpointID)
	if err := s.Drain(service); err != nil {
		logger.Warnf("Endpoint %s: failed to drain %s before leaving: %v", id, service, err)
	}

	g := s.cfg.gate
	g.mu.Lock()
	drain := g.drain
	g.mu.Unlock()
	d := s.getDaemon()
	if drain.at.IsZero() || drain.external || d == nil {
		return
	}
	if !g.advertised.Load() {
		logger.Debugf("Endpoint %s: %s was never advertised, not waiting for control", id, service)
		return
	}
	after := drain.fetches
	if d != drain.daemon {
		// tailscaled restarted since: any fetch of the new one is after the drain
		after = 0
	}

	tm := s.cfg.times()
	logger.Infof("Endpoint %s: %s drained, waiting for control to fetch it", id, service)
	acked := d.waitServicesFetch(after, drain.at.Add(tm.drainAckTimeout))
	if wait := time.Until(drain.at.Add(tm.drainAckFloor)); wait > 0 {
		sleep(d.ctx, wait)
	}
	if acked {
		logger.Infof("Endpoint %s: control fetched the drained %s, %v after the drain",
			id, service, time.Since(drain.at).Round(time.Millisecond))
		return
	}
	logger.Warnf("Endpoint %s: control did not fetch the drained %s within %v, leaving anyway",
		id, service, tm.drainAckTimeout)
}
