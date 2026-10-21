package tailscale

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
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
// It logs it before it reads the advertised Services.
const servicesFetchLog = "c2n: GET /vip-services received"

// editPrefsLog is what tailscaled logs of a prefs edit it applies, before it
// sets the prefs and while it holds the lock the Service list fetch waits for
// (ipn/ipnlocal/local.go, editPrefsLocked, in Tailscale 1.104). A fetch
// logged after it answers with the edited Services; an edit it refused logs
// "EditPrefs(" instead.
const editPrefsLog = "EditPrefs: MaskedPrefs{"

// advertiseServicesField is how an edit of the advertised Services shows in
// editPrefsLog's line: AdvertiseServices=["svc:a" "svc:b"].
const advertiseServicesField = "AdvertiseServices=["

// errDrained stops configuring a Service backend that was drained meanwhile.
var errDrained = errors.New("service backend is drained")

// serviceGate keeps a drained Service backend drained. A supervisor's daemons
// share it, so it survives a restart of tailscaled.
type serviceGate struct {
	drained atomic.Bool // set by the first drain, even one that failed

	// Held while tslink changes the Service's serve configuration or
	// advertisement, so a drain cannot slip between a check and a change
	mu sync.Mutex
	// The last edit of tslink's that removed Services from the node's prefs,
	// which control may not have fetched yet, and a channel closed when it
	// changes; protected by mu
	removal removalRecord
	changed chan struct{}

	// Set while Services the container does not name may still be
	// advertised: removing them failed
	unpruned atomic.Bool

	// Cancels the LocalAPI calls of the change holding mu, so a drain need not
	// wait for them; nil while no change runs. cancelledAt is when a drain
	// last cancelled one: tailscaled may still carry its call out.
	cancelMu    sync.Mutex
	cancel      context.CancelFunc
	cancelledAt time.Time
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
		g.cancelledAt = time.Now()
	}
}

// lastCancel returns when a drain last cancelled a change in progress, zero
// if none did.
func (g *serviceGate) lastCancel() time.Time {
	g.cancelMu.Lock()
	defer g.cancelMu.Unlock()
	return g.cancelledAt
}

// removalRecord is an edit of tslink's that removed Services from a daemon's
// prefs. Whether it took effect, and when, tslink learns from tailscaled's
// log, whatever its LocalAPI call returned: a call that timed out, or that a
// drain cancelled, may still be carried out.
type removalRecord struct {
	at     time.Time
	daemon *daemon // the daemon edited; nil if there was none
	// The daemon's removals as tailscaled logged them before this one was
	// sent, and the Services the edit leaves: it took effect once a later
	// removal left no more than those. One logged before it but read after
	// the record was made, from a call carried out late, may leave more.
	removals int64
	leaves   []string
	// No edit: the daemon started drained, with nothing to remove, after a
	// removal on an earlier daemon. Any fetch of it is after the removal.
	start bool
}

// noteRemoval records that d is about to be sent an edit that leaves the
// Services leaves, or with start that d started drained after an earlier
// removal. The caller holds mu.
func (g *serviceGate) noteRemoval(d *daemon, leaves []string, start bool) {
	d.fetchMu.Lock()
	removals := d.removals
	d.fetchMu.Unlock()
	g.removal = removalRecord{
		at:       time.Now(),
		daemon:   d,
		removals: removals,
		leaves:   slices.Clone(leaves),
		start:    start,
	}
	if g.changed != nil {
		close(g.changed)
		g.changed = nil
	}
}

// same reports whether r and o are the same record.
func (r removalRecord) same(o removalRecord) bool {
	return r.at.Equal(o.at) && r.daemon == o.daemon && r.removals == o.removals &&
		r.start == o.start
}

// lastRemoval returns the removal record and a channel closed when it changes.
func (g *serviceGate) lastRemoval() (removalRecord, <-chan struct{}) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.changed == nil {
		g.changed = make(chan struct{})
	}
	return g.removal, g.changed
}

// isDrained reports whether the Service backend was drained.
func (d *daemon) isDrained() bool {
	return d.config.gate.drained.Load()
}

// parseAdvertisedEdit returns the advertised Services an editPrefsLog line
// sets, and false if it sets none.
func parseAdvertisedEdit(line string) ([]string, bool) {
	_, rest, ok := strings.Cut(line, advertiseServicesField)
	if !ok {
		return nil, false
	}
	list, _, ok := strings.Cut(rest, "]")
	if !ok {
		return nil, false
	}
	services := []string{}
	for field := range strings.FieldsSeq(list) {
		s, err := strconv.Unquote(field)
		if err != nil {
			return nil, false
		}
		services = append(services, s)
	}
	return services, true
}

// noteAdvertisedEdit notes an edit of the advertised Services that tailscaled
// applied, as it logged it. An edit that advertises a Service once the node
// is drained, as a call the drain cancelled that tailscaled carried out
// late, is undone.
func (d *daemon) noteAdvertisedEdit(services []string) {
	d.fetchMu.Lock()
	// Before the first edit, what tailscaled advertised is unknown: it may
	// have removed something
	dropped := func(s string) bool { return !slices.Contains(services, s) }
	if !d.listKnown || slices.ContainsFunc(d.listed, dropped) {
		d.removals++
		d.removedAt = d.fetches
		d.removedTo = services
	}
	d.listed, d.listKnown = services, true
	d.relisted = d.isDrained() && len(services) > 0
	relisted := d.relisted
	d.wakeLocked()
	d.fetchMu.Unlock()
	if relisted && d.ctx.Err() == nil {
		d.wg.Go(d.drainAgain)
	}
}

// removalFetched reports whether control fetched the Service list after rec's
// edit took effect, with nothing advertised again since, and returns a
// channel closed at the next fetch or edit.
func (d *daemon) removalFetched(rec removalRecord) (bool, <-chan struct{}) {
	d.fetchMu.Lock()
	defer d.fetchMu.Unlock()
	applied := rec.start || d.removals > rec.removals && within(d.removedTo, rec.leaves)
	return applied && !d.relisted && d.fetches > d.removedAt, d.nextLocked()
}

// within reports whether every Service of services is one of set.
func within(services, set []string) bool {
	return !slices.ContainsFunc(services, func(s string) bool { return !slices.Contains(set, s) })
}

// Drain stops new connections to the node's backends for all its Tailscale
// Services; existing connections continue. It stops advertising every
// Service of the node in one prefs edit, so control learns of all of them in
// one fetch and no node goes with a Service pointed at it. The backends stay
// drained: tslink does not advertise them again. A node that advertises
// nothing, as one drained before or outside tslink, is left as it is. A serve
// configuration in progress is cancelled rather than waited for.
func (d *daemon) Drain() error {
	g := d.config.gate
	g.markDrained()
	g.mu.Lock()
	defer g.mu.Unlock()
	return d.unadvertiseAll()
}

// unadvertiseAll stops advertising every Service of the node. The caller
// holds the gate's lock.
func (d *daemon) unadvertiseAll() error {
	id := stringid.TruncateID(d.config.EndpointID)
	removed, err := d.editAdvertised(d.ctx, func(string) bool { return false })
	if err != nil {
		return err
	}
	if len(removed) == 0 {
		logger.Infof("No Service is advertised for %s: nothing to drain", id)
		return nil
	}
	logger.Infof("Drained %q for %s", removed, id)
	return nil
}

// drainAgain stops advertising the Services an edit advertised once the node
// was drained.
func (d *daemon) drainAgain() {
	g := d.config.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	id := stringid.TruncateID(d.config.EndpointID)
	logger.Warnf("Endpoint %s: a Service was advertised after the drain, draining again", id)
	if err := d.unadvertiseAll(); err != nil && d.ctx.Err() == nil {
		logger.Warnf("Endpoint %s: failed to drain again: %v", id, err)
	}
}

// drainAtStart stops advertising the Services the node's state advertises, if
// the backend was drained before this daemon started, as when a drain failed
// while tailscaled restarted: tailscaled advertises them from its state.
// Should a removal on an earlier daemon have been pending, control learns of
// it from this daemon.
func (d *daemon) drainAtStart() {
	if !d.isDrained() {
		return
	}
	g := d.config.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	pending := g.removal.daemon != nil && g.removal.daemon != d
	removed, err := d.editAdvertised(d.ctx, func(string) bool { return false })
	if err != nil {
		logger.Warnf("Endpoint %s: failed to drain the restarted tailscaled: %v",
			stringid.TruncateID(d.config.EndpointID), err)
		return
	}
	if len(removed) == 0 && pending {
		g.noteRemoval(d, nil, true)
	}
}

// waitRemovalFetched waits until control fetched the Service list after rec's
// edit took effect, changed is closed, d stops or deadline passes. It
// reports whether control fetched it.
func (d *daemon) waitRemovalFetched(
	rec removalRecord,
	changed <-chan struct{},
	deadline time.Time,
) bool {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for {
		fetched, next := d.removalFetched(rec)
		if fetched {
			return true
		}
		select {
		case <-next:
		case <-changed:
			return false
		case <-timer.C:
			return false
		case <-d.ctx.Done():
			return false
		}
	}
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
// of it: until control fetched the Service list after the last removal of
// Services tslink made, at least drainAckFloor and at most drainAckTimeout
// after it. A node that tslink removed nothing from has nothing for control
// to fetch, and is not waited for. Should tailscaled restart meanwhile, or
// be restarting, the wait goes on with the daemon the supervisor starts next,
// which starts drained, until the same deadline.
func (s *DaemonSupervisor) DrainAndWait() {
	id := stringid.TruncateID(s.cfg.EndpointID)
	service := fmt.Sprintf("%q", ServiceNames(s.cfg.Services))
	drained, err := s.drain()
	if err != nil {
		logger.Warnf("Endpoint %s: failed to drain %s before leaving: %v", id, service, err)
	}

	tm := s.cfg.times()
	w := drainWait{s: s, drained: drained, deadline: time.Now().Add(tm.drainAckTimeout)}
	if rec, _ := s.cfg.gate.lastRemoval(); rec.daemon != nil {
		w.deadline = rec.at.Add(tm.drainAckTimeout)
	}
	for !w.step() {
		if !time.Now().Before(w.deadline) {
			logger.Warnf(
				"Endpoint %s: control did not fetch the drained %s within %v, leaving anyway",
				id, service, tm.drainAckTimeout)
			return
		}
	}
}

// drainWait is DrainAndWait's wait for control.
type drainWait struct {
	s        *DaemonSupervisor
	drained  *daemon // the daemon drained last
	deadline time.Time
	waiting  bool // the wait was logged
	settled  bool // a call the drain cancelled was waited out
}

// step waits for what the last removal record calls for, and reports
// whether nothing is left to wait for.
func (w *drainWait) step() bool {
	s, g := w.s, w.s.cfg.gate
	rec, changed := g.lastRemoval()
	switch {
	case rec.daemon != nil && rec.daemon.ctx.Err() == nil:
		w.log("%s drained, waiting for control to fetch it")
		return s.waitFetched(rec, changed, w.deadline)
	case s.ctx.Err() != nil:
		return true
	case s.restarting(rec):
		w.log("tailscaled is restarting, waiting for it to drain %s")
		s.waitNextDaemon(changed, w.deadline)
		return false
	case s.getDaemon() != w.drained:
		// Published since the drain, as a tailscaled restarted before the
		// node was drained: drain it
		d, err := s.drain()
		w.drained = d
		if err != nil {
			logger.Warnf("Endpoint %s: failed to drain the restarted tailscaled: %v",
				stringid.TruncateID(s.cfg.EndpointID), err)
		}
		return false
	case !w.settled && !g.lastCancel().IsZero():
		// A call the drain cancelled may still be carried out, and advertise
		// a Service again: look again once it would have been
		w.settled = true
		if wait := time.Until(g.lastCancel().Add(s.cfg.times().drainAckFloor)); wait > 0 {
			ctxtime.Sleep(s.ctx, wait)
		}
		return false
	default:
		// Nothing tslink removed is left for control to fetch
		return true
	}
}

// log logs what the wait waits for, once; format takes the Services.
func (w *drainWait) log(format string) {
	if w.waiting {
		return
	}
	w.waiting = true
	logger.Infof("Endpoint %s: "+format, stringid.TruncateID(w.s.cfg.EndpointID),
		fmt.Sprintf("%q", ServiceNames(w.s.cfg.Services)))
}

// restarting reports whether tailscaled is restarting, none being
// published or the one published having exited, and may advertise the
// Services from its state, given the last removal rec: the next one drains
// them as it starts.
func (s *DaemonSupervisor) restarting(rec removalRecord) bool {
	return !s.running() && (rec.daemon != nil || StateExists(s.cfg.StateDir))
}

// running reports whether a daemon is published whose tailscaled has not
// exited.
func (s *DaemonSupervisor) running() bool {
	d := s.getDaemon()
	return d != nil && !d.hasExited()
}

// waitFetched waits until control fetched the Service list after rec's edit,
// at least drainAckFloor after it, and reports whether it did; see
// waitRemovalFetched.
func (s *DaemonSupervisor) waitFetched(
	rec removalRecord,
	changed <-chan struct{},
	deadline time.Time,
) bool {
	if !rec.daemon.waitRemovalFetched(rec, changed, deadline) {
		return false
	}
	if wait := time.Until(rec.at.Add(s.cfg.times().drainAckFloor)); wait > 0 {
		ctxtime.Sleep(rec.daemon.ctx, wait)
	}
	// A call the drain cancelled may have advertised a Service meanwhile,
	// which a new removal undoes: wait for that one
	if again, _ := s.cfg.gate.lastRemoval(); !again.same(rec) {
		return false
	}
	if fetched, _ := rec.daemon.removalFetched(rec); !fetched {
		return false
	}
	logger.Infof("Endpoint %s: control fetched the drained %q, %v after the drain",
		stringid.TruncateID(s.cfg.EndpointID), ServiceNames(s.cfg.Services),
		time.Since(rec.at).Round(time.Millisecond))
	return true
}

// waitNextDaemon waits until the supervisor published a daemon that runs,
// the removal record changes, the supervisor stops or deadline passes.
func (s *DaemonSupervisor) waitNextDaemon(changed <-chan struct{}, deadline time.Time) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for !s.running() {
		select {
		case <-changed:
			return
		case <-ticker.C:
		case <-timer.C:
			return
		case <-s.ctx.Done():
			return
		}
	}
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
// fetch the list, up to advertiseNudges times, and then warns in the
// endpoint's status until control fetches it. A drained backend is left
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
				b.warnUnfetched(fmt.Sprintf(
					"Endpoint %s: control has not fetched the advertised %s after %d attempts: "+
						"it may route no traffic to this host until the next change",
					id, service, advertiseNudges,
				))
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
		fetches, _ := b.d.servicesFetches()
		added, err := b.d.addAdvertised(ctx, service)
		if added {
			b.markAdvertised(fetches)
		}
		return err
	})
}
