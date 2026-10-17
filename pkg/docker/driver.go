// Package docker implements the Docker network driver API and watches
// Docker events for container starts and stops.
package docker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"runtime/pprof"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/docker/go-plugins-helpers/network"
	dockernetwork "github.com/moby/moby/api/types/network"
	dockerclient "github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/stringid"

	"github.com/matchory/tslink/internal/ctxtime"
	"github.com/matchory/tslink/pkg/core"
	"github.com/matchory/tslink/pkg/logger"
	"github.com/matchory/tslink/pkg/netutil"
	"github.com/matchory/tslink/pkg/tailscale"
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
	runTailscale   func(e *core.Endpoint, ctx context.Context, info *core.ContainerInfo)
	leaveEndpoint  func(e *core.Endpoint)
	stopEndpoint   func(e *core.Endpoint)
	drainService   func(e *core.Endpoint)

	// Installs (true) or removes (false) the host's tailnet isolation, given
	// the interface names of Docker's bridge networks
	hostIsolation func(on bool, bridges []string) error
	// Returns the bridges the host's tailnet isolation covers as installed
	installedBridges func() ([]string, error)

	// Serializes the isolation's updates, which keep the bridge names it
	// covers in bridges: those Docker listed last, and at start those saved
	// in the data directory and those the isolation already covered
	isolationMu sync.Mutex
	bridges     []string

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

// watchdogInterval is the interval at which the watchdog checks for orphaned
// endpoints. A variable for tests.
var watchdogInterval = 60 * time.Second

// redactKey returns a redacted version of a key for safe logging.
func redactKey(key string) string {
	if key == "" {
		return "(empty)"
	}
	return fmt.Sprintf("(set, %d chars)", len(key))
}

// NewDriver creates a new Docker network driver.
func NewDriver() (*Driver, error) {
	cfg := core.LoadConfig()
	core.RemoveDownloadCache(cfg.DataDir)
	core.WarnUnusedCertificates(cfg.DataDir, cfg.SharedDir)

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
		networks:         make(map[string]*core.Network),
		endpoints:        make(map[string]*core.Endpoint),
		config:           cfg,
		claims:           core.NewStateClaims(),
		cache:            NewContainerCache(),
		docker:           docker,
		joinEndpoint:     (*core.Endpoint).Join,
		recoverRouting:   (*core.Endpoint).Recover,
		runTailscale:     (*core.Endpoint).RunTailscale,
		leaveEndpoint:    (*core.Endpoint).Leave,
		stopEndpoint:     (*core.Endpoint).Stop,
		drainService:     (*core.Endpoint).DrainService,
		hostIsolation:    applyHostIsolation,
		installedBridges: netutil.IsolatedBridges,
		ctx:              ctx,
		cancel:           cancel,
	}
}

// GetCapabilities returns the capabilities of the driver.
func (d *Driver) GetCapabilities() (*network.CapabilitiesResponse, error) {
	logger.Infof("GetCapabilities called")
	return &network.CapabilitiesResponse{
		Scope:             "global", // So Swarm carries driver options to nodes
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

	opts, err := core.ParseNetworkOptions(req.Options)
	if err != nil {
		return err
	}
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
	// Hand the options back so Swarm stores them as driver state
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

	if err := core.CheckEndpointOptions(req.Options); err != nil {
		return nil, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	var opts core.EndpointOptions
	if req.Interface != nil && req.Interface.Address != "" {
		prefix, err := netip.ParsePrefix(req.Interface.Address)
		if err != nil {
			logger.Warnf("Ignoring IPAM address %q: %v", req.Interface.Address, err)
		} else {
			opts.IPAMAddress = prefix.Addr()
		}
	}

	endpoint := core.NewEndpoint(req.EndpointID, net, opts, d.config, d.claims)

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

	d.stopEndpoint(endpoint)
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
			stringid.TruncateID(req.EndpointID),
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

	d.leaveEndpoint(endpoint)

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

	endpoints := d.endpointList()

	// tailscaled stops with the plugin. Drain Service backends first, so
	// callers use other nodes' backends until the plugin is back.
	var drains sync.WaitGroup
	for _, ep := range endpoints {
		drains.Go(func() { d.drainService(ep) })
	}
	drains.Wait()
	if len(endpoints) > 0 {
		ctxtime.Sleep(ctx, drainGrace)
	}

	// Each stop can take seconds, tailscaled's grace period: stop the
	// endpoints concurrently, and wait for them and the driver's goroutines
	// no longer than ctx allows
	done := make(chan struct{})
	go func() {
		var stops sync.WaitGroup
		for _, ep := range endpoints {
			stops.Go(func() {
				logger.Infof("Stopping endpoint %s", stringid.TruncateID(ep.ID))
				d.stopEndpoint(ep)
			})
		}
		stops.Wait()
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

	// List all running containers. Each listing is bounded: a scan holds
	// recoverMu, and one that waited for good would stop every later scan
	listCtx, cancel := context.WithTimeout(ctx, dockerCallTimeout)
	containerList, err := d.docker.ContainerList(listCtx, dockerclient.ContainerListOptions{})
	cancel()
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}

	// After the containers: a network their attachments name exists by now
	networks, err := d.ownNetworks(ctx, ownNames)
	if err != nil {
		return err
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
				networks,
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

	// Before the plugin serves requests: no container joins while the host's
	// tailnet is open to it. Without asking Docker for its bridges: after a
	// host reboot it answers no API call involving plugins until the plugin's
	// socket is open, and disables a plugin that does not open it in time.
	d.installHostIsolation()
	if d.config.IsolateHostTailnet {
		d.wg.Go(func() {
			// Every second until Docker lists its bridges, as recovery does
			err := d.updateHostIsolation()
			if err != nil {
				logger.Warnf("Host tailnet isolation: Docker's bridges unknown, "+
					"using the saved list until Docker answers: %v", err)
			}
			for err != nil && ctxtime.Sleep(ctx, time.Second) {
				err = d.updateHostIsolation()
			}
		})
	}

	// Start event watcher in background (tracked by waitgroup)
	// Pass callback to trigger Tailscale setup when container info arrives
	d.wg.Go(func() {
		WatchEvents(ctx, d.docker, d.cache, d.ownsNetwork, d.onContainerInfo, d.onContainerStop,
			d.applyHostIsolation)
	})

	// Recover orphaned endpoints from previous plugin instance (host reboot, plugin restart)
	// Run in background so plugin starts accepting requests immediately
	d.wg.Go(func() {
		// Every second until it works: tasks have no tailnet until recovered,
		// and the Docker API may still be starting after a host reboot
		for ctxtime.Sleep(ctx, time.Second) {
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
	ctx, cancel := context.WithTimeout(ctx, dockerCallTimeout)
	defer cancel()
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

// ownNetworks returns the networks whose driver is one of ownNames, by ID,
// from a single listing. Its entries carry all recovery uses of a network:
// its ID, driver, options and labels.
func (d *Driver) ownNetworks(
	ctx context.Context,
	ownNames map[string]bool,
) (map[string]dockerclient.NetworkInspectResult, error) {
	ctx, cancel := context.WithTimeout(ctx, dockerCallTimeout)
	defer cancel()
	list, err := d.docker.NetworkList(ctx, dockerclient.NetworkListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list networks: %w", err)
	}
	networks := make(map[string]dockerclient.NetworkInspectResult)
	for _, n := range list.Items {
		if ownNames[n.Driver] {
			networks[n.ID] = dockerclient.NetworkInspectResult{
				Network: dockernetwork.Inspect{Network: n.Network},
			}
		}
	}
	return networks, nil
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
			stringid.TruncateID(endpointID),
		)
		return
	}

	// Call endpoint methods without holding driver.mu
	if endpoint.IsTailscaleStarted() {
		logger.Debugf(
			"onContainerInfo: Tailscale already started for endpoint %s",
			stringid.TruncateID(endpointID),
		)
		return
	}

	if endpoint.SandboxKey() == "" {
		logger.Debugf(
			"onContainerInfo: endpoint %s has no sandbox key (Join not called yet)",
			stringid.TruncateID(endpointID),
		)
		return
	}

	logger.Infof(
		"onContainerInfo: triggering Tailscale setup for endpoint %s (hostname=%s)",
		stringid.TruncateID(endpointID),
		info.Hostname,
	)

	// Start Tailscale in a goroutine so we don't block the event handler
	d.startTailscale(endpoint, info)
}

// startTailscale starts Tailscale for the endpoint in the background, and
// records that it was asked to, so recovery leaves the endpoint to this start.
// The goroutines of the run, its supervisor and tailscaled carry the endpoint
// ID as a label, which tracebacks show.
func (d *Driver) startTailscale(endpoint *core.Endpoint, info *core.ContainerInfo) {
	endpoint.MarkStartRequested()
	labels := pprof.Labels("endpoint", stringid.TruncateID(endpoint.ID))
	d.wg.Go(func() {
		pprof.Do(d.ctx, labels, func(ctx context.Context) { d.runTailscale(endpoint, ctx, info) })
	})
}

// gcMinAge protects state that an endpoint still starting may be using.
const gcMinAge = 2 * time.Minute

// onContainerStop drains the Tailscale Service backends of a stopping
// container, given its endpoints on this driver's networks. Docker sends the
// stop signal before it tears down the network, so callers move to other
// backends while the application can still finish its requests, instead of
// when the backend disappears.
func (d *Driver) onContainerStop(_ string, endpointIDs []string) {
	for _, endpointID := range endpointIDs {
		d.mu.RLock()
		endpoint, ok := d.endpoints[endpointID]
		d.mu.RUnlock()
		if ok {
			d.wg.Go(func() { d.drainService(endpoint) })
		}
	}
}

// endpointList returns the driver's endpoints, for calling their methods
// after driver.mu is released.
func (d *Driver) endpointList() []*core.Endpoint {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return slices.Collect(maps.Values(d.endpoints))
}

// collectGarbage removes state and sockets of endpoints that are gone without
// a Leave, as after a host crash. It runs once recovery knows every endpoint,
// and after each watchdog scan.
func (d *Driver) collectGarbage(ctx context.Context) {
	// Endpoint methods take the endpoint's lock: never under driver.mu
	endpoints := d.endpointList()
	state := make(map[string]bool, len(endpoints))
	sockets := make(map[string]bool, len(endpoints))
	for _, ep := range endpoints {
		sockets[stringid.TruncateID(ep.ID)] = true
		state[ep.StateDir()] = true
	}

	core.CollectGarbage(ctx, d.config, d.claims, state, sockets, gcMinAge)
}

// applyHostIsolation installs the veth isolation, and installs or removes the
// host's tailnet isolation, as the plugin's settings say. Both stay in place
// when the plugin stops, so a restart or an upgrade never opens the host's
// tailnet, or other containers, to containers. The isolation covers the
// bridges of Docker's bridge networks by name: a stack file may name one
// otherwise than Docker would. If Docker cannot list them, it keeps those it
// covers.
func (d *Driver) applyHostIsolation() {
	d.isolationMu.Lock()
	defer d.isolationMu.Unlock()
	if d.config.IsolateHostTailnet {
		if err := d.listBridges(); err != nil {
			logger.Warnf(
				"Host tailnet isolation: Docker's bridges unknown, using the last list: %v",
				err,
			)
		}
	}
	d.setHostIsolation()
}

// updateHostIsolation is applyHostIsolation with the host's tailnet isolation
// on, but applies nothing if Docker cannot list its bridges: the list is as it
// was applied.
func (d *Driver) updateHostIsolation() error {
	d.isolationMu.Lock()
	defer d.isolationMu.Unlock()
	if err := d.listBridges(); err != nil {
		return err
	}
	d.setHostIsolation()
	return nil
}

// installHostIsolation is applyHostIsolation without a Docker API call: with
// the bridges saved in the data directory, and those the isolation already
// covers, so a missing or outdated list never narrows it.
func (d *Driver) installHostIsolation() {
	d.isolationMu.Lock()
	defer d.isolationMu.Unlock()
	if d.config.IsolateHostTailnet {
		installed, err := d.installedBridges()
		if err != nil {
			logger.Warnf("Host tailnet isolation: failed to read the bridges it covers: %v", err)
		}
		d.bridges = slices.Compact(slices.Sorted(slices.Values(
			append(d.loadBridges(), installed...),
		)))
	}
	d.setHostIsolation()
}

// listBridges sets bridges to those Docker lists, and saves them for the next
// start unless the saved list is the same; the caller holds isolationMu.
func (d *Driver) listBridges() error {
	bridges, err := bridgeNames(d.ctx, d.docker)
	if err != nil {
		return err
	}
	d.bridges = bridges
	// Against the file, not the last list: a failed save is retried
	if !slices.Equal(bridges, d.loadBridges()) {
		d.saveBridges(bridges)
	}
	return nil
}

// setHostIsolation applies the plugin's settings with d.bridges; the caller
// holds isolationMu.
func (d *Driver) setHostIsolation() {
	if err := d.hostIsolation(d.config.IsolateHostTailnet, d.bridges); err != nil {
		logger.Errorf("Host tailnet isolation (%s=%t): %v",
			core.IsolateHostTailnetSetting, d.config.IsolateHostTailnet, err)
	}
}

// loadBridges returns the bridge names saved in the data directory, none if
// there are none.
func (d *Driver) loadBridges() []string {
	data, err := os.ReadFile(filepath.Join(d.config.DataDir, bridgesFile))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logger.Warnf("Host tailnet isolation: failed to read the saved bridges: %v", err)
		}
		return nil
	}
	return strings.Fields(string(data))
}

// saveBridges saves the bridge names in the data directory, for the next
// start.
func (d *Driver) saveBridges(bridges []string) {
	if err := writeSynced(d.config.DataDir, bridgesFile, bridges); err != nil {
		logger.Warnf("Host tailnet isolation: failed to save the bridges: %v", err)
	}
}

// writeSynced writes lines to the file name in dir through a temporary file
// it renames, syncing both and the directory: after a crash, of the plugin
// or the host, the file holds either the old lines or the new ones.
func writeSynced(dir, name string, lines []string) error {
	f, err := os.CreateTemp(dir, name+".tmp*")
	if err != nil {
		return err
	}
	for _, l := range lines {
		if _, err = f.WriteString(l + "\n"); err != nil {
			break
		}
	}
	err = errors.Join(err, f.Sync(), f.Close())
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(dir, name))
	}
	if err != nil {
		if rmErr := os.Remove(f.Name()); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			logger.Warnf("Failed to remove %s: %v", f.Name(), rmErr)
		}
		return err
	}
	df, err := os.Open(dir) // #nosec G304 -- the plugin's data directory
	if err != nil {
		return err
	}
	return errors.Join(df.Sync(), df.Close())
}

// bridgesFile, in the data directory, holds the bridge names Docker listed
// last, one per line.
const bridgesFile = "bridges"

// bridgeNameOption is the driver option of a bridge network that names its
// bridge. Without it, Docker names the bridge br-<network ID>.
const bridgeNameOption = "com.docker.network.bridge.name"

// bridgeNames returns the bridge names Docker's bridge networks set, the
// default network's docker0 among them.
func bridgeNames(ctx context.Context, cli dockerAPI) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, dockerCallTimeout)
	defer cancel()
	res, err := cli.NetworkList(ctx, dockerclient.NetworkListOptions{
		Filters: dockerclient.Filters{}.Add("driver", "bridge"),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list networks: %w", err)
	}
	var names []string
	for _, n := range res.Items {
		if name := n.Options[bridgeNameOption]; n.Driver == "bridge" && name != "" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names, nil
}

// applyHostIsolation is the default Driver.hostIsolation. The veth isolation
// is always on: TSLINK_ISOLATE_HOST_TAILNET concerns the host's tailnet only.
func applyHostIsolation(on bool, bridges []string) error {
	veth := netutil.SetupVethIsolation(tailscale.WireGuardPort)
	if veth != nil {
		veth = fmt.Errorf("veth isolation: %w", veth)
	}
	if on {
		return errors.Join(veth, netutil.SetupHostIsolation(bridges))
	}
	return errors.Join(veth, netutil.RemoveHostIsolation())
}

// runWatchdog periodically scans for orphaned endpoints and recovers them,
// then collects garbage. This handles cases where containers restart after
// initial recovery.
func (d *Driver) runWatchdog(ctx context.Context) {
	// The first check is one interval in, after the initial recovery
	ticker := time.NewTicker(watchdogInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Infof("Watchdog shutting down")
			return
		case <-ticker.C:
			if ctx.Err() != nil {
				continue // shutting down: Done wins the next select
			}
			// Again here: restores the rules if something removed them
			d.applyHostIsolation()
			if err := d.RecoverEndpoints(ctx); err != nil {
				if ctx.Err() != nil {
					logger.Infof("Watchdog shutting down, recovery cut short: %v", err)
					return
				}
				logger.Errorf("Watchdog recovery failed: %v", err)
				continue
			}
			// Again here: state younger than gcMinAge survives the first run
			d.collectGarbage(ctx)
		}
	}
}

// recoverAttachment recovers the container's endpoint on a network, if the
// network is tslink's, one of networks, and reports whether it recovered one
// the driver did not know. A known endpoint that joined and was never started
// missed its connect event: it starts Tailscale for it.
func (d *Driver) recoverAttachment(
	ctx context.Context,
	networks map[string]dockerclient.NetworkInspectResult,
	containerID, netName, networkID, endpointID string,
) bool {
	networkResult, ok := networks[networkID]
	if !ok {
		return false
	}

	// Check if we already have this endpoint
	d.mu.RLock()
	known, exists := d.endpoints[endpointID]
	d.mu.RUnlock()

	if exists {
		if known.SandboxKey() != "" && !known.StartRequested() {
			if err := d.startMissedEndpoint(
				ctx,
				containerID,
				netName,
				known,
				networkResult,
			); err != nil {
				logger.Errorf(
					"RecoverEndpoints: failed to start endpoint %s: %v",
					stringid.TruncateID(endpointID),
					err,
				)
			}
		}
		return false
	}

	// Found orphaned endpoint - recover it
	logger.Infof("RecoverEndpoints: found orphaned endpoint %s for container %s on network %s",
		stringid.TruncateID(endpointID), stringid.TruncateID(containerID), netName)

	if err := d.recoverEndpoint(ctx, containerID, netName, endpointID, networkResult); err != nil {
		logger.Errorf(
			"RecoverEndpoints: failed to recover endpoint %s: %v",
			stringid.TruncateID(endpointID),
			err,
		)
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
	a, err := d.inspectContainer(ctx, containerID, netName, networkResult)
	if err != nil {
		return err
	}
	tsInfo := a.info
	logger.Infof(
		"RecoverEndpoints: endpoint %s joined without its connect event, starting Tailscale (hostname=%s)",
		stringid.TruncateID(endpoint.ID),
		tsInfo.Hostname,
	)
	d.cache.Store(endpoint.ID, tsInfo)
	d.startTailscale(endpoint, tsInfo)
	return nil
}

// inspectedAttachment is a container's attachment to a tslink network, as
// inspected.
type inspectedAttachment struct {
	info        *core.ContainerInfo // the container's Tailscale configuration
	sandboxKey  string              // its network namespace
	ipamAddress netip.Addr          // Docker's IPAM address for it on the network, if any
}

// inspectContainer returns the Tailscale configuration of a container on a
// tslink network, from its name and labels, its network namespace and its
// IPAM address on the network.
func (d *Driver) inspectContainer(
	ctx context.Context,
	containerID, netName string,
	networkResult dockerclient.NetworkInspectResult,
) (inspectedAttachment, error) {
	containerInfo, err := d.docker.ContainerInspect(
		ctx,
		containerID,
		dockerclient.ContainerInspectOptions{},
	)
	if err != nil {
		return inspectedAttachment{}, fmt.Errorf("failed to inspect container: %w", err)
	}

	settings := containerInfo.Container.NetworkSettings
	if settings == nil {
		return inspectedAttachment{}, errors.New("container has no network settings")
	}
	// Verify container is attached to the network
	netSettings, ok := settings.Networks[netName]
	if !ok || netSettings == nil {
		return inspectedAttachment{}, fmt.Errorf("container not attached to network %s", netName)
	}

	// Sandbox key might be empty if container isn't fully running
	if settings.SandboxKey == "" {
		return inspectedAttachment{}, errors.New(
			"container has no sandbox key (not fully started?)",
		)
	}

	a := inspectedAttachment{
		info: inspectedContainerInfo(
			&containerInfo.Container,
			networkResult.Network.Labels[core.StackLabel],
		),
		sandboxKey: settings.SandboxKey,
	}
	addDaemonDNS(ctx, d.docker, a.info)
	a.ipamAddress = netSettings.IPAddress
	return a, nil
}

// recoverEndpoint restores a single orphaned endpoint.
// It recreates the network and endpoint in driver state, then triggers Tailscale setup.
func (d *Driver) recoverEndpoint(
	ctx context.Context,
	containerID, netName, endpointID string,
	networkResult dockerclient.NetworkInspectResult,
) error {
	a, err := d.inspectContainer(ctx, containerID, netName, networkResult)
	if err != nil {
		return err
	}
	tsInfo, sandboxKey := a.info, a.sandboxKey

	// Ensure network exists in driver state
	net, err := d.adoptNetwork(networkResult)
	if err != nil {
		return err
	}

	// Create endpoint
	endpoint := core.NewEndpoint(
		endpointID,
		net,
		// Recover puts the IPAM address on the container's veth, as Join
		// does: an older version may not have
		core.EndpointOptions{Hostname: tsInfo.Hostname, IPAMAddress: a.ipamAddress},
		d.config,
		d.claims,
	)

	// Store endpoint
	d.mu.Lock()
	// Double-check someone else didn't create it
	if _, exists := d.endpoints[endpointID]; exists {
		d.mu.Unlock()
		logger.Debugf(
			"recoverEndpoint: endpoint %s already exists (race)",
			stringid.TruncateID(endpointID),
		)
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
		d.stopEndpoint(endpoint)
		return fmt.Errorf("failed to recover routing: %w", err)
	}

	// Store in cache for event handler
	d.cache.Store(endpointID, tsInfo)

	// Trigger Tailscale setup
	// Note: We don't recreate the veth pair - if networking is broken,
	// the container needs to be restarted anyway. We only recover Tailscale.
	logger.Infof(
		"recoverEndpoint: triggering Tailscale setup for endpoint %s (hostname=%s)",
		stringid.TruncateID(endpointID),
		tsInfo.Hostname,
	)

	d.startTailscale(endpoint, tsInfo)

	return nil
}
