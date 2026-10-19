package tailscale

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/moby/moby/client/pkg/stringid"
	"tailscale.com/tailcfg"

	"github.com/matchory/tslink/internal/ctxtime"
	"github.com/matchory/tslink/pkg/logger"
)

// Service is a Tailscale Service the container is a backend of, and what it
// serves on it.
type Service struct {
	Name      string          // With the svc: prefix, e.g. "svc:web"
	Endpoints []ServeEndpoint // What the node serves for it
}

// serviceBackend is the node's backend for one of its container's Services:
// its serve config, advertisement and certificate. A daemon has one per
// Service; the Services of a supervisor share its serviceGate.
type serviceBackend struct {
	d         *daemon
	name      string
	endpoints []ServeEndpoint

	// Start the certificate expiry watcher at most once
	certWatch sync.Once
	// Set while the Service waits for its certificate to be configured
	certWaiting atomic.Bool
	// A check whether control approved the backend is waiting
	approvalCheck atomic.Bool
	// Set while a failed configuration is retried
	retrying atomic.Bool

	// The count of Service list fetches before tslink last added the Service
	// to the node's prefs, or noFetchAwaited
	fetchMark atomic.Int64
	// Guards controlWarned, set while the endpoint's status warns that control
	// has not fetched the Service list
	controlMu     sync.Mutex
	controlWarned bool

	// The ports of the Service's definition, read from Tailscale's API; nil
	// while unknown
	apiPorts atomic.Pointer[[]tailcfg.ProtoPortRange]
	// A failed read of the definition was logged
	apiPortsLogged atomic.Bool
}

// noFetchAwaited is a backend's fetchMark while control need not fetch the
// node's Service list: tslink has not added the Service to the prefs, as
// when the node's state advertised it already.
const noFetchAwaited = -1

// newBackends returns a backend for each of d's Services, in their order.
func newBackends(d *daemon) []*serviceBackend {
	backends := make([]*serviceBackend, 0, len(d.config.Services))
	for _, svc := range d.config.Services {
		b := &serviceBackend{d: d, name: svc.Name, endpoints: svc.Endpoints}
		b.fetchMark.Store(noFetchAwaited)
		backends = append(backends, b)
	}
	return backends
}

// start configures the backend, at once or once its certificate exists. A
// failure is retried in the background: the node keeps serving its other
// Services, and a start that failed would stop it without draining them.
func (b *serviceBackend) start() {
	err := b.configureServiceWhenCertified()
	b.noteFailure(err)
	if err != nil {
		b.retryLater()
	}
}

// retryLater configures the backend again every certPollInterval until it
// succeeds, the backend is drained or the daemon stops. One retry runs at a
// time.
func (b *serviceBackend) retryLater() {
	if !b.retrying.CompareAndSwap(false, true) {
		return
	}
	b.d.wg.Go(func() {
		defer b.retrying.Store(false)
		ctxtime.Poll(b.d.ctx, b.d.config.times().certPollInterval, func() bool {
			if b.d.ctx.Err() != nil {
				return true // the daemon is stopping
			}
			err := b.configureServiceWhenCertified()
			if errors.Is(err, errDrained) {
				return true
			}
			b.noteFailure(err)
			return err == nil
		})
	})
}

// serviceRetryWarnEvery is how often the log repeats that a Service's
// configuration keeps failing.
const serviceRetryWarnEvery = time.Hour

// noteFailure reports a failed configuration of the backend in the
// endpoint's status, and in the log at most every serviceRetryWarnEvery; nil
// clears it.
func (b *serviceBackend) noteFailure(err error) {
	key := "service/" + b.name
	if err == nil {
		b.d.warn(key, "", 0)
		return
	}
	id := stringid.TruncateID(b.d.config.EndpointID)
	logger.Debugf("Configuring %s for %s failed: %v", b.name, id, err)
	b.d.warn(key, fmt.Sprintf("Failed to configure %s for %s, retrying: %v", b.name, id, err),
		serviceRetryWarnEvery)
}

// markAdvertised notes that tslink added the Service to the node's prefs
// after fetches fetches of the Service list: until control fetches it again,
// control may not know that the node advertises the Service.
func (b *serviceBackend) markAdvertised(fetches int64) {
	b.fetchMark.Store(fetches)
}

// awaitingFetch reports whether control has not fetched the node's Service
// list since tslink last added the Service to the prefs.
func (b *serviceBackend) awaitingFetch() bool {
	mark := b.fetchMark.Load()
	fetches, _ := b.d.servicesFetches()
	return mark != noFetchAwaited && fetches <= mark
}

// warnUnfetched warns in the endpoint's status, and in the log, that control
// has not fetched the Service list, unless it has by now.
func (b *serviceBackend) warnUnfetched(message string) {
	b.controlMu.Lock()
	defer b.controlMu.Unlock()
	if !b.awaitingFetch() {
		return
	}
	b.controlWarned = true
	b.d.warn("control/"+b.name, message, 0)
}

// noteFetch clears the warning of warnUnfetched: control fetched the Service
// list.
func (b *serviceBackend) noteFetch() {
	b.controlMu.Lock()
	defer b.controlMu.Unlock()
	if b.controlWarned {
		b.controlWarned = false
		b.d.warn("control/"+b.name, "", 0)
	}
}

// ServiceNames returns the names of services.
func ServiceNames(services []Service) []string {
	names := make([]string, 0, len(services))
	for _, svc := range services {
		names = append(names, svc.Name)
	}
	return names
}
