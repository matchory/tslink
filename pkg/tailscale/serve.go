package tailscale

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/matchory/tslink/pkg/logger"
)

// ServeEndpoint represents a single Tailscale serve configuration.
type ServeEndpoint struct {
	Proto  string // http, https, tcp, tls-terminated-tcp, tun
	Port   string // External port Tailscale exposes
	Target string // Container port or address to forward to
	Path   string // L7 only - path prefix (e.g., "/api")

	ProxyProtocol string // L4 only - PROXY protocol version sent to the target ("1", "2" or "")
	AcceptAppCaps string // L7 only - comma-separated app capabilities forwarded to the target
}

// serveOptionArgs returns the "tailscale serve" flags for the endpoint's options.
func serveOptionArgs(ep ServeEndpoint) []string {
	var args []string
	if ep.ProxyProtocol != "" {
		args = append(args, "--proxy-protocol="+ep.ProxyProtocol)
	}
	if ep.AcceptAppCaps != "" {
		args = append(args, "--accept-app-caps="+ep.AcceptAppCaps)
	}
	return args
}

// serveArgs returns the "tailscale serve" arguments, after --socket, that
// configure ep as a backend of service, or for direct machine serve if service
// is empty. Direct serve does not support L3 (tun): it returns nil args.
func serveArgs(ep ServeEndpoint, service string) ([]string, error) {
	// Validate port is numeric
	if _, err := strconv.Atoi(ep.Port); err != nil {
		return nil, fmt.Errorf("invalid external port %q: must be numeric", ep.Port)
	}
	if _, err := strconv.Atoi(ep.Target); err != nil {
		return nil, fmt.Errorf("invalid target port %q: must be numeric", ep.Target)
	}

	// Direct serve runs in the background; service mode does so by itself
	args := []string{serveCmd, serveBackground}
	if service != "" {
		args = []string{serveCmd, "--service=" + service}
	}

	switch ep.Proto {
	case "http", "https":
		// L7: --https=443 [--set-path=/api] http://127.0.0.1:8080, or
		// 127.0.0.1:8080 for a service (per Tailscale docs, just host:port)
		args = append(args, "--"+ep.Proto+"="+ep.Port)
		if ep.Path != "" {
			args = append(args, "--set-path="+ep.Path)
		}
		args = append(args, serveOptionArgs(ep)...)
		if service == "" {
			return append(args, "http://127.0.0.1:"+ep.Target), nil
		}
		return append(args, "127.0.0.1:"+ep.Target), nil

	case "tcp", "tls-terminated-tcp":
		// L4, optionally with TLS termination: --tcp=5432 tcp://127.0.0.1:5432
		args = append(args, "--"+ep.Proto+"="+ep.Port)
		args = append(args, serveOptionArgs(ep)...)
		return append(args, "tcp://127.0.0.1:"+ep.Target), nil

	case "tun":
		// L3: --tun, for services only
		if service == "" {
			return nil, nil
		}
		return append(args, "--tun"), nil

	default:
		return nil, fmt.Errorf("unsupported protocol: %s", ep.Proto)
	}
}

// Arguments of the "tailscale serve" commands the daemon runs.
const (
	serveCmd        = "serve"
	serveBackground = "--bg"
)

// untaggedServiceHostError is what "tailscale serve --service" fails with on
// a node without tags.
const untaggedServiceHostError = "service hosts must be tagged nodes"

// serveDebugLogMaxBytes is where serve-debug.log, the record of serve
// attempts, is rotated; a variable for tests.
var serveDebugLogMaxBytes int64 = 1 << 20

// configureService runs "tailscale serve" to register as a service backend,
// and advertises it. This should be called after bringUp() succeeds.
func (d *Daemon) configureService() error {
	return d.configureServiceBackend(true)
}

// configureServiceBackend configures the node as a backend of its Service,
// and advertises it if advertise is set. A drained backend is left alone: it
// returns errDrained.
func (d *Daemon) configureServiceBackend(advertise bool) error {
	if d.config.Service == "" {
		return nil // No service configured, skip
	}

	if len(d.config.Endpoints) == 0 {
		logger.Warnf("Service %s configured but no endpoints defined", d.config.Service)
		return nil
	}

	g := d.config.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	if d.isDrained() {
		logger.Infof("Not configuring %s for %s: drained", d.config.Service, d.config.EndpointID)
		return errDrained
	}

	logger.Infof("Configuring Tailscale Service %s with %d endpoint(s) for %s",
		d.config.Service, len(d.config.Endpoints), d.config.EndpointID)

	// Configure each endpoint
	for i, ep := range d.config.Endpoints {
		if err := d.configureServeEndpoint(ep); err != nil {
			return fmt.Errorf("failed to configure endpoint %d (%s:%s): %w",
				i, ep.Proto, ep.Port, err)
		}
	}

	if !advertise {
		// "tailscale serve --service" advertises the Service by itself
		return d.runServe(context.Background(), "serve-unadvertise", "drain", d.config.Service)
	}
	return d.runServe(context.Background(), "serve-advertise", "advertise", d.config.Service)
}

// advertise runs "tailscale serve advertise", unless the backend was drained.
// A drained service stays drained in the node's state, as after a plugin
// restart: advertise it again once its endpoints are configured.
func (d *Daemon) advertise(service string) error {
	g := d.config.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	if d.isDrained() {
		return errDrained
	}
	return d.runServe(context.Background(), "serve-advertise", "advertise", service)
}

// configureDirectServe runs "tailscale serve" without --service flag to configure
// direct machine serve. This makes the container accessible via its MagicDNS hostname
// (e.g., https://hostname.tailnet.ts.net/) with automatic TLS certificates.
func (d *Daemon) configureDirectServe() error {
	if len(d.config.Endpoints) == 0 {
		return nil // No endpoints to configure
	}

	logger.Infof("Configuring direct machine serve with %d endpoint(s) for %s",
		len(d.config.Endpoints), d.config.EndpointID)
	if servesWeb(d.config.Endpoints) {
		logger.Warnf(
			"Endpoint %s serves HTTP on its own name %s: tailscaled issues a certificate for it, "+
				"even for plain HTTP, and every new task's name counts against Let's Encrypt's weekly limit for the tailnet",
			d.config.EndpointID[:12],
			d.config.Hostname,
		)
	}

	// Configure each endpoint for direct serve
	for i, ep := range d.config.Endpoints {
		if err := d.configureDirectServeEndpoint(ep); err != nil {
			return fmt.Errorf("failed to configure direct endpoint %d (%s:%s): %w",
				i, ep.Proto, ep.Port, err)
		}
	}

	return nil
}

// configureDirectServeEndpoint configures a single direct serve endpoint (without --service).
func (d *Daemon) configureDirectServeEndpoint(ep ServeEndpoint) error {
	logger.Debugf("configureDirectServeEndpoint: proto=%s port=%s target=%s path=%s",
		ep.Proto, ep.Port, ep.Target, ep.Path)

	args, err := serveArgs(ep, "")
	if err != nil {
		return err
	}
	if args == nil {
		// L3: not applicable for direct serve without service
		logger.Debugf("Skipping L3 (tun) endpoint for direct serve - only supported with services")
		return nil
	}
	args = append([]string{"--socket=" + d.socketPath}, args...)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Use streaming to see output as it arrives
	prefix := fmt.Sprintf("direct-serve:%s:%s", ep.Proto, ep.Port)
	output, err := d.tailscale(ctx, prefix, args...)
	if err != nil {
		logger.Errorf("tailscale serve (direct) failed: %v", err)
		return fmt.Errorf("tailscale serve (direct) failed: %w (output: %s)", err, output)
	}

	logger.Infof("Direct serve endpoint %s:%s configured", ep.Proto, ep.Port)
	return nil
}

// configureServeEndpoint configures a single serve endpoint for a service backend.
// Supports L3 (tun), L4 (tcp, tls-terminated-tcp), and L7 (http, https).
func (d *Daemon) configureServeEndpoint(ep ServeEndpoint) error {
	logger.Debugf("configureServeEndpoint: proto=%s port=%s target=%s path=%s service=%s",
		ep.Proto, ep.Port, ep.Target, ep.Path, d.config.Service)

	args, err := serveArgs(ep, d.config.Service)
	if err != nil {
		return err
	}
	if ep.Proto == "tun" {
		logger.Warnf("L3 (tun) endpoints require additional iptables configuration")
	}
	args = append([]string{"--socket=" + d.socketPath}, args...)

	d.serveDebugf("Running: %s %v\n", d.config.TailscaleBin, args)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Use streaming to see output as it arrives
	prefix := fmt.Sprintf("serve:%s:%s", ep.Proto, ep.Port)
	output, err := d.tailscale(ctx, prefix, args...)
	if err != nil {
		d.serveDebugf("FAILED: %v\nOutput: %s\n", err, output)
		logger.Errorf("tailscale serve failed: %v", err)

		if strings.Contains(output, "service not found") ||
			strings.Contains(output, "unknown service") {
			return fmt.Errorf("service %s not found: create it in Tailscale admin console first",
				d.config.Service)
		}
		if strings.Contains(output, untaggedServiceHostError) {
			return fmt.Errorf(
				"tailscale serve failed: requires tagged auth key (output: %s)",
				output,
			)
		}

		return fmt.Errorf("tailscale serve failed: %w (output: %s)", err, output)
	}

	// Check for approval pending (command succeeds but backend not active yet)
	if strings.Contains(output, "approval from an admin is required") {
		logger.Warnf("Service backend registered but pending admin approval: %s", d.config.Service)
	}

	d.serveDebugf("SUCCESS\nOutput: %s\n", output)
	logger.Infof("tailscale serve endpoint %s:%s configured", ep.Proto, ep.Port)
	return nil
}

// serveDebugf appends a message to serve-debug.log in the state directory,
// which keeps a record of the serve attempts for debugging.
func (d *Daemon) serveDebugf(format string, args ...any) {
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

// ConfigureServeEndpoints configures multiple Tailscale serve endpoints after startup.
// This is called when container info is obtained from cache after initial Join.
func (d *Daemon) ConfigureServeEndpoints(
	service string,
	endpoints []ServeEndpoint,
	tags []string,
	direct bool,
) error {
	logger.Infof("Late-configuring serve endpoints: service=%s endpoints=%d direct=%v for %s",
		service, len(endpoints), direct, d.config.EndpointID)

	// Update tags if provided
	if len(tags) > 0 {
		tagsArg := strings.Join(tags, ",")
		args := []string{
			"--socket=" + d.socketPath,
			"set",
			"--advertise-tags=" + tagsArg,
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if output, err := d.tailscale(ctx, "set-tags", args...); err != nil {
			logger.Warnf("Failed to set tags: %v (output: %s)", err, output)
		}
	}

	// Store config for endpoint configuration
	d.config.Service = service
	d.config.Endpoints = endpoints
	d.config.Direct = direct

	// Configure direct serve first (fast access via machine hostname)
	if direct && len(endpoints) > 0 {
		for i, ep := range endpoints {
			if err := d.configureDirectServeEndpoint(ep); err != nil {
				return fmt.Errorf("failed to configure direct endpoint %d (%s:%s): %w",
					i, ep.Proto, ep.Port, err)
			}
		}
		logger.Infof("Late direct serve configuration completed")
	}

	// Configure service backend if specified
	if service != "" {
		switch err := d.configureService(); {
		case errors.Is(err, errDrained):
		case err != nil:
			return err
		default:
			logger.Infof("Late service configuration completed")
		}
	}

	return nil
}
