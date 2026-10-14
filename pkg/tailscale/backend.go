package tailscale

import (
	"sync"
	"sync/atomic"
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
}

// newBackends returns a backend for each of d's Services, in their order.
func newBackends(d *daemon) []*serviceBackend {
	backends := make([]*serviceBackend, 0, len(d.config.Services))
	for _, svc := range d.config.Services {
		backends = append(backends, &serviceBackend{d: d, name: svc.Name, endpoints: svc.Endpoints})
	}
	return backends
}

// serviceNames returns the names of services.
func serviceNames(services []Service) []string {
	names := make([]string, 0, len(services))
	for _, svc := range services {
		names = append(names, svc.Name)
	}
	return names
}
