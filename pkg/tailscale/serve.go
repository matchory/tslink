package tailscale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/client/pkg/stringid"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/tailcfg/peercap"
	"tailscale.com/util/mak"

	"github.com/matchory/tslink/internal/ctxtime"
	"github.com/matchory/tslink/pkg/logger"
)

// protoTun is the ServeEndpoint protocol that forwards all of a Service's
// traffic to the node (tailscale serve --tun).
const protoTun = "tun"

// ServeEndpoint represents a single Tailscale serve configuration.
// Configured via labels like: tslink.serve.443=https:8080/api.
type ServeEndpoint struct {
	Proto  string // http, https, tcp, tls-terminated-tcp, tun
	Port   string // External port Tailscale exposes
	Target string // Container port or address to forward to
	Path   string // L7 only - path prefix (e.g., "/api")

	ProxyProtocol string // L4 only - PROXY protocol version sent to the target ("1", "2" or "")
	AcceptAppCaps string // L7 only - comma-separated app capabilities forwarded to the target
}

// applyServeEndpoint sets ep in sc as "tailscale serve" would: as a backend
// of service, or, if service is empty, for direct serve on the node's own
// name host. mds is the tailnet's MagicDNS suffix. Direct serve does not
// support L3 (tun): it leaves sc as it is. The endpoint forwards only to the
// container's loopback.
func applyServeEndpoint(
	sc *ipn.ServeConfig,
	ep ServeEndpoint,
	service tailcfg.ServiceName,
	host, mds string,
) error {
	port, err := strconv.ParseUint(ep.Port, 10, 16)
	if err != nil {
		return fmt.Errorf("invalid external port %q: must be numeric", ep.Port)
	}
	if _, err := strconv.ParseUint(ep.Target, 10, 16); err != nil {
		return fmt.Errorf("invalid target port %q: must be numeric", ep.Target)
	}
	name := host
	if service != "" {
		// Else the config would serve it as a host name
		if err := service.Validate(); err != nil {
			return err
		}
		name = service.String()
	}
	target := "127.0.0.1:" + ep.Target

	switch ep.Proto {
	case "http", "https":
		mount, err := cleanMount(ep.Path)
		if err != nil {
			return err
		}
		if sc.IsTCPForwardingOnPort(uint16(port), service) {
			return fmt.Errorf("cannot serve web on %d: already serving TCP", port)
		}
		h := &ipn.HTTPHandler{Proxy: "http://" + target, AcceptAppCaps: appCaps(ep.AcceptAppCaps)}
		sc.SetWebHandler(h, name, uint16(port), mount, ep.Proto == "https", mds)
		return nil

	case "tcp", "tls-terminated-tcp":
		if sc.IsServingWeb(uint16(port), service) {
			return fmt.Errorf("cannot serve TCP on %d: already serving web", port)
		}
		proxyProtocol, _ := strconv.Atoi(ep.ProxyProtocol) // "" is none, labels allow 1 or 2
		tls := ep.Proto == "tls-terminated-tcp"
		if service != "" {
			sc.SetTCPForwardingForService(uint16(port), target, tls, service, proxyProtocol, mds)
		} else {
			sc.SetTCPForwarding(uint16(port), target, tls, proxyProtocol, host)
		}
		return nil

	case protoTun:
		if service == "" {
			return nil
		}
		svc := sc.Services[service]
		if svc == nil {
			svc = new(ipn.ServiceConfig)
			mak.Set(&sc.Services, service, svc)
		}
		svc.Tun = true
		return nil

	default:
		return fmt.Errorf("unsupported protocol: %s", ep.Proto)
	}
}

// cleanMount returns the mount point of an L7 endpoint's path, as "tailscale
// serve --set-path" does: "/" for none, and a path that cleaning would not
// change.
func cleanMount(p string) (string, error) {
	if p == "" {
		return "/", nil
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if c := path.Clean(p); p != c && p != c+"/" {
		return "", fmt.Errorf("invalid mount point %q", p)
	}
	return p, nil
}

// appCaps returns the app capabilities of a comma-separated list.
func appCaps(list string) []peercap.Cap {
	var caps []peercap.Cap
	for c := range strings.SplitSeq(list, ",") {
		if c = strings.TrimSpace(c); c != "" {
			caps = append(caps, peercap.Cap(c))
		}
	}
	return caps
}

// errUntaggedServiceHost is what configuring a Service backend fails with on
// a node without tags, as "tailscale serve --service" does.
var errUntaggedServiceHost = errors.New(
	"service hosts must be tagged nodes: log in with a tagged auth key or OAuth client",
)

// errNoMagicDNS is what configuring a Service that serves HTTPS fails with
// in a tailnet without MagicDNS.
var errNoMagicDNS = errors.New("tailscale status has no MagicDNS suffix: HTTPS needs MagicDNS")

// tagged reports whether st is the status of a tagged node, as a Service
// host must be.
func tagged(st *ipnstate.Status) bool {
	return st.Self != nil && st.Self.Tags != nil && st.Self.Tags.Len() > 0
}

// serveDebugLogMaxBytes is where serve-debug.log, the record of serve
// attempts, is rotated; a variable for tests.
var serveDebugLogMaxBytes int64 = 1 << 20

// checkServiceHost checks what every Service of the node needs: tags, and a
// MagicDNS suffix if any Service serves HTTPS. Its error fails the start.
func (d *daemon) checkServiceHost() error {
	ctx, cancel := context.WithTimeout(d.ctx, 10*time.Second)
	defer cancel()
	st, err := d.lc.StatusWithoutPeers(ctx)
	if err != nil {
		return fmt.Errorf("failed to read the node's status: %w", err)
	}
	if !tagged(st) {
		return errUntaggedServiceHost
	}
	for _, b := range d.backends {
		if servesHTTPS(b.endpoints) && magicDNSSuffix(st) == "" {
			return errNoMagicDNS
		}
	}
	return nil
}

// configureService configures the node as a backend of its Service, and
// advertises it. This should be called after bringUp() succeeds.
func (b *serviceBackend) configureService() error {
	return b.configureServiceBackend(true)
}

// configureServiceBackend configures the node as a backend of its Service,
// and advertises it if advertise is set, or else makes sure it is not
// advertised. A drained backend is left alone: it returns errDrained.
func (b *serviceBackend) configureServiceBackend(advertise bool) error {
	if len(b.endpoints) == 0 {
		logger.Warnf("Service %s configured but no endpoints defined", b.name)
		return nil
	}

	if b.d.isDrained() {
		logger.Infof("Not configuring %s for %s: drained", b.name, b.d.config.EndpointID)
		return errDrained
	}
	return b.d.config.gate.change(b.d, func(ctx context.Context) error {
		logger.Infof("Configuring Tailscale Service %s with %d endpoint(s) for %s",
			b.name, len(b.endpoints), b.d.config.EndpointID)

		service := tailcfg.ServiceName(b.name)
		err := b.d.editServe(ctx, "service", func(sc *ipn.ServeConfig, st *ipnstate.Status) error {
			if !tagged(st) {
				return errUntaggedServiceHost
			}
			return applyServeEndpoints(sc, b.endpoints, service, "", magicDNSSuffix(st))
		})
		// A drain cancels the change in progress and waits for this to
		// return: run nothing more once it began
		if b.d.isDrained() {
			return errDrained
		}
		if err != nil {
			return err
		}
		if !advertise {
			_, err := b.d.editAdvertised(ctx, func(s string) bool { return s != b.name })
			return err
		}
		return b.runAdvertise(ctx, b.name)
	})
}

// applyServeEndpoints applies each endpoint to sc, with applyServeEndpoint.
func applyServeEndpoints(
	sc *ipn.ServeConfig,
	endpoints []ServeEndpoint,
	service tailcfg.ServiceName,
	host, mds string,
) error {
	for i, ep := range endpoints {
		if err := applyServeEndpoint(sc, ep, service, host, mds); err != nil {
			return fmt.Errorf("failed to configure endpoint %d (%s:%s): %w",
				i, ep.Proto, ep.Port, err)
		}
	}
	return nil
}

// errServeUnchanged, returned by an editServe edit, leaves the serve config
// as it is: there is nothing to write.
var errServeUnchanged = errors.New("serve config unchanged")

// editServe changes tailscaled's serve config with edit, as "tailscale serve"
// does: it reads the config and the node's status, and writes the config
// back, unless another client changed it meanwhile. what names the change in
// serve-debug.log.
func (d *daemon) editServe(
	ctx context.Context,
	what string,
	edit func(*ipn.ServeConfig, *ipnstate.Status) error,
) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := func() error {
		st, err := d.lc.StatusWithoutPeers(ctx)
		if err != nil {
			return fmt.Errorf("failed to read the node's status: %w", err)
		}
		sc, err := d.lc.GetServeConfig(ctx)
		if err != nil {
			return fmt.Errorf("failed to read the serve config: %w", err)
		}
		if sc == nil {
			sc = new(ipn.ServeConfig)
		}
		err = edit(sc, st)
		if errors.Is(err, errServeUnchanged) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := d.lc.SetServeConfig(ctx, sc); err != nil {
			return fmt.Errorf("failed to set the serve config: %w", err)
		}
		config, err := json.Marshal(sc)
		if err != nil {
			return fmt.Errorf("failed to encode the serve config: %w", err)
		}
		d.serveDebugf("SUCCESS %s: %s\n", what, config)
		return nil
	}()
	if err != nil {
		d.serveDebugf("FAILED %s: %v\n", what, err)
	}
	return err
}

// addAdvertised adds service to the Services the node's prefs advertise, as
// "tailscale serve advertise" does.
func (d *daemon) addAdvertised(ctx context.Context, service string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	prefs, err := d.lc.GetPrefs(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to advertise %s: %w", service, err)
	}
	if slices.Contains(prefs.AdvertiseServices, service) {
		return false, nil
	}
	if _, err := d.lc.EditPrefs(ctx, &ipn.MaskedPrefs{
		AdvertiseServicesSet: true,
		AdvertiseServices:    append(slices.Clone(prefs.AdvertiseServices), service),
	}); err != nil {
		return false, fmt.Errorf("failed to advertise %s: %w", service, err)
	}
	return true, nil
}

// editAdvertised stops advertising every Service of the node's prefs keep
// rejects, in one prefs edit, and returns those it removed.
func (d *daemon) editAdvertised(ctx context.Context, keep func(string) bool) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	prefs, err := d.lc.GetPrefs(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read the advertised Services: %w", err)
	}
	var kept, removed []string
	for _, s := range prefs.AdvertiseServices {
		if keep(s) {
			kept = append(kept, s)
		} else {
			removed = append(removed, s)
		}
	}
	if len(removed) == 0 {
		return nil, nil
	}
	if _, err := d.lc.EditPrefs(ctx, &ipn.MaskedPrefs{
		AdvertiseServicesSet: true,
		AdvertiseServices:    kept,
	}); err != nil {
		return nil, fmt.Errorf("failed to unadvertise %q: %w", removed, err)
	}
	return removed, nil
}

// pruneServices removes from the serve config and the prefs every Service the
// container does not name, as one its labels no longer have: the node would
// keep advertising it, and never drain it. A failure is logged, and noted in
// the gate, as is a removal from the prefs: the node is then drained before
// it goes, and waits for control to fetch the change.
func (d *daemon) pruneServices() {
	names := ServiceNames(d.config.Services)
	keep := func(s string) bool { return slices.Contains(names, s) }
	id := stringid.TruncateID(d.config.EndpointID)
	g := d.config.gate
	err := g.change(d, func(ctx context.Context) error {
		err := d.editServe(ctx, "prune", func(sc *ipn.ServeConfig, _ *ipnstate.Status) error {
			var stale []tailcfg.ServiceName
			for name := range sc.Services {
				if !keep(string(name)) {
					stale = append(stale, name)
				}
			}
			if len(stale) == 0 {
				return errServeUnchanged
			}
			for _, name := range stale {
				logger.Infof(
					"Removing %s, which its container does not name, from the serve config of %s",
					name, id)
				delete(sc.Services, name)
			}
			return nil
		})
		if err != nil {
			return err
		}
		fetches, _ := d.servicesFetches()
		removed, err := d.editAdvertised(ctx, keep)
		if len(removed) > 0 {
			logger.Infof("Stopped advertising %q, which its container does not name, for %s",
				removed, id)
			// The caller holds the gate's lock
			g.prune = drainRecord{at: time.Now(), daemon: d, fetches: fetches}
			g.advertised.Store(true)
		}
		return err
	})
	g.unpruned.Store(err != nil && !errors.Is(err, errDrained))
	if g.unpruned.Load() {
		logger.Warnf("Failed to remove stale Services of %s: %v", id, err)
	}
}

// runAdvertise advertises the Service and notes that the backend was
// advertised. The caller holds the gate's lock.
func (b *serviceBackend) runAdvertise(ctx context.Context, service string) error {
	fetches, _ := b.d.servicesFetches()
	added, err := b.d.addAdvertised(ctx, service)
	if err != nil {
		return err
	}
	b.d.config.gate.advertised.Store(true)
	if added {
		b.markAdvertised(fetches)
		// ctx ends with this change; the check runs on the daemon's
		b.confirmAdvertisedLater(service, fetches) //nolint:contextcheck // see above
	}
	b.checkApprovalLater() //nolint:contextcheck // as is the approval check
	return nil
}

// noteSurvivingState marks the backend advertised if the node's state exists
// before tailscaled starts: it may advertise the Service from a previous run.
func (d *daemon) noteSurvivingState() {
	if StateExists(d.config.StateDir) {
		d.config.gate.advertised.Store(true)
	}
}

// advertise advertises the Service, unless the backend was drained. A
// drained service stays drained in the node's state, as after a plugin
// restart: advertise it again once its endpoints are configured.
func (b *serviceBackend) advertise(service string) error {
	return b.d.config.gate.change(b.d, func(ctx context.Context) error {
		return b.runAdvertise(ctx, service)
	})
}

// configureDirectServe configures direct machine serve, without a Service.
// This makes the container accessible via its MagicDNS hostname (e.g.,
// https://hostname.tailnet.ts.net/) with automatic TLS certificates.
func (d *daemon) configureDirectServe() error {
	if len(d.config.Endpoints) == 0 {
		return nil // No endpoints to configure
	}

	logger.Infof("Configuring direct machine serve with %d endpoint(s) for %s",
		len(d.config.Endpoints), d.config.EndpointID)
	if servesWeb(d.config.Endpoints) {
		logger.Warnf(
			"Endpoint %s serves HTTP on its own name %s: tailscaled issues a certificate for it, "+
				"even for plain HTTP, and every new task's name counts against Let's Encrypt's weekly limit for the tailnet",
			stringid.TruncateID(d.config.EndpointID),
			d.config.Hostname,
		)
	}

	err := d.editServe(d.ctx, "direct", func(sc *ipn.ServeConfig, st *ipnstate.Status) error {
		host := ""
		if st.Self != nil {
			host = strings.TrimSuffix(st.Self.DNSName, ".")
		}
		if host == "" {
			return errors.New("the node has no DNS name yet")
		}
		return applyServeEndpoints(sc, d.config.Endpoints, "", host, magicDNSSuffix(st))
	})
	if err != nil {
		return fmt.Errorf("direct serve failed: %w", err)
	}
	logger.Infof("Direct serve configured for %s", d.config.EndpointID)
	return nil
}

// checkApprovalLater notes when control has not approved the backend yet, as
// right after a start, and warns if it still has not after approvalGrace. A
// Service that does not exist looks the same: control gives the node no
// addresses for it. Each check reads the ports of the Service's definition
// from Tailscale's API, if it can. One check runs at a time.
func (b *serviceBackend) checkApprovalLater() {
	if !b.approvalCheck.CompareAndSwap(false, true) {
		return
	}
	grace := b.d.config.times().approvalGrace
	awaiting := func() bool {
		return b.d.readinessReport(b.d.ctx).Services[b.name] == ReadyAwaitingApproval
	}
	b.d.wg.Go(func() {
		defer b.approvalCheck.Store(false)
		b.refreshDefinedPorts()
		if !awaiting() {
			return
		}
		logger.Infof("Waiting for control to approve %s as a backend of %s",
			stringid.TruncateID(b.d.config.EndpointID), b.name)
		if !ctxtime.Sleep(b.d.ctx, grace) {
			return
		}
		b.refreshDefinedPorts()
		if awaiting() {
			logger.Warnf(
				"Service backend registered but pending admin approval after %v, "+
					"or the Service is not defined in the admin console: %s",
				grace,
				b.name,
			)
		}
	})
}

// serveDebugf appends a message to serve-debug.log in the state directory,
// which keeps a record of the serve attempts for debugging.
func (d *daemon) serveDebugf(format string, args ...any) {
	path := filepath.Join(d.config.StateDir, "serve-debug.log")
	f, err := logger.OpenRotating(path, serveDebugLogMaxBytes)
	if err != nil {
		logger.Warnf("Failed to open serve debug file: %v", err)
		return
	}
	if _, err := fmt.Fprintf(f, format, args...); err != nil {
		logger.Warnf("Failed to write serve debug file: %v", err)
	}
	if err := f.Close(); err != nil {
		logger.Warnf("Failed to close serve debug file: %v", err)
	}
}
