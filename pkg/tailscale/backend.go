package tailscale

import (
	"errors"
	"sync"
	"sync/atomic"

	"github.com/moby/moby/client/pkg/stringid"

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
}

// newBackends returns a backend for each of d's Services, in their order.
func newBackends(d *daemon) []*serviceBackend {
	backends := make([]*serviceBackend, 0, len(d.config.Services))
	for _, svc := range d.config.Services {
		backends = append(backends, &serviceBackend{d: d, name: svc.Name, endpoints: svc.Endpoints})
	}
	return backends
}

// start configures the backend, at once or once its certificate exists. A
// failure is retried in the background: the node keeps serving its other
// Services, and a start that failed would stop it without draining them.
func (b *serviceBackend) start() {
	if err := b.configureServiceWhenCertified(); err != nil {
		logger.Warnf("Failed to configure %s for %s, retrying: %v",
			b.name, stringid.TruncateID(b.d.config.EndpointID), err)
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
			if err == nil || errors.Is(err, errDrained) {
				return true
			}
			logger.Debugf("Configuring %s for %s failed again: %v",
				b.name, stringid.TruncateID(b.d.config.EndpointID), err)
			return false
		})
	})
}

// ServiceNames returns the names of services.
func ServiceNames(services []Service) []string {
	names := make([]string, 0, len(services))
	for _, svc := range services {
		names = append(names, svc.Name)
	}
	return names
}
