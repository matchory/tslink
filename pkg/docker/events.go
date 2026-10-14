package docker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	dockerclient "github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/stringid"
	"golang.org/x/sys/unix"

	"github.com/matchory/tslink/internal/ctxtime"
	"github.com/matchory/tslink/pkg/core"
	"github.com/matchory/tslink/pkg/logger"
	"github.com/matchory/tslink/pkg/tailscale"
)

// dockerCallTimeout bounds each Docker API call of the event handlers, which
// run one at a time: a call Docker does not answer would hold up every event
// after it. The host isolation's network listing, the inspect that rebuilds
// a network and the recovery scan's listings use it too. A variable for
// tests.
var dockerCallTimeout = 10 * time.Second

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
		stringid.TruncateID(endpointID),
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

// Container returns the endpoints the cache holds the container's info for,
// and its stop signal.
func (c *ContainerCache) Container(containerID string) ([]string, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var endpointIDs []string
	stopSignal := ""
	for endpointID, info := range c.byEndpoint {
		if info.ID == containerID {
			endpointIDs = append(endpointIDs, endpointID)
			stopSignal = info.StopSignal
		}
	}
	return endpointIDs, stopSignal
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

// ContainerStopCallback is called when Docker sends a container on this
// driver's networks its stop signal, before the container exits, with the
// container's endpoints on them.
type ContainerStopCallback func(containerID string, endpointIDs []string)

// WatchEvents watches Docker events and caches container info.
// When container info is stored, the callback is invoked to trigger Tailscale setup.
// The context controls the lifecycle - when cancelled, the watcher stops.
// ownsNetwork reports whether a network ID belongs to this driver.
// onStop, if set, is called when a container is sent its stop signal.
// onBridgeNetwork, if set, is called when a bridge network is created.
func WatchEvents(
	ctx context.Context,
	cli dockerAPI,
	cache *ContainerCache,
	ownsNetwork func(id string) bool,
	onInfo ContainerInfoCallback,
	onStop ContainerStopCallback,
	onBridgeNetwork func(),
) {
	logger.Infof("Starting Docker event watcher")

	// Watch for network connects, for kills to see containers stopping, and
	// for new bridge networks, whose bridges the host's tailnet isolation
	// covers by name
	filterArgs := dockerclient.Filters{}.
		Add("type", "network", "container").
		Add("event", "connect", "kill", "create")

	// The time of the first event the watcher has not seen. Reconnecting, it
	// asks for the events since then: a connect event lost while the stream
	// was down would leave its endpoint without Tailscale until the watchdog.
	since := time.Now()
	result := cli.Events(ctx, dockerclient.EventsListOptions{
		Filters: filterArgs,
	})

	logger.Debugf("Event stream started, waiting for events...")

	for {
		select {
		case <-ctx.Done():
			logger.Infof("Event watcher shutting down")
			return

		case err, ok := <-result.Err:
			if !ok {
				// The stream ended: a closed channel would yield nil errors
				// in a busy loop
				err = errors.New("event stream closed")
			}
			if err == nil {
				continue
			}
			if !backOff(ctx, err) {
				return
			}
			// Reconnect, with the events missed meanwhile
			result = cli.Events(ctx, dockerclient.EventsListOptions{
				Since:   eventsSince(since),
				Filters: filterArgs,
			})

		case msg := <-result.Messages:
			if msg.TimeNano != 0 {
				since = time.Unix(0, msg.TimeNano+1)
			}

			// Log all network events for debugging
			logger.Debugf("Event received: type=%s network=%s driver=%s",
				msg.Action, msg.Actor.ID, msg.Actor.Attributes["type"])

			if msg.Type == events.ContainerEventType {
				handleContainerEvent(msg, cache, onStop)
				continue
			}
			handleNetworkEvent(ctx, cli, msg, cache, ownsNetwork, onInfo, onBridgeNetwork)
		}
	}
}

// handleContainerEvent passes containers on this driver's networks sent their
// stop signal to onStop. Their endpoints and stop signal come from the cache,
// as their connect events or recovery stored them, so a kill costs no Docker
// call: kill events of every container on the host pass through here, and a
// container removed right after its kill, as by docker rm -f, could no longer
// be inspected.
func handleContainerEvent(msg events.Message, cache *ContainerCache, onStop ContainerStopCallback) {
	if msg.Action != events.ActionKill || onStop == nil {
		return
	}
	endpointIDs, stopSignal := cache.Container(msg.Actor.ID)
	if len(endpointIDs) > 0 && isStopSignal(msg.Actor.Attributes["signal"], stopSignal) {
		onStop(msg.Actor.ID, endpointIDs)
	}
}

// handleNetworkEvent passes new bridge networks to onBridgeNetwork, and
// containers joining this driver's networks to handleConnect.
func handleNetworkEvent(
	ctx context.Context,
	cli dockerAPI,
	msg events.Message,
	cache *ContainerCache,
	ownsNetwork func(id string) bool,
	onInfo ContainerInfoCallback,
	onBridgeNetwork func(),
) {
	if msg.Action == events.ActionCreate {
		if msg.Actor.Attributes["type"] == "bridge" && onBridgeNetwork != nil {
			onBridgeNetwork()
		}
		return
	}

	// Only process events for networks Docker created through this driver,
	// whatever name the plugin was installed under
	if !ownsNetwork(msg.Actor.ID) {
		return
	}

	handleConnect(ctx, cli, msg, cache, onInfo)
}

// backOff logs the error that ended the event stream and waits a second
// before the watcher reconnects. It reports false if ctx is done.
func backOff(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		logger.Infof("Event watcher context cancelled, stopping")
		return false
	}
	logger.Warnf("Event stream error: %v, reconnecting in 1s...", err)
	if !ctxtime.Sleep(ctx, time.Second) {
		logger.Infof("Event watcher context cancelled during backoff")
		return false
	}
	return true
}

// handleConnect handles a container connecting to a tslink network: it
// inspects the container and network, caches the container's settings and
// passes them to onInfo.
func handleConnect(
	ctx context.Context,
	cli dockerAPI,
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
		stringid.TruncateID(containerID),
		networkName,
	)

	// Inspect container to get labels and endpoint ID
	// This is safe - we're not in a callback
	callCtx, cancel := context.WithTimeout(ctx, dockerCallTimeout)
	info, err := cli.ContainerInspect(
		callCtx,
		containerID,
		dockerclient.ContainerInspectOptions{},
	)
	cancel()
	if err != nil {
		logger.Errorf("Failed to inspect container %s: %v", stringid.TruncateID(containerID), err)
		return
	}

	// Find the endpoint ID for this network
	if info.Container.NetworkSettings == nil ||
		info.Container.NetworkSettings.Networks == nil {
		logger.Warnf("Container %s has no network settings", stringid.TruncateID(containerID))
		return
	}

	netSettings, ok := info.Container.NetworkSettings.Networks[networkName]
	if !ok {
		logger.Warnf(
			"Container %s not found in network %s",
			stringid.TruncateID(containerID),
			networkName,
		)
		return
	}

	endpointID := netSettings.EndpointID
	if endpointID == "" {
		logger.Warnf(
			"Container %s has no endpoint ID for network %s",
			stringid.TruncateID(containerID),
			networkName,
		)
		return
	}

	logger.Debugf(
		"Container %s has endpoint ID %s",
		stringid.TruncateID(containerID),
		stringid.TruncateID(endpointID),
	)

	// The network's stack decides which tasks may use it
	callCtx, cancel = context.WithTimeout(ctx, dockerCallTimeout)
	netInfo, err := cli.NetworkInspect(
		callCtx,
		netSettings.NetworkID,
		dockerclient.NetworkInspectOptions{},
	)
	cancel()
	if err != nil {
		logger.Errorf("Failed to inspect network %s: %v", networkName, err)
		return
	}
	containerInfo := inspectedContainerInfo(
		&info.Container,
		netInfo.Network.Labels[core.StackLabel],
	)
	addDaemonDNS(ctx, cli, containerInfo)

	// Store in cache
	cache.Store(endpointID, containerInfo)

	// Trigger callback to start Tailscale setup
	if onInfo != nil {
		onInfo(endpointID, containerInfo)
	}
}

// eventsSince formats t for the Since option of the event stream.
func eventsSince(t time.Time) string {
	return fmt.Sprintf("%d.%09d", t.Unix(), t.Nanosecond())
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

// inspectedContainerInfo returns the Tailscale configuration of an inspected
// container: from its name and labels, and its DNS servers. networkStack is
// the stack label of the tslink network, which decides which tasks may use
// it.
func inspectedContainerInfo(
	c *container.InspectResponse,
	networkStack string,
) *core.ContainerInfo {
	labels := map[string]string{}
	if c.Config != nil && c.Config.Labels != nil {
		labels = c.Config.Labels
	}
	info := parseContainerInfo(core.ContainerName(c.Name), labels)
	info.ID = c.ID
	if c.Config != nil {
		info.StopSignal = c.Config.StopSignal
	}
	if c.HostConfig != nil {
		info.DNS = c.HostConfig.DNS
	}
	info.NetworkStack = networkStack
	return info
}

// containerLabels sets each of tslink's container labels on a container's
// info from its value. The labels tslink.serve.<port> are
// parseServeEndpoints'.
var containerLabels = map[string]func(info *core.ContainerInfo, v string){
	core.HostnameLabel: func(info *core.ContainerInfo, v string) {
		if v != "" {
			info.Hostname = v
		}
	},
	// Comma-separated ACL tags, e.g. "tag:web,tag:prod"
	core.TagsLabel: func(info *core.ContainerInfo, v string) {
		info.Tags = core.ParseTags(v)
	},
	core.ServiceLabel: func(info *core.ContainerInfo, v string) {
		info.Service = v
	},
	// Serve the endpoints on the task's own name too. Defaults to true
	// without a Service only: with one, it would double what is served, and
	// every task would get a certificate of its own, even for plain HTTP
	// (https://github.com/tailscale/tailscale/issues/21693)
	core.DirectLabel: func(info *core.ContainerInfo, v string) {
		info.Direct = v != "false" && v != "0" && v != "no"
	},
	// Serve the readiness endpoint on this loopback port
	core.HealthLabel: func(info *core.ContainerInfo, v string) {
		port, err := strconv.ParseUint(v, 10, 16)
		if err != nil || port == 0 {
			logger.Warnf("Container %s: ignoring %s=%q: not a port", info.Name, core.HealthLabel, v)
			return
		}
		info.HealthPort = int(port)
	},
}

// parseContainerInfo reads a container's tslink labels into its info. A label
// under tslink's prefix that it does not know is logged and ignored.
func parseContainerInfo(name string, labels map[string]string) *core.ContainerInfo {
	info := &core.ContainerInfo{
		Name:     name,
		Labels:   labels,
		Stack:    labels[core.StackLabel],
		Hostname: hostnameFromName(name),
	}
	for _, key := range slices.Sorted(maps.Keys(labels)) {
		set, known := containerLabels[key]
		switch {
		case known:
			set(info, labels[key])
		case strings.HasPrefix(key, core.ServeLabelPrefix):
		case strings.HasPrefix(key, core.KeyPrefix):
			logger.Warnf("Container %s: ignoring unknown label %s", name, key)
		}
	}
	if _, ok := labels[core.DirectLabel]; !ok {
		info.Direct = info.Service == ""
	}
	info.Endpoints = parseServeEndpoints(name, labels)
	return info
}

// hostnameFromName turns a container name into a DNS label Tailscale accepts.
// Swarm task names such as "stack_svc.1.<task-id>" contain underscores and
// dots, which Tailscale rejects.
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
//
// It logs the labels it ignores, with the name of the container they belong
// to.
func parseServeEndpoints(containerName string, labels map[string]string) []tailscale.ServeEndpoint {
	var endpoints []tailscale.ServeEndpoint

	for key, value := range labels {
		externalPort, ok := strings.CutPrefix(key, core.ServeLabelPrefix)
		if !ok {
			continue
		}

		endpoint := parseServeValue(containerName, externalPort, value)
		if endpoint != nil {
			endpoints = append(endpoints, *endpoint)
		}
	}

	// In the order of their ports, as labels come as a map. Ports are
	// numbers (parseServeValue); "0443" and "443" go by their labels.
	slices.SortFunc(endpoints, func(a, b tailscale.ServeEndpoint) int {
		pa, _ := strconv.Atoi(a.Port)
		pb, _ := strconv.Atoi(b.Port)
		return cmp.Or(cmp.Compare(pa, pb), strings.Compare(a.Port, b.Port))
	})
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
func parseServeValue(containerName, externalPort, value string) *tailscale.ServeEndpoint {
	if value == "" {
		return nil
	}
	if port, err := strconv.ParseUint(externalPort, 10, 16); err != nil || port == 0 {
		logger.Warnf(
			"Container %s: tslink.serve.%s: ignoring endpoint: not a port",
			containerName,
			externalPort,
		)
		return nil
	}

	endpoint := &tailscale.ServeEndpoint{
		Port:   externalPort,
		Target: externalPort, // Default: same as external
	}

	value, options, _ := strings.Cut(value, "?")
	if options != "" && !parseServeOptions(containerName, endpoint, options) {
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
		logger.Warnf(
			"Container %s: tslink.serve.%s: ignoring endpoint: unknown protocol %q",
			containerName,
			externalPort,
			endpoint.Proto,
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

	if !optionsFitProtocol(containerName, endpoint) {
		return nil
	}

	// Default target to external port if empty
	if endpoint.Target == "" {
		endpoint.Target = externalPort
	}

	return endpoint
}

// optionsFitProtocol reports whether the endpoint's options apply to its
// protocol, and logs the one that does not.
func optionsFitProtocol(containerName string, endpoint *tailscale.ServeEndpoint) bool {
	l4 := endpoint.Proto == "tcp" || endpoint.Proto == "tls-terminated-tcp"
	l7 := endpoint.Proto == "http" || endpoint.Proto == "https"
	switch {
	case endpoint.ProxyProtocol != "" && !l4:
		logger.Warnf(
			"Container %s: tslink.serve.%s: ignoring endpoint: "+
				"proxy-protocol needs tcp or tls-terminated-tcp, not %s",
			containerName,
			endpoint.Port,
			endpoint.Proto,
		)
		return false
	case endpoint.AcceptAppCaps != "" && !l7:
		logger.Warnf(
			"Container %s: tslink.serve.%s: ignoring endpoint: "+
				"accept-app-caps needs http or https, not %s",
			containerName,
			endpoint.Port,
			endpoint.Proto,
		)
		return false
	}
	return true
}

// parseServeOptions sets the endpoint's options from the query after the
// "?" of a tslink.serve value, and reports false if one is invalid.
func parseServeOptions(
	containerName string,
	endpoint *tailscale.ServeEndpoint,
	options string,
) bool {
	for opt := range strings.SplitSeq(options, "&") {
		key, v, _ := strings.Cut(opt, "=")
		switch {
		case key == "proxy-protocol" && (v == "1" || v == "2"):
			endpoint.ProxyProtocol = v
		case key == "accept-app-caps" && validAppCaps(v):
			endpoint.AcceptAppCaps = v
		default:
			logger.Warnf("Container %s: tslink.serve.%s: ignoring endpoint with invalid option %q "+
				"(want proxy-protocol=1|2 or accept-app-caps=<domain>/<name>[,...])",
				containerName, endpoint.Port, opt)
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
