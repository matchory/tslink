package docker

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/moby/moby/api/types/events"
	dockerclient "github.com/moby/moby/client"
	"golang.org/x/sys/unix"

	"github.com/matchory/tslink/pkg/core"
	"github.com/matchory/tslink/pkg/logger"
)

// ContainerCache caches container info for quick lookup during Join().
type ContainerCache struct {
	mu sync.RWMutex
	// Map EndpointID -> ContainerInfo
	byEndpoint map[string]*core.ContainerInfo
}

// NewContainerCache creates a new container cache.
func NewContainerCache() *ContainerCache {
	return &ContainerCache{
		byEndpoint: make(map[string]*core.ContainerInfo),
	}
}

// Store stores container info keyed by endpoint ID.
func (c *ContainerCache) Store(endpointID string, info *core.ContainerInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byEndpoint[endpointID] = info
	logger.Infof(
		"ContainerCache: stored info for endpoint %s (hostname=%s service=%s endpoints=%d direct=%v)",
		endpointID[:12],
		info.Hostname,
		info.Service,
		len(info.Endpoints),
		info.Direct,
	)
}

// GetByEndpoint retrieves container info by endpoint ID.
func (c *ContainerCache) GetByEndpoint(endpointID string) (*core.ContainerInfo, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	info, ok := c.byEndpoint[endpointID]
	return info, ok
}

// Delete removes container info by endpoint ID.
func (c *ContainerCache) Delete(endpointID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.byEndpoint, endpointID)
}

// ContainerInfoCallback is called when container info is stored in the cache.
// The driver uses this to trigger Tailscale setup.
type ContainerInfoCallback func(endpointID string, info *core.ContainerInfo)

// ContainerStopCallback is called when Docker sends a container its stop
// signal, before the container exits.
type ContainerStopCallback func(containerID string)

// WatchEvents watches Docker events and caches container info.
// When container info is stored, the callback is invoked to trigger Tailscale setup.
// The context controls the lifecycle - when cancelled, the watcher stops.
// ownsNetwork reports whether a network ID belongs to this driver.
// onStop, if set, is called when a container is sent its stop signal.
func WatchEvents(
	ctx context.Context,
	cache *ContainerCache,
	ownsNetwork func(id string) bool,
	onInfo ContainerInfoCallback,
	onStop ContainerStopCallback,
) {
	logger.Infof("Starting Docker event watcher")

	cli, err := dockerclient.New(dockerclient.FromEnv)
	if err != nil {
		logger.Errorf("Failed to create Docker client for event watching: %v", err)
		return
	}
	defer cli.Close()

	logger.Debugf("Docker client created, starting event stream")

	// Watch for network connects, and for kills to see containers stopping
	filterArgs := dockerclient.Filters{}.
		Add("type", "network", "container").
		Add("event", "connect", "kill")

	result := cli.Events(ctx, dockerclient.EventsListOptions{
		Filters: filterArgs,
	})

	logger.Debugf("Event stream started, waiting for events...")

	for {
		select {
		case <-ctx.Done():
			logger.Infof("Event watcher shutting down")
			return

		case err := <-result.Err:
			if err != nil {
				// Check if context is cancelled before reconnecting
				if ctx.Err() != nil {
					logger.Infof("Event watcher context cancelled, stopping")
					return
				}

				logger.Warnf("Event stream error: %v, reconnecting in 1s...", err)

				// Backoff before reconnecting to avoid tight loop
				select {
				case <-ctx.Done():
					logger.Infof("Event watcher context cancelled during backoff")
					return
				case <-time.After(1 * time.Second):
				}

				// Reconnect after backoff
				result = cli.Events(ctx, dockerclient.EventsListOptions{
					Filters: filterArgs,
				})
			}

		case msg := <-result.Messages:
			// Log all network events for debugging
			logger.Debugf("Event received: type=%s network=%s driver=%s",
				msg.Action, msg.Actor.ID, msg.Actor.Attributes["type"])

			if msg.Type == events.ContainerEventType {
				if msg.Action == events.ActionKill && onStop != nil {
					signal := msg.Actor.Attributes["signal"]
					if isStopSignal(signal, stopSignalOf(ctx, cli, msg.Actor.ID)) {
						onStop(msg.Actor.ID)
					}
				}
				continue
			}

			// Only process events for networks Docker created through this driver,
			// whatever name the plugin was installed under
			if !ownsNetwork(msg.Actor.ID) {
				continue
			}

			handleConnect(ctx, cli, msg, cache, onInfo)
		}
	}
}

// handleConnect handles a container connecting to a tslink network: it
// inspects the container and network, caches the container's settings and
// passes them to onInfo.
func handleConnect(
	ctx context.Context,
	cli *dockerclient.Client,
	msg events.Message,
	cache *ContainerCache,
	onInfo ContainerInfoCallback,
) {
	containerID := msg.Actor.Attributes["container"]
	networkName := msg.Actor.Attributes["name"]

	if containerID == "" {
		return
	}

	logger.Infof(
		"Event: network connect - container=%s network=%s",
		containerID[:12],
		networkName,
	)

	// Inspect container to get labels and endpoint ID
	// This is safe - we're not in a callback
	info, err := cli.ContainerInspect(
		ctx,
		containerID,
		dockerclient.ContainerInspectOptions{},
	)
	if err != nil {
		logger.Errorf("Failed to inspect container %s: %v", containerID[:12], err)
		return
	}

	// Find the endpoint ID for this network
	if info.Container.NetworkSettings == nil ||
		info.Container.NetworkSettings.Networks == nil {
		logger.Warnf("Container %s has no network settings", containerID[:12])
		return
	}

	netSettings, ok := info.Container.NetworkSettings.Networks[networkName]
	if !ok {
		logger.Warnf("Container %s not found in network %s", containerID[:12], networkName)
		return
	}

	endpointID := netSettings.EndpointID
	if endpointID == "" {
		logger.Warnf(
			"Container %s has no endpoint ID for network %s",
			containerID[:12],
			networkName,
		)
		return
	}

	logger.Debugf("Container %s has endpoint ID %s", containerID[:12], endpointID[:12])

	// Parse container info
	name := strings.TrimPrefix(info.Container.Name, "/")

	labels := make(map[string]string)
	if info.Container.Config != nil && info.Container.Config.Labels != nil {
		labels = info.Container.Config.Labels
	}

	containerInfo := parseContainerInfo(name, labels)

	// The network's stack decides which tasks may use it
	netInfo, err := cli.NetworkInspect(
		ctx,
		netSettings.NetworkID,
		dockerclient.NetworkInspectOptions{},
	)
	if err != nil {
		logger.Errorf("Failed to inspect network %s: %v", networkName, err)
		return
	}
	containerInfo.NetworkStack = netInfo.Network.Labels[core.StackLabel]

	// Store in cache
	cache.Store(endpointID, containerInfo)

	// Trigger callback to start Tailscale setup
	if onInfo != nil {
		onInfo(endpointID, containerInfo)
	}
}

// stopSignalOf returns the container's configured stop signal, empty for the default.
func stopSignalOf(ctx context.Context, cli *dockerclient.Client, containerID string) string {
	info, err := cli.ContainerInspect(ctx, containerID, dockerclient.ContainerInspectOptions{})
	if err != nil || info.Container.Config == nil {
		return ""
	}
	return info.Container.Config.StopSignal
}

// isStopSignal reports whether a kill event's signal stops the container: its
// stop signal (SIGTERM unless configured) or SIGKILL, which docker stop sends
// when the timeout expires. Other signals, such as SIGHUP to reload, leave the
// container running.
func isStopSignal(signal, stopSignal string) bool {
	n, err := strconv.Atoi(signal)
	if err != nil {
		return false
	}
	if syscall.Signal(n) == unix.SIGKILL {
		return true
	}
	if stopSignal == "" {
		stopSignal = "SIGTERM"
	}
	if s, err := strconv.Atoi(stopSignal); err == nil {
		return n == s
	}
	name := strings.ToUpper(stopSignal)
	if !strings.HasPrefix(name, "SIG") {
		name = "SIG" + name
	}
	return unix.SignalNum(name) == syscall.Signal(n)
}

// parseContainerInfo extracts tslink.* labels into structured ContainerInfo.
func parseContainerInfo(name string, labels map[string]string) *core.ContainerInfo {
	info := &core.ContainerInfo{
		Name:   name,
		Labels: labels,
		Stack:  labels[core.StackLabel],
	}

	// tslink.hostname - override Tailscale hostname
	if v, ok := labels["tslink.hostname"]; ok && v != "" {
		info.Hostname = v
	} else {
		info.Hostname = hostnameFromName(name)
	}

	// tslink.tags - comma-separated ACL tags (e.g., "tag:web,tag:prod")
	if v, ok := labels["tslink.tags"]; ok && v != "" {
		info.Tags = core.ParseTags(v)
	}

	// tslink.service - service name (e.g., "svc:hello-world")
	if v, ok := labels["tslink.service"]; ok && v != "" {
		info.Service = v
	}

	// tslink.direct - serve the endpoints on the task's own name too. Defaults
	// to true without a Service only: with one, it would double what is
	// served, and every task would get a certificate of its own, even for
	// plain HTTP (https://github.com/tailscale/tailscale/issues/21693)
	if v, ok := labels["tslink.direct"]; ok {
		info.Direct = v != "false" && v != "0" && v != "no"
	} else {
		info.Direct = info.Service == ""
	}

	// Parse serve endpoints from tslink.serve.<port> labels
	info.Endpoints = parseServeEndpoints(labels)

	return info
}

// hostnameFromName turns a container name into a DNS label Tailscale accepts.
// Swarm task names such as "stack_svc.1.<task-id>" contain underscores and
// dots, which tailscale up rejects.
func hostnameFromName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	label := b.String()
	if len(label) > 63 {
		label = label[:63]
	}
	return strings.Trim(label, "-")
}

// parseServeEndpoints parses tslink.serve.<port> labels into ServeEndpoint structs.
// Format: tslink.serve.<external-port> = <proto>:<target>[/<path>]
// Examples:
//   - tslink.serve.443=https:8080       → HTTPS on 443 → localhost:8080
//   - tslink.serve.80=http:3000/api     → HTTP on 80 → localhost:3000 at /api
//   - tslink.serve.5432=tcp             → TCP on 5432 → localhost:5432 (same port)
func parseServeEndpoints(labels map[string]string) []core.ServeEndpoint {
	var endpoints []core.ServeEndpoint

	for key, value := range labels {
		if !strings.HasPrefix(key, "tslink.serve.") {
			continue
		}

		// Extract external port from key
		externalPort := strings.TrimPrefix(key, "tslink.serve.")
		if externalPort == "" {
			continue
		}

		endpoint := parseServeValue(externalPort, value)
		if endpoint != nil {
			endpoints = append(endpoints, *endpoint)
		}
	}

	return endpoints
}

// parseServeValue parses a serve label value.
// Format: <proto>[:<target>][/<path>][?<option>[&<option>]]
// Options:
//   - proxy-protocol=1|2, for tcp and tls-terminated-tcp: send the target the
//     caller's tailnet address in a PROXY protocol header
//   - accept-app-caps=<cap>[,<cap>], for http and https: forward the caller's
//     grants of these app capabilities in the Tailscale-App-Capabilities header
//
// Examples:
//   - "https:8080"      → proto=https, target=8080
//   - "http:3000/api"   → proto=http, target=3000, path=/api
//   - "tcp"             → proto=tcp, target=<same as external port>
//   - "tcp:5432"        → proto=tcp, target=5432
func parseServeValue(externalPort, value string) *core.ServeEndpoint {
	if value == "" {
		return nil
	}

	endpoint := &core.ServeEndpoint{
		Port:   externalPort,
		Target: externalPort, // Default: same as external
	}

	value, options, _ := strings.Cut(value, "?")
	if options != "" && !parseServeOptions(endpoint, options) {
		return nil
	}

	// Split by colon to get proto and target
	parts := strings.SplitN(value, ":", 2)
	endpoint.Proto = strings.ToLower(parts[0])

	// Validate protocol
	switch endpoint.Proto {
	case "http", "https", "tcp", "tls-terminated-tcp", "tun":
		// Valid
	default:
		logger.Debugf(
			"parseServeValue: unknown protocol %q for port %s",
			endpoint.Proto,
			externalPort,
		)
		return nil
	}

	if len(parts) > 1 {
		targetAndPath := parts[1]

		// Check for path (L7 only)
		if idx := strings.Index(targetAndPath, "/"); idx != -1 {
			endpoint.Target = targetAndPath[:idx]
			endpoint.Path = targetAndPath[idx:] // Keep the leading /
		} else {
			endpoint.Target = targetAndPath
		}
	}

	if endpoint.ProxyProtocol != "" && endpoint.Proto != "tcp" &&
		endpoint.Proto != "tls-terminated-tcp" {
		logger.Warnf(
			"tslink.serve.%s: ignoring endpoint: proxy-protocol needs tcp or tls-terminated-tcp, not %s",
			externalPort,
			endpoint.Proto,
		)
		return nil
	}
	if endpoint.AcceptAppCaps != "" && endpoint.Proto != "http" && endpoint.Proto != "https" {
		logger.Warnf(
			"tslink.serve.%s: ignoring endpoint: accept-app-caps needs http or https, not %s",
			externalPort,
			endpoint.Proto,
		)
		return nil
	}

	// Default target to external port if empty
	if endpoint.Target == "" {
		endpoint.Target = externalPort
	}

	return endpoint
}

// parseServeOptions sets the endpoint's options from the query after the
// "?" of a tslink.serve value, and reports false if one is invalid.
func parseServeOptions(endpoint *core.ServeEndpoint, options string) bool {
	for opt := range strings.SplitSeq(options, "&") {
		key, v, _ := strings.Cut(opt, "=")
		switch {
		case key == "proxy-protocol" && (v == "1" || v == "2"):
			endpoint.ProxyProtocol = v
		case key == "accept-app-caps" && validAppCaps(v):
			endpoint.AcceptAppCaps = v
		default:
			logger.Warnf("tslink.serve.%s: ignoring endpoint with invalid option %q "+
				"(want proxy-protocol=1|2 or accept-app-caps=<domain>/<name>[,...])", endpoint.Port, opt)
			return false
		}
	}
	return true
}

// validAppCaps reports whether caps is a comma-separated list of app
// capability names, each of the form <domain>/<name>.
func validAppCaps(caps string) bool {
	for c := range strings.SplitSeq(caps, ",") {
		domain, name, ok := strings.Cut(c, "/")
		if !ok || domain == "" || name == "" || strings.ContainsAny(c, " \t") {
			return false
		}
	}
	return true
}
