// Package docker implements the Docker network driver API and watches
// Docker events for container starts and stops.
package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/docker/go-plugins-helpers/network"
	dockerclient "github.com/moby/moby/client"

	"github.com/matchory/tslink/pkg/core"
	"github.com/matchory/tslink/pkg/logger"
)

// Driver implements the Docker network plugin interface.
type Driver struct {
	mu        sync.RWMutex
	networks  map[string]*core.Network
	endpoints map[string]*core.Endpoint
	config    *core.Config
	claims    *core.StateClaims // State directories in use by endpoints or garbage collection
	cache     *ContainerCache   // Pre-cached container info from Docker events

	// Docker client for container inspection, recovery and events
	docker dockerAPI

	// Endpoint operations that need root, netlink or tailscaled; tests
	// replace them. newDriver sets them to the core.Endpoint methods.
	joinEndpoint   func(e *core.Endpoint, sandboxKey string) (*network.JoinResponse, error)
	recoverRouting func(e *core.Endpoint, sandboxKey string) error
	runTailscale   func(e *core.Endpoint, info *core.ContainerInfo)
	leaveEndpoint  func(e *core.Endpoint) error
	stopEndpoint   func(e *core.Endpoint) error

	// Serializes RecoverEndpoints
	recoverMu sync.Mutex

	// Lifecycle management
	ctx    context.Context //nolint:containedctx // lifecycle context, cancelled on shutdown
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// drainGrace is how long a shutdown waits after draining Service backends,
// for the tailnet to learn of it before the backends disappear.
const drainGrace = 2 * time.Second

// watchdogInterval is the interval at which the watchdog checks for orphaned endpoints.
const watchdogInterval = 60 * time.Second

// redactKey returns a redacted version of a key for safe logging.
// Shows length and a short hash to identify if the key changed.
func redactKey(key string) string {
	if key == "" {
		return "(empty)"
	}
	hash := sha256.Sum256([]byte(key))
	shortHash := hex.EncodeToString(hash[:4])
	return fmt.Sprintf("(set, %d chars, hash=%s)", len(key), shortHash)
}

// NewDriver creates a new Docker network driver.
func NewDriver() (*Driver, error) {
	cfg, err := core.LoadConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	// Create Docker client for container inspection and recovery
	docker, err := dockerclient.New(dockerclient.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	d := newDriver(cfg, docker)
	d.start()
	return d, nil
}

// newDriver returns a driver using the given configuration and Docker client,
// without starting its background work.
func newDriver(cfg *core.Config, docker dockerAPI) *Driver {
	ctx, cancel := context.WithCancel(context.Background())
	return &Driver{
		networks:       make(map[string]*core.Network),
		endpoints:      make(map[string]*core.Endpoint),
		config:         cfg,
		claims:         core.NewStateClaims(),
		cache:          NewContainerCache(),
		docker:         docker,
		joinEndpoint:   (*core.Endpoint).Join,
		recoverRouting: (*core.Endpoint).Recover,
		runTailscale:   (*core.Endpoint).RunTailscale,
		leaveEndpoint:  (*core.Endpoint).Leave,
		stopEndpoint:   (*core.Endpoint).Stop,
		ctx:            ctx,
		cancel:         cancel,
	}
}

// GetCapabilities returns the capabilities of the driver.
func (d *Driver) GetCapabilities() (*network.CapabilitiesResponse, error) {
	logger.Infof("GetCapabilities called")
	return &network.CapabilitiesResponse{
		Scope:             "global", // spike: so swarm carries driver options to nodes
		ConnectivityScope: "global", // Containers can reach the tailnet
	}, nil
}

// CreateNetwork creates a new network.
func (d *Driver) CreateNetwork(req *network.CreateNetworkRequest) error {
	logger.Infof("CreateNetwork: %s", req.NetworkID)

	// Log option keys only: values include the auth key
	for k, v := range req.Options {
		logger.Debugf("  Option: %q (type: %T)", k, v)
		if nested, ok := v.(map[string]any); ok {
			for nk := range nested {
				logger.Debugf("    Nested: %q", nk)
			}
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	opts := core.ParseNetworkOptions(req.Options)
	logger.Debugf("Parsed opts: authkey=%s", redactKey(opts.AuthKey))

	net, err := core.NewNetwork(req.NetworkID, opts, d.config)
	if err != nil {
		return err
	}
	if net.UsesClusterCredential() {
		logger.Debugf("Network %s uses the cluster credential", req.NetworkID)
	}

	d.networks[req.NetworkID] = net
	logger.Infof("Created network %s", req.NetworkID)

	return nil
}

// AllocateNetwork is called during network creation (for multi-host networks).
func (d *Driver) AllocateNetwork(
	req *network.AllocateNetworkRequest,
) (*network.AllocateNetworkResponse, error) {
	logger.Infof("AllocateNetwork: %s", req.NetworkID)
	// spike: hand the options back so swarm stores them as driver state
	return &network.AllocateNetworkResponse{Options: req.Options}, nil
}

// DeleteNetwork deletes a network.
func (d *Driver) DeleteNetwork(req *network.DeleteNetworkRequest) error {
	logger.Infof("DeleteNetwork: %s", req.NetworkID)

	d.mu.Lock()
	defer d.mu.Unlock()

	delete(d.networks, req.NetworkID)
	return nil
}

// FreeNetwork is called during network deletion (for multi-host networks).
func (d *Driver) FreeNetwork(req *network.FreeNetworkRequest) error {
	logger.Infof("FreeNetwork: %s", req.NetworkID)
	return nil
}

// CreateEndpoint creates a new endpoint for a container.
func (d *Driver) CreateEndpoint(
	req *network.CreateEndpointRequest,
) (*network.CreateEndpointResponse, error) {
	logger.Infof("CreateEndpoint: network=%s endpoint=%s", req.NetworkID, req.EndpointID)

	net, err := d.networkFor(d.ctx, req.NetworkID)
	if err != nil {
		return nil, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	// Parse endpoint options for hostname override
	opts := core.ParseEndpointOptions(req.Options)

	endpoint, err := core.NewEndpoint(req.EndpointID, net, opts, d.config, d.claims)
	if err != nil {
		return nil, fmt.Errorf("failed to create endpoint: %w", err)
	}

	d.endpoints[req.EndpointID] = endpoint

	// The interface will be configured during Join
	return &network.CreateEndpointResponse{}, nil
}

// DeleteEndpoint deletes an endpoint.
func (d *Driver) DeleteEndpoint(req *network.DeleteEndpointRequest) error {
	logger.Infof("DeleteEndpoint: network=%s endpoint=%s", req.NetworkID, req.EndpointID)

	// Forget the endpoint under the lock, and stop it after releasing it:
	// endpoint methods take endpoint.mu, and stopping can take seconds
	d.mu.Lock()
	d.cache.Delete(req.EndpointID)
	endpoint, ok := d.endpoints[req.EndpointID]
	delete(d.endpoints, req.EndpointID)
	d.mu.Unlock()

	if !ok {
		logger.Infof("Endpoint %s not found, ignoring", req.EndpointID)
		return nil
	}

	if err := d.stopEndpoint(endpoint); err != nil {
		logger.Infof("Warning: failed to stop endpoint: %v", err)
	}
	return nil
}

// EndpointInfo returns information about an endpoint.
func (d *Driver) EndpointInfo(req *network.InfoRequest) (*network.InfoResponse, error) {
	logger.Infof("EndpointInfo: network=%s endpoint=%s", req.NetworkID, req.EndpointID)

	d.mu.RLock()
	defer d.mu.RUnlock()

	endpoint, ok := d.endpoints[req.EndpointID]
	if !ok {
		return nil, fmt.Errorf("endpoint %s not found", req.EndpointID)
	}

	// Use safe getter to avoid race with StartTailscale
	tailscaleIP, hostname := endpoint.GetInfo()

	return &network.InfoResponse{
		Value: map[string]string{
			"tailscale_ip": tailscaleIP,
			"hostname":     hostname,
		},
	}, nil
}

// Join is called when a container joins the network.
// It sets up basic networking and returns quickly.
// Tailscale setup is triggered asynchronously by the Docker event handler.
func (d *Driver) Join(req *network.JoinRequest) (*network.JoinResponse, error) {
	logger.Infof(
		"Join: network=%s endpoint=%s sandbox=%s",
		req.NetworkID,
		req.EndpointID,
		req.SandboxKey,
	)

	// Get endpoint reference under lock, then release before calling endpoint methods
	d.mu.RLock()
	endpoint, ok := d.endpoints[req.EndpointID]
	d.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("endpoint %s not found", req.EndpointID)
	}

	// Set up basic networking (veth, IPs, routing, NAT)
	// This returns quickly - Tailscale setup is deferred until container info arrives via event
	// NOTE: driver.mu is NOT held here to avoid lock ordering issues with endpoint.mu
	joinResp, err := d.joinEndpoint(endpoint, req.SandboxKey)
	if err != nil {
		return nil, fmt.Errorf("failed to join: %w", err)
	}

	// Check if container info is already in cache (rare but possible)
	// cache has its own internal lock, so this is safe
	if info, ok := d.cache.GetByEndpoint(req.EndpointID); ok {
		logger.Infof("Join: container info already cached, triggering immediate Tailscale setup")
		d.startTailscale(endpoint, info)
	} else {
		logger.Infof(
			"Join: waiting for Docker event to trigger Tailscale setup for endpoint %s",
			req.EndpointID[:12],
		)
	}

	return joinResp, nil
}

// Leave is called when a container leaves the network.
func (d *Driver) Leave(req *network.LeaveRequest) error {
	logger.Infof("Leave: network=%s endpoint=%s", req.NetworkID, req.EndpointID)

	// Leaving drains, waits for control and logs out, which takes seconds:
	// hold no driver lock meanwhile. The endpoint stays in d.endpoints until
	// DeleteEndpoint, so recovery does not adopt it again while it leaves.
	d.mu.RLock()
	endpoint, ok := d.endpoints[req.EndpointID]
	d.mu.RUnlock()
	if !ok {
		logger.Infof("Endpoint %s not found, ignoring", req.EndpointID)
		return nil
	}

	if err := d.leaveEndpoint(endpoint); err != nil {
		logger.Infof("Warning: failed to leave: %v", err)
	}

	return nil
}

// DiscoverNew is called when a new node is discovered.
func (d *Driver) DiscoverNew(req *network.DiscoveryNotification) error {
	logger.Infof("DiscoverNew: type=%d", req.DiscoveryType)
	return nil
}

// DiscoverDelete is called when a node is removed.
func (d *Driver) DiscoverDelete(req *network.DiscoveryNotification) error {
	logger.Infof("DiscoverDelete: type=%d", req.DiscoveryType)
	return nil
}

// ProgramExternalConnectivity is called to program external connectivity.
func (d *Driver) ProgramExternalConnectivity(
	req *network.ProgramExternalConnectivityRequest,
) error {
	logger.Infof(
		"ProgramExternalConnectivity: network=%s endpoint=%s",
		req.NetworkID,
		req.EndpointID,
	)
	return nil
}

// RevokeExternalConnectivity is called to revoke external connectivity.
func (d *Driver) RevokeExternalConnectivity(req *network.RevokeExternalConnectivityRequest) error {
	logger.Infof(
		"RevokeExternalConnectivity: network=%s endpoint=%s",
		req.NetworkID,
		req.EndpointID,
	)
	return nil
}

// Shutdown gracefully stops the driver and all managed resources.
// It cancels the context (stopping the event watcher and any pending operations),
// stops all endpoints, and waits for goroutines to finish.
// The passed context controls how long to wait for graceful shutdown.
func (d *Driver) Shutdown(ctx context.Context) error {
	logger.Infof("Driver shutdown initiated")

	// Cancel internal context to stop event watcher and pending operations
	d.cancel()

	// Stop all endpoints
	d.mu.Lock()
	endpoints := make([]*core.Endpoint, 0, len(d.endpoints))
	for _, ep := range d.endpoints {
		endpoints = append(endpoints, ep)
	}
	d.mu.Unlock()

	// tailscaled stops with the plugin. Drain Service backends first, so
	// callers use other nodes' backends until the plugin is back.
	var drains sync.WaitGroup
	for _, ep := range endpoints {
		drains.Go(ep.DrainService)
	}
	drains.Wait()
	if len(endpoints) > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(drainGrace):
		}
	}

	for _, ep := range endpoints {
		logger.Infof("Stopping endpoint %s", ep.ID[:12])
		if err := ep.Stop(); err != nil {
			logger.Warnf("Failed to stop endpoint %s: %v", ep.ID[:12], err)
		}
	}

	// Wait for all goroutines to finish (or context timeout)
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		logger.Infof("Driver shutdown complete")
	case <-ctx.Done():
		logger.Warnf("Driver shutdown timed out, some goroutines may still be running")
		return ctx.Err()
	}

	// Close Docker client
	if d.docker != nil {
		if err := d.docker.Close(); err != nil {
			logger.Warnf("Failed to close Docker client: %v", err)
		}
	}

	return nil
}

// RecoverEndpoints scans Docker for containers on tslink networks that are missing
// from the driver's in-memory state. This handles host reboot and plugin restart.
// It also starts Tailscale for endpoints that joined but missed their connect
// event, as when the event stream was down.
func (d *Driver) RecoverEndpoints(ctx context.Context) error {
	// The initial recovery and the watchdog may overlap while Docker is down
	d.recoverMu.Lock()
	defer d.recoverMu.Unlock()

	logger.Infof("RecoverEndpoints: scanning for orphaned endpoints")

	ownNames, err := d.ownPluginNames(ctx)
	if err != nil {
		return err
	}

	// List all running containers
	containerList, err := d.docker.ContainerList(ctx, dockerclient.ContainerListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}

	recovered := 0
	for _, container := range containerList.Items {
		// Check each network the container is attached to
		if container.NetworkSettings == nil {
			continue
		}

		for netName, netSettings := range container.NetworkSettings.Networks {
			if netSettings == nil || netSettings.NetworkID == "" || netSettings.EndpointID == "" {
				continue
			}
			if d.recoverAttachment(
				ctx,
				ownNames,
				container.ID,
				netName,
				netSettings.NetworkID,
				netSettings.EndpointID,
			) {
				recovered++
			}
		}
	}

	if recovered > 0 {
		logger.Infof("RecoverEndpoints: recovered %d orphaned endpoint(s)", recovered)
	} else {
		logger.Debugf("RecoverEndpoints: no orphaned endpoints found")
	}

	return nil
}

// start starts the event watcher, the initial recovery and the watchdog.
func (d *Driver) start() {
	ctx := d.ctx

	// Start event watcher in background (tracked by waitgroup)
	// Pass callback to trigger Tailscale setup when container info arrives
	d.wg.Go(func() {
		WatchEvents(ctx, d.docker, d.cache, d.ownsNetwork, d.onContainerInfo, d.onContainerStop)
	})

	// Recover orphaned endpoints from previous plugin instance (host reboot, plugin restart)
	// Run in background so plugin starts accepting requests immediately
	d.wg.Go(func() {
		// Every second until it works: tasks have no tailnet until recovered,
		// and the Docker API may still be starting after a host reboot
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			err := d.RecoverEndpoints(ctx)
			if err == nil {
				d.collectGarbage(ctx)
				return
			}
			logger.Warnf("Initial endpoint recovery failed, retrying: %v", err)
		}
	})

	// Start watchdog for continuous reconciliation
	d.wg.Go(func() {
		d.runWatchdog(ctx)
	})
}

// ownsNetwork reports whether Docker created the network through this driver.
func (d *Driver) ownsNetwork(id string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	_, ok := d.networks[id]
	return ok
}

// ownPluginNames returns the names of enabled plugins running tslink, which
// are the driver names of its networks. The plugin can be installed under any
// name, and recovery runs before Docker has told it about any network.
// If several tslink plugins are enabled at once, each recovers the others'
// networks too.
func (d *Driver) ownPluginNames(ctx context.Context) (map[string]bool, error) {
	plugins, err := d.docker.PluginList(ctx, dockerclient.PluginListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list plugins: %w", err)
	}
	names := make(map[string]bool)
	for _, p := range plugins.Items {
		if p.Enabled && slices.Equal(p.Config.Entrypoint, []string{"/tslink"}) {
			names[p.Name] = true
		}
	}
	return names, nil
}

// onContainerInfo is called by the event watcher when container info is stored.
// It triggers Tailscale setup for the endpoint with the correct hostname.
func (d *Driver) onContainerInfo(endpointID string, info *core.ContainerInfo) {
	// NOTE: We must not hold driver.mu while calling endpoint methods.
	// Lock ordering: never hold driver.mu when acquiring endpoint.mu to avoid deadlock.

	// Get endpoint reference under lock
	d.mu.RLock()
	endpoint, ok := d.endpoints[endpointID]
	d.mu.RUnlock()

	if !ok {
		logger.Debugf(
			"onContainerInfo: endpoint %s not found (may have already left)",
			endpointID[:12],
		)
		return
	}

	// Call endpoint methods without holding driver.mu
	if endpoint.IsTailscaleStarted() {
		logger.Debugf("onContainerInfo: Tailscale already started for endpoint %s", endpointID[:12])
		return
	}

	if endpoint.GetSandboxKey() == "" {
		logger.Debugf(
			"onContainerInfo: endpoint %s has no sandbox key (Join not called yet)",
			endpointID[:12],
		)
		return
	}

	logger.Infof(
		"onContainerInfo: triggering Tailscale setup for endpoint %s (hostname=%s)",
		endpointID[:12],
		info.Hostname,
	)

	// Start Tailscale in a goroutine so we don't block the event handler
	d.startTailscale(endpoint, info)
}

// startTailscale starts Tailscale for the endpoint in the background, and
// records that it was asked to, so recovery leaves the endpoint to this start.
func (d *Driver) startTailscale(endpoint *core.Endpoint, info *core.ContainerInfo) {
	endpoint.MarkStartRequested()
	d.wg.Go(func() { d.runTailscale(endpoint, info) })
}

// gcMinAge protects state that an endpoint still starting may be using.
const gcMinAge = 2 * time.Minute

// onContainerStop drains the Tailscale Service backends of a stopping
// container. Docker sends the stop signal before it tears down the network,
// so callers move to other backends while the application can still finish
// its requests, instead of when the backend disappears.
func (d *Driver) onContainerStop(containerID string) {
	info, err := d.docker.ContainerInspect(
		d.ctx,
		containerID,
		dockerclient.ContainerInspectOptions{},
	)
	if err != nil || info.Container.NetworkSettings == nil {
		return
	}
	for _, settings := range info.Container.NetworkSettings.Networks {
		if settings == nil || !d.ownsNetwork(settings.NetworkID) {
			continue
		}
		d.mu.RLock()
		endpoint, ok := d.endpoints[settings.EndpointID]
		d.mu.RUnlock()
		if ok {
			d.wg.Go(endpoint.DrainService)
		}
	}
}

// collectGarbage removes state and sockets of endpoints that are gone without
// a Leave, as after a host crash. It runs once recovery knows every endpoint,
// and after each watchdog scan.
func (d *Driver) collectGarbage(ctx context.Context) {
	d.mu.RLock()
	endpoints := make([]*core.Endpoint, 0, len(d.endpoints))
	for _, ep := range d.endpoints {
		endpoints = append(endpoints, ep)
	}
	d.mu.RUnlock()

	// Endpoint methods take the endpoint's lock: never under driver.mu
	state := make(map[string]bool, len(endpoints))
	sockets := make(map[string]bool, len(endpoints))
	for _, ep := range endpoints {
		sockets[ep.ID[:12]] = true
		state[ep.GetStateDir()] = true
	}

	core.CollectGarbage(ctx, d.config, d.claims, state, sockets, gcMinAge)
}

// runWatchdog periodically scans for orphaned endpoints and recovers them,
// then collects garbage. This handles cases where containers restart after
// initial recovery.
func (d *Driver) runWatchdog(ctx context.Context) {
	// Wait before first check (let initial recovery complete)
	select {
	case <-ctx.Done():
		return
	case <-time.After(watchdogInterval):
	}

	ticker := time.NewTicker(watchdogInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Infof("Watchdog shutting down")
			return
		case <-ticker.C:
			if err := d.RecoverEndpoints(ctx); err != nil {
				logger.Errorf("Watchdog recovery failed: %v", err)
				continue
			}
			// Again here: state younger than gcMinAge survives the first run
			d.collectGarbage(ctx)
		}
	}
}

// recoverAttachment recovers the container's endpoint on a network, if the
// network is tslink's, and reports whether it recovered one the driver did not
// know. A known endpoint that joined and was never started missed its connect
// event: it starts Tailscale for it.
func (d *Driver) recoverAttachment(
	ctx context.Context,
	ownNames map[string]bool,
	containerID, netName, networkID, endpointID string,
) bool {
	// Check if this network uses our driver
	networkResult, err := d.docker.NetworkInspect(
		ctx,
		networkID,
		dockerclient.NetworkInspectOptions{},
	)
	if err != nil {
		logger.Debugf("RecoverEndpoints: failed to inspect network %s: %v", networkID[:12], err)
		return false
	}
	if !ownNames[networkResult.Network.Driver] {
		return false
	}

	// Check if we already have this endpoint
	d.mu.RLock()
	known, exists := d.endpoints[endpointID]
	d.mu.RUnlock()

	if exists {
		if known.GetSandboxKey() != "" && !known.StartRequested() {
			if err := d.startMissedEndpoint(
				ctx,
				containerID,
				netName,
				known,
				networkResult,
			); err != nil {
				logger.Errorf(
					"RecoverEndpoints: failed to start endpoint %s: %v",
					endpointID[:12],
					err,
				)
			}
		}
		return false
	}

	// Found orphaned endpoint - recover it
	logger.Infof("RecoverEndpoints: found orphaned endpoint %s for container %s on network %s",
		endpointID[:12], containerID[:12], netName)

	if err := d.recoverEndpoint(ctx, containerID, netName, endpointID, networkResult); err != nil {
		logger.Errorf("RecoverEndpoints: failed to recover endpoint %s: %v", endpointID[:12], err)
		return false
	}
	return true
}

// startMissedEndpoint starts Tailscale for an endpoint that joined but never
// saw its connect event, with the container's info from inspect.
func (d *Driver) startMissedEndpoint(
	ctx context.Context,
	containerID, netName string,
	endpoint *core.Endpoint,
	networkResult dockerclient.NetworkInspectResult,
) error {
	tsInfo, _, err := d.inspectContainer(ctx, containerID, netName, networkResult)
	if err != nil {
		return err
	}
	logger.Infof(
		"RecoverEndpoints: endpoint %s joined without its connect event, starting Tailscale (hostname=%s)",
		endpoint.ID[:12],
		tsInfo.Hostname,
	)
	d.cache.Store(endpoint.ID, tsInfo)
	d.startTailscale(endpoint, tsInfo)
	return nil
}

// inspectContainer returns the Tailscale configuration of a container on a
// tslink network, from its name and labels, and its network namespace.
func (d *Driver) inspectContainer(
	ctx context.Context,
	containerID, netName string,
	networkResult dockerclient.NetworkInspectResult,
) (*core.ContainerInfo, string, error) {
	containerInfo, err := d.docker.ContainerInspect(
		ctx,
		containerID,
		dockerclient.ContainerInspectOptions{},
	)
	if err != nil {
		return nil, "", fmt.Errorf("failed to inspect container: %w", err)
	}

	settings := containerInfo.Container.NetworkSettings
	if settings == nil {
		return nil, "", errors.New("container has no network settings")
	}
	// Verify container is attached to the network
	if _, ok := settings.Networks[netName]; !ok {
		return nil, "", fmt.Errorf("container not attached to network %s", netName)
	}

	// Sandbox key might be empty if container isn't fully running
	if settings.SandboxKey == "" {
		return nil, "", errors.New("container has no sandbox key (not fully started?)")
	}

	// Parse container info for Tailscale config
	name := strings.TrimPrefix(containerInfo.Container.Name, "/")

	labels := make(map[string]string)
	if containerInfo.Container.Config != nil && containerInfo.Container.Config.Labels != nil {
		labels = containerInfo.Container.Config.Labels
	}

	tsInfo := parseContainerInfo(name, labels)
	if containerInfo.Container.HostConfig != nil {
		tsInfo.DNS = containerInfo.Container.HostConfig.DNS
	}
	tsInfo.NetworkStack = networkResult.Network.Labels[core.StackLabel]
	return tsInfo, settings.SandboxKey, nil
}

// recoverEndpoint restores a single orphaned endpoint.
// It recreates the network and endpoint in driver state, then triggers Tailscale setup.
func (d *Driver) recoverEndpoint(
	ctx context.Context,
	containerID, netName, endpointID string,
	networkResult dockerclient.NetworkInspectResult,
) error {
	tsInfo, sandboxKey, err := d.inspectContainer(ctx, containerID, netName, networkResult)
	if err != nil {
		return err
	}

	// Ensure network exists in driver state
	net, err := d.adoptNetwork(networkResult)
	if err != nil {
		return err
	}

	// Create endpoint
	endpoint, err := core.NewEndpoint(
		endpointID,
		net,
		core.EndpointOptions{Hostname: tsInfo.Hostname},
		d.config,
		d.claims,
	)
	if err != nil {
		return fmt.Errorf("failed to create endpoint: %w", err)
	}

	// Store endpoint
	d.mu.Lock()
	// Double-check someone else didn't create it
	if _, exists := d.endpoints[endpointID]; exists {
		d.mu.Unlock()
		logger.Debugf("recoverEndpoint: endpoint %s already exists (race)", endpointID[:12])
		return nil
	}
	d.endpoints[endpointID] = endpoint
	d.mu.Unlock()

	// Before garbage collection runs, which would take the directory for unused
	if err := endpoint.ClaimStateDir(tsInfo); err != nil {
		logger.Warnf("recoverEndpoint: %v", err)
	}

	// Adopt what Join set up (normally done by Join). Without its routes the
	// task could reach the tailnet through the host, so do not start it.
	if err := d.recoverRouting(endpoint, sandboxKey); err != nil {
		d.mu.Lock()
		delete(d.endpoints, endpointID)
		d.mu.Unlock()
		// Give the state directory up: the container's next endpoint needs it
		if stopErr := endpoint.Stop(); stopErr != nil {
			logger.Warnf(
				"recoverEndpoint: failed to stop endpoint %s: %v",
				endpointID[:12],
				stopErr,
			)
		}
		return fmt.Errorf("failed to recover routing: %w", err)
	}

	// Store in cache for event handler
	d.cache.Store(endpointID, tsInfo)

	// Trigger Tailscale setup
	// Note: We don't recreate the veth pair - if networking is broken,
	// the container needs to be restarted anyway. We only recover Tailscale.
	logger.Infof(
		"recoverEndpoint: triggering Tailscale setup for endpoint %s (hostname=%s)",
		endpointID[:12],
		tsInfo.Hostname,
	)

	d.startTailscale(endpoint, tsInfo)

	return nil
}
