package tailscale

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/util/mak"
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

// ReadinessReport is how far a node is from serving what its container asks
// for, and how far each of its Services is.
type ReadinessReport struct {
	State    Readiness
	Services map[string]Readiness // By Service name; nil without Services
}

// readinessRank orders the states a Service can be in, least ready first.
var readinessRank = map[Readiness]int{
	ReadyCertificatePending: 0,
	ReadyServingPending:     1,
	ReadyAwaitingApproval:   2,
	Ready:                   3,
}

// lessReady returns the less ready of a and b.
func lessReady(a, b Readiness) Readiness {
	if readinessRank[b] < readinessRank[a] {
		return b
	}
	return a
}

// readiness returns how far the node is from serving what its container
// asks for.
func (d *daemon) readiness(ctx context.Context) Readiness {
	return d.readinessReport(ctx).State
}

// readinessReport asks tailscaled how far the node is from serving what its
// container asks for: running, its serve config applied and, for each
// Service, advertised and approved by control as a backend. The node is as
// ready as its least ready Service. It reads the status, the serve config and
// the prefs once each.
func (d *daemon) readinessReport(ctx context.Context) ReadinessReport {
	if d.isDrained() {
		return ReadinessReport{State: ReadyDrained}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	status, err := d.lc.StatusWithoutPeers(ctx)
	if err != nil {
		return ReadinessReport{State: ReadyStarting}
	}
	switch {
	case status.BackendState == needsLogin || d.nodeNotFoundRecently():
		return ReadinessReport{State: ReadyLoggedOut}
	case status.BackendState != "Running":
		return ReadinessReport{State: ReadyStarting}
	}

	direct := d.config.Direct && len(d.config.Endpoints) > 0
	var config *ipn.ServeConfig
	if direct || len(d.backends) > 0 {
		config, _ = d.lc.GetServeConfig(ctx) // nil if unreadable: nothing applied
	}
	var prefs *ipn.Prefs
	if len(d.backends) > 0 {
		prefs, _ = d.lc.GetPrefs(ctx) // nil if unreadable: nothing advertised
	}

	report := ReadinessReport{State: Ready}
	if direct && !applied(config, "", d.config.Endpoints) {
		report.State = ReadyServingPending
	}
	for _, b := range d.backends {
		state := b.readiness(status, config, prefs)
		mak.Set(&report.Services, b.name, state)
		report.State = lessReady(report.State, state)
	}
	return report
}

// readiness tells how far the backend is from serving its Service, from the
// node's status, serve config and prefs.
func (b *serviceBackend) readiness(
	status *ipnstate.Status,
	config *ipn.ServeConfig,
	prefs *ipn.Prefs,
) Readiness {
	if !applied(config, tailcfg.ServiceName(b.name), b.endpoints) ||
		prefs == nil || !slices.Contains(prefs.AdvertiseServices, b.name) {
		return b.pendingService(magicDNSSuffix(status))
	}
	if status.Self == nil {
		return ReadyAwaitingApproval
	}
	for _, raw := range status.Self.CapMap[serviceHostCap] {
		var services map[string]json.RawMessage
		if json.Unmarshal([]byte(raw), &services) == nil && services[b.name] != nil {
			return Ready
		}
	}
	return ReadyAwaitingApproval
}

// pendingService tells a Service waiting for its certificate from other
// serve config not applied yet: the certificate lease holder configures the
// Service only to issue the certificate, and the others wait for it.
func (b *serviceBackend) pendingService(suffix string) Readiness {
	if !servesHTTPS(b.endpoints) || suffix == "" {
		return ReadyServingPending
	}
	domain := b.serviceDomainIn(suffix)
	if !validCert(filepath.Join(b.d.config.StateDir, certsDirName), domain, time.Now()) {
		return ReadyCertificatePending
	}
	return ReadyServingPending
}

// applied reports whether config serves every port of endpoints, under the
// Service service, or on the node itself if service is empty.
func applied(config *ipn.ServeConfig, service tailcfg.ServiceName, endpoints []ServeEndpoint) bool {
	if len(endpoints) == 0 {
		return true
	}
	if config == nil {
		return false
	}
	for _, ep := range endpoints {
		port, err := strconv.ParseUint(ep.Port, 10, 16)
		if err != nil {
			return false
		}
		if service == "" {
			if ep.Proto != protoTun && config.TCP[uint16(port)] == nil {
				return false
			}
			continue
		}
		svc := config.Services[service]
		if svc == nil || (ep.Proto == protoTun && !svc.Tun) ||
			(ep.Proto != protoTun && svc.TCP[uint16(port)] == nil) {
			return false
		}
	}
	return true
}
