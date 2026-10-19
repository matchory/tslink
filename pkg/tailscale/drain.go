package tailscale

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/moby/moby/client/pkg/stringid"

	"github.com/matchory/tslink/internal/ctxtime"
	"github.com/matchory/tslink/pkg/logger"
)

// A drained Service backend leaves the tailnet only once control has learned
// of the drain. Draining only removes the Service from the node's preferences;
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
	// The last removal of Services the container does not name, which control
	// may not have fetched yet; protected by mu
	prune drainRecord

	// Set while Services the container does not name may still be
	// advertised: removing them failed
	unpruned atomic.Bool

	// Cancels the LocalAPI calls of the change holding mu, so a drain need not
	// wait for them; nil while no change runs
	cancelMu sync.Mutex
	cancel   context.CancelFunc
}

// change runs fn, a change of d's Service, under the gate's lock, with the
// context of its LocalAPI calls, which a drain cancels. Once the backend is
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

// markDrained marks the backend drained, and cancels the LocalAPI calls of the
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

// Drain stops new connections to the node's backends for all its Tailscale
// Services; existing connections continue. It stops advertising every
// Service of the node in one prefs edit, so control learns of all of them in
// one fetch and no node goes with a Service pointed at it. The backends stay
// drained: tslink does not advertise them again. Draining again does nothing
// once a drain took effect, and neither does a drain of a node that
// advertises nothing, as one drained outside tslink. A serve configuration in
// progress is cancelled rather than waited for.
func (d *daemon) Drain() error {
	g := d.config.gate
	g.markDrained()
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.drain.at.IsZero() {
		return nil
	}
	id := stringid.TruncateID(d.config.EndpointID)
	fetches, _ := d.servicesFetches()
	removed, err := d.editAdvertised(d.ctx, func(string) bool { return false })
	if err != nil {
		return err
	}
	if len(removed) == 0 {
		logger.Infof("No Service is advertised for %s, drained outside tslink: not draining again",
			id)
		g.drain = drainRecord{at: time.Now(), daemon: d, external: true}
		return nil
	}
	logger.Infof("Drained %q for %s", removed, id)
	g.drain = drainRecord{at: time.Now(), daemon: d, fetches: fetches}
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
		case <-d.ctx.Done():
			return false
		}
	}
}

// DrainAndWait drains the node's Service backends, unless that was done,
// as when the container got its stop signal, and waits for control to learn
// of it: until control fetched the drained Service list, at least
// drainAckFloor and at most drainAckTimeout after the drain. A backend never
// advertised has nothing for control to fetch, and is not waited for. A drain
// that found nothing advertised waits instead for the removal of Services
// the container does not name, if there was one.
func (s *DaemonSupervisor) DrainAndWait() {
	id := stringid.TruncateID(s.cfg.EndpointID)
	service := fmt.Sprintf("%q", ServiceNames(s.cfg.Services))
	if err := s.Drain(); err != nil {
		logger.Warnf("Endpoint %s: failed to drain %s before leaving: %v", id, service, err)
	}

	g := s.cfg.gate
	g.mu.Lock()
	drain, prune := g.drain, g.prune
	g.mu.Unlock()
	if drain.external && !prune.at.IsZero() {
		// The drain removed nothing, but Services removed at start may not
		// have been fetched yet
		drain = prune
	}
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
		ctxtime.Sleep(d.ctx, wait)
	}
	if acked {
		logger.Infof("Endpoint %s: control fetched the drained %s, %v after the drain",
			id, service, time.Since(drain.at).Round(time.Millisecond))
		return
	}
	logger.Warnf("Endpoint %s: control did not fetch the drained %s within %v, leaving anyway",
		id, service, tm.drainAckTimeout)
}

// advertiseNudges bounds how often confirmAdvertisedLater advertises a
// Service anew.
const advertiseNudges = 3

// confirmAdvertisedLater makes sure control learns that the node advertises
// service. Control fetches the node's Service list once tailscaled reports a
// change of its advertised Services, but after a plugin restart it was seen
// not to, for good: the host stayed configured but inactive, and no caller
// reached it. Without a fetch after more than after within
// advertiseAckTimeout, the Service is advertised anew, which made control
// fetch the list, up to advertiseNudges times. A drained backend is left
// drained.
func (b *serviceBackend) confirmAdvertisedLater(service string, after int64) {
	tm := b.d.config.times()
	id := stringid.TruncateID(b.d.config.EndpointID)
	b.d.wg.Go(func() {
		for attempt := 1; ; attempt++ {
			if b.d.waitServicesFetch(after, time.Now().Add(tm.advertiseAckTimeout)) ||
				b.d.ctx.Err() != nil || b.d.isDrained() {
				return
			}
			if attempt > advertiseNudges {
				logger.Warnf(
					"Endpoint %s: control has not fetched the advertised %s after %d attempts",
					id,
					service,
					advertiseNudges,
				)
				return
			}
			logger.Warnf(
				"Endpoint %s: control has not fetched the advertised %s within %v, advertising it anew",
				id,
				service,
				tm.advertiseAckTimeout,
			)
			after, _ = b.d.servicesFetches()
			if err := b.readvertise(service, tm.advertiseNudgeGap); err != nil {
				if !errors.Is(err, errDrained) && b.d.ctx.Err() == nil {
					logger.Warnf("Endpoint %s: failed to advertise %s anew: %v", id, service, err)
				}
				return
			}
		}
	})
}

// readvertise stops advertising service, and advertises it again gap later:
// two changes of the advertised Services, each reported to control. Each
// runs under the gate, so a drain in between keeps it drained.
func (b *serviceBackend) readvertise(service string, gap time.Duration) error {
	g := b.d.config.gate
	err := g.change(b.d, func(ctx context.Context) error {
		_, err := b.d.editAdvertised(ctx, func(s string) bool { return s != service })
		return err
	})
	if err != nil {
		return err
	}
	if !ctxtime.Sleep(b.d.ctx, gap) {
		return b.d.ctx.Err()
	}
	return g.change(b.d, func(ctx context.Context) error {
		_, err := b.d.addAdvertised(ctx, service)
		return err
	})
}
