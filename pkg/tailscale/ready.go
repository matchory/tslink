package tailscale

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"tailscale.com/tailcfg"
)

// Readiness is how far a node is from serving what its container asks for.
type Readiness string

// The readiness states, from a node not started to one serving all it should.
const (
	ReadyStarting           Readiness = "starting"            // tailscaled is not up yet
	ReadyFailed             Readiness = "failed"              // starting failed for good
	ReadyLoggedOut          Readiness = "logged-out"          // the node needs a login
	ReadyServingPending     Readiness = "serving-pending"     // the tslink.serve.* config is not applied
	ReadyCertificatePending Readiness = "certificate-pending" // the Service waits for its certificate
	ReadyAwaitingApproval   Readiness = "awaiting-approval"   // control has not approved the backend
	ReadyDrained            Readiness = "drained"             // the backend is drained, or the container leaving
	Ready                   Readiness = "ready"
)

// serviceHostCap is the node capability whose values map the VIP Services
// control approved the node to host to their addresses
// (tailcfg.NodeAttrServiceHost).
const serviceHostCap = "service-host"

// readiness asks tailscaled how far the node is from serving what its
// container asks for: running, its serve config applied and, for a Service,
// advertised and approved by control as a backend.
func (d *Daemon) readiness(ctx context.Context) Readiness {
	if d.isDrained() {
		return ReadyDrained
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	status, err := d.lc.StatusWithoutPeers(ctx)
	if err != nil {
		return ReadyStarting
	}
	switch {
	case status.BackendState == needsLogin || d.nodeNotFoundRecently():
		return ReadyLoggedOut
	case status.BackendState != "Running":
		return ReadyStarting
	}

	service, direct := d.serveApplied(ctx)
	if !service || (d.config.Service != "" && !d.advertised(ctx)) {
		return d.pendingService(magicDNSSuffix(status))
	}
	if !direct {
		return ReadyServingPending
	}
	if d.config.Service == "" {
		return Ready
	}
	if status.Self == nil {
		return ReadyAwaitingApproval
	}
	for _, raw := range status.Self.CapMap[serviceHostCap] {
		var services map[string]json.RawMessage
		if json.Unmarshal([]byte(raw), &services) == nil && services[d.config.Service] != nil {
			return Ready
		}
	}
	return ReadyAwaitingApproval
}

// pendingService tells a Service waiting for its certificate from other
// serve config not applied yet: the certificate lease holder configures the
// Service only to issue the certificate, and the others wait for it.
func (d *Daemon) pendingService(suffix string) Readiness {
	if d.config.Service == "" || !servesHTTPS(d.config.Endpoints) || suffix == "" {
		return ReadyServingPending
	}
	domain := d.config.Service[len("svc:"):] + "." + suffix
	if !validCert(filepath.Join(d.config.StateDir, certsDirName), domain, time.Now()) {
		return ReadyCertificatePending
	}
	return ReadyServingPending
}

// serveApplied reports whether tailscaled's serve config holds every port the
// container asks for under its Service, if it has one, and on the node itself
// for direct serve.
func (d *Daemon) serveApplied(ctx context.Context) (bool, bool) {
	if len(d.config.Endpoints) == 0 {
		return true, true
	}
	config, err := d.lc.GetServeConfig(ctx)
	if err != nil {
		return false, false
	}
	service, direct := true, true
	for _, ep := range d.config.Endpoints {
		port, err := strconv.ParseUint(ep.Port, 10, 16)
		if err != nil {
			return false, false
		}
		if d.config.Service != "" {
			svc := config.Services[tailcfg.ServiceName(d.config.Service)]
			if svc == nil || (ep.Proto == protoTun && !svc.Tun) ||
				(ep.Proto != protoTun && svc.TCP[uint16(port)] == nil) {
				service = false
			}
		}
		if d.config.Direct && ep.Proto != protoTun && config.TCP[uint16(port)] == nil {
			direct = false
		}
	}
	return service, direct
}

// advertised reports whether the node's prefs advertise its Service.
func (d *Daemon) advertised(ctx context.Context) bool {
	prefs, err := d.lc.GetPrefs(ctx)
	return err == nil && slices.Contains(prefs.AdvertiseServices, d.config.Service)
}
