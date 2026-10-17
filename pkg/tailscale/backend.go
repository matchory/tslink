package tailscale

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

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

// ServiceNames returns the names of services.
func ServiceNames(services []Service) []string {
	names := make([]string, 0, len(services))
	for _, svc := range services {
		names = append(names, svc.Name)
	}
	return names
}
