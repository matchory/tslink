// Package core holds the endpoint and network logic: the per-container
// Tailscale lifecycle, its state directories and their garbage collection.
package core

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/docker/go-plugins-helpers/network"

	"github.com/matchory/tslink/pkg/logger"
	"github.com/matchory/tslink/pkg/netutil"
	"github.com/matchory/tslink/pkg/tailscale"
)

// Endpoint represents a container endpoint with Tailscale connectivity.
type Endpoint struct {
	mu sync.RWMutex // Protects concurrent access to mutable fields

	ID          string
	Network     *Network
	Hostname    string
	Tags        []string                  // ACL tags for tailscale up --advertise-tags
	Service     string                    // Service name (e.g., "svc:hello-world")
	Endpoints   []tailscale.ServeEndpoint // Parsed tslink.serve.<port> labels
	Direct      bool                      // Serve the endpoints on the task's own name too
	TailscaleIP string
	VethName    string
	StateDir    string
	SandboxKey  string // Container's network namespace path, stored during Join
	DataDir     string // Base data directory for state
	SharedDir   string // Directory shared between hosts; empty for none

	claims           *StateClaims // The driver's state directory claims
	supervisor       *tailscale.DaemonSupervisor
	tailscaleStarted bool // Whether Tailscale setup has been completed
	startRequested   bool // Whether the driver has asked to start Tailscale

	running bool                       // Whether RunTailscale is active
	leaving bool                       // Leave or Stop began: RunTailscale starts nothing until a Join
	runCtx  context.Context            //nolint:containedctx // Cancelled when the endpoint leaves
	stopRun context.CancelFunc         // Cancels runCtx
	startFn func(*ContainerInfo) error // Replaces StartTailscale in tests

	health      *health                                   // Readiness endpoint; nil without tslink.health
	readinessFn func(context.Context) tailscale.Readiness // Replaces the supervisor's readiness in tests

	statusMu sync.Mutex               // Protects status and warnings; never held with mu
	status   *EndpointStatusFile      // Last status written; nil before the first, after Leave
	warnings map[string]StatusWarning // Reported by tailscaled's supervision, by key
}

// Delays between attempts to start Tailscale; variables so tests can shorten them.
var (
	startRetryInitial = 5 * time.Second
	startRetryMax     = 2 * time.Minute
)

// permanentError marks a start failure that retrying cannot fix, such as a
// container from another stack or an invalid hostname.
type permanentError struct{ error }

func (e permanentError) Unwrap() error { return e.error }

// ContainerInfo holds information extracted from Docker container inspection.
type ContainerInfo struct {
	Name      string                    // Container name (without leading /)
	Labels    map[string]string         // All container labels
	Hostname  string                    // Parsed tslink.hostname label or container name
	Tags      []string                  // Parsed tslink.tags label (comma-separated)
	Service   string                    // Parsed tslink.service label (e.g., "svc:hello-world")
	Endpoints []tailscale.ServeEndpoint // Parsed tslink.serve.<port> labels
	Direct    bool                      // tslink.direct; defaults to true without a Service

	Stack        string // Container's com.docker.stack.namespace label
	NetworkStack string // Network's com.docker.stack.namespace label

	DNS []netip.Addr // DNS servers the container was given, as with docker run --dns

	HealthPort int // tslink.health: loopback port of the readiness endpoint; 0 for none
}

// StackLabel is the label docker stack deploy puts on a stack's services,
// containers and networks. It overrides any value from the compose file.
const StackLabel = "com.docker.stack.namespace"

// NewEndpoint creates a new endpoint with the given configuration. It claims
// state directories in claims.
func NewEndpoint(
	id string,
	net *Network,
	opts EndpointOptions,
	cfg *Config,
	claims *StateClaims,
) (*Endpoint, error) {
	hostname := opts.Hostname
	if hostname == "" {
		// Use short endpoint ID as default hostname (will be updated when container info arrives)
		hostname = id[:12]
	}

	// Use hostname-based state directory for Tailscale identity reuse
	// Same hostname = same Tailscale node, enabling state reuse on container restart
	stateDir := filepath.Join(cfg.DataDir, "by-hostname", hostname)

	return &Endpoint{
		ID:        id,
		Network:   net,
		Hostname:  hostname,
		Direct:    true, // Default to direct serve enabled
		StateDir:  stateDir,
		DataDir:   cfg.DataDir,
		SharedDir: cfg.SharedDir,
		claims:    claims,
	}, nil
}

// vethSlots is the number of /30 subnets for veth pairs in 10.200.0.0/16,
// leaving out the first and the last few.
const vethSlots = 16380

// vethSlot returns the endpoint's preferred subnet, 1 to vethSlots, from the
// first 8 characters of its ID.
func vethSlot(endpointID string) int {
	var hash uint16
	for i := 0; i < len(endpointID) && i < 8; i++ {
		hash = hash*31 + uint16(endpointID[i])
	}
	return int(hash%vethSlots) + 1
}

// vethSlotIPs returns the host and container addresses of a subnet.
func vethSlotIPs(slot int) (netip.Addr, netip.Addr) {
	base := slot * 4 // Each /30 has 4 IPs: network, host, container, broadcast
	//nolint:gosec // base is at most vethSlots*4, so both bytes fit
	host := netip.AddrFrom4([4]byte{10, 200, byte(base / 256), byte(base%256 + 1)})
	return host, host.Next()
}

// generateVethIPs returns the addresses of the endpoint's preferred subnet.
func generateVethIPs(endpointID string) (string, string) {
	host, ctr := vethSlotIPs(vethSlot(endpointID))
	return host.String(), ctr.String()
}

// pickVethSubnet returns the addresses of the first subnet, from the
// endpoint's preferred one on, that used does not contain. The preferred
// subnets of about 150 endpoints on one host collide with even odds, and
// with one subnet on two host veths, the host routes the second container's
// traffic to the first.
func pickVethSubnet(endpointID string, used map[netip.Prefix]bool) (netip.Addr, netip.Addr, error) {
	start := vethSlot(endpointID)
	for i := range vethSlots {
		host, ctr := vethSlotIPs((start-1+i)%vethSlots + 1)
		if !used[netip.PrefixFrom(host, 30).Masked()] {
			return host, ctr, nil
		}
	}
	return netip.Addr{}, netip.Addr{}, errors.New("no free veth subnet in 10.200.0.0/16")
}

// vethAllocMu makes choosing a veth subnet and assigning it to the host veth
// one step, so concurrent Joins do not pick the same one.
var vethAllocMu sync.Mutex

// assignVethSubnet picks a subnet no host link uses and assigns its host
// address to vethHost.
func assignVethSubnet(endpointID, vethHost string) (string, string, error) {
	vethAllocMu.Lock()
	defer vethAllocMu.Unlock()
	used, err := netutil.UsedVethSubnets()
	if err != nil {
		return "", "", err
	}
	host, ctr, err := pickVethSubnet(endpointID, used)
	if err != nil {
		return "", "", err
	}
	if err := netutil.SetupHostRouting(vethHost, host.String()); err != nil {
		return "", "", err
	}
	return host.String(), ctr.String(), nil
}

// Join is called when a container joins the network.
// It sets up basic networking (veth, IPs, routing, NAT) and stores the sandbox key.
// Tailscale setup is deferred until container info is available via Docker events.
// This allows Join() to return quickly while Tailscale is configured asynchronously.
func (e *Endpoint) Join(sandboxKey string) (*network.JoinResponse, error) {
	// NOTE: We intentionally do NOT hold the lock during network operations.
	// Network syscalls can block for seconds, which would block all other endpoint operations.
	// We only lock briefly to read/write state.

	logger.Infof("Endpoint %s joining with sandbox %s", e.ID, sandboxKey)

	// Create veth pair (blocking network operation - no lock held)
	vethHost, vethContainer, err := netutil.CreateVethPair(e.ID[:8], e.Network.MTU)
	if err != nil {
		return nil, fmt.Errorf("failed to create veth pair: %w", err)
	}

	logger.Infof("Created veth pair: host=%s container=%s", vethHost, vethContainer)

	// Move container end of veth into container's network namespace
	if err := netutil.MoveToNetNS(vethContainer, sandboxKey); err != nil {
		if cleanupErr := netutil.DeleteVeth(vethHost); cleanupErr != nil {
			logger.Warnf("failed to cleanup veth %s after error: %v", vethHost, cleanupErr)
		}
		return nil, fmt.Errorf("failed to move veth to container ns: %w", err)
	}

	// Set up host side of veth with IP
	hostVethIP, containerVethIP, err := assignVethSubnet(e.ID, vethHost)
	if err != nil {
		if cleanupErr := netutil.DeleteVeth(vethHost); cleanupErr != nil {
			logger.Warnf("failed to cleanup veth %s after error: %v", vethHost, cleanupErr)
		}
		return nil, fmt.Errorf("failed to set up host routing: %w", err)
	}
	logger.Infof("Using veth IPs: host=%s container=%s", hostVethIP, containerVethIP)

	// Set up container side with IP, bring it up, and add default route
	if err := netutil.SetupInterfaceInNS(sandboxKey, vethContainer, ""); err != nil {
		if cleanupErr := netutil.DeleteVeth(vethHost); cleanupErr != nil {
			logger.Warnf("failed to cleanup veth %s after error: %v", vethHost, cleanupErr)
		}
		return nil, fmt.Errorf("failed to set up container interface: %w", err)
	}

	if err := netutil.SetupContainerRouting(
		sandboxKey,
		vethContainer,
		containerVethIP,
		hostVethIP,
	); err != nil {
		if cleanupErr := netutil.DeleteVeth(vethHost); cleanupErr != nil {
			logger.Warnf("failed to cleanup veth %s after error: %v", vethHost, cleanupErr)
		}
		return nil, fmt.Errorf("failed to set up container routing: %w", err)
	}

	// tailscaled's own traffic must not depend on the container's other networks
	if err := netutil.SetupBypassRoute(sandboxKey, vethContainer, hostVethIP); err != nil {
		if cleanupErr := netutil.DeleteVeth(vethHost); cleanupErr != nil {
			logger.Warnf("failed to cleanup veth %s after error: %v", vethHost, cleanupErr)
		}
		return nil, fmt.Errorf("failed to route tailscaled via veth: %w", err)
	}

	// Fail closed: tailnet traffic must not fall through to the host's tailscaled
	if err := netutil.SetupTailnetBlackhole(sandboxKey); err != nil {
		if cleanupErr := netutil.DeleteVeth(vethHost); cleanupErr != nil {
			logger.Warnf("failed to cleanup veth %s after error: %v", vethHost, cleanupErr)
		}
		return nil, fmt.Errorf("failed to blackhole tailnet ranges: %w", err)
	}

	// Set up NAT/MASQUERADE for internet access
	if err := netutil.SetupNAT(vethHost); err != nil {
		logger.Warnf("Failed to set up NAT: %v", err)
		// Continue anyway - tailscaled might still work via DERP
	}

	// Now lock briefly to store results
	e.mu.Lock()
	e.SandboxKey = sandboxKey
	e.VethName = vethHost
	e.leaving = false
	e.mu.Unlock()

	logger.Infof(
		"Network setup complete for endpoint %s, Tailscale will be configured when container info arrives",
		e.ID[:12],
	)

	// Return immediately - Tailscale setup will be triggered by Docker event.
	// Docker provides the gateway: without one, its embedded DNS server
	// considers the container cut off and does not resolve public names.
	return &network.JoinResponse{}, nil
}

// validHostname matches hostnames that are safe to use as a directory name:
// container names, Swarm task names (which contain dots) and DNS labels.
var validHostname = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)

// stateDirFor returns the tailscaled state directory for a container.
// Stack tasks get <data>/by-stack/<stack>/<hostname>, so one stack cannot
// reuse or wipe another's state by choosing a hostname; other containers
// keep <data>/by-hostname/<hostname>. Both names may come from labels, so
// neither may name a path outside its directory.
func stateDirFor(dataDir, stack, hostname string) (string, error) {
	if !validHostname.MatchString(hostname) {
		return "", fmt.Errorf("invalid hostname %q", hostname)
	}
	if stack == "" {
		return filepath.Join(dataDir, "by-hostname", hostname), nil
	}
	if !validHostname.MatchString(stack) {
		return "", fmt.Errorf("invalid stack name %q", stack)
	}
	return filepath.Join(dataDir, "by-stack", stack, hostname), nil
}

// socketPathFor returns the tailscaled socket path for an endpoint. It is kept
// out of the state directory because Unix socket paths are limited to 108
// bytes, and stack and hostname can make the state directory longer than that.
func socketPathFor(dataDir, endpointID string) string {
	return filepath.Join(dataDir, "sock", endpointID[:12]+".sock")
}

// StartTailscale starts the Tailscale daemon with the provided container info.
// This is called by the event handler when container info becomes available.
// It uses the correct hostname and state directory for identity reuse.
func (e *Endpoint) StartTailscale(info *ContainerInfo) error {
	// NOTE: We intentionally do NOT hold the lock during blocking operations.
	// Binary download can take seconds, supervisor startup can take 90s, WaitForIP can take 60s.
	// We only lock briefly to check/update state.

	// Quick check if already started (avoids duplicate work)
	e.mu.Lock()
	if e.tailscaleStarted {
		e.mu.Unlock()
		logger.Debugf("Tailscale already started for endpoint %s", e.ID[:12])
		return nil
	}
	if e.supervisor != nil {
		e.mu.Unlock()
		logger.Debugf("Tailscale startup already in progress for endpoint %s", e.ID[:12])
		return nil
	}
	sandboxKey := e.SandboxKey
	if sandboxKey == "" {
		e.mu.Unlock()
		return errors.New("no sandbox key set - Join() must be called first")
	}

	// Copy immutable config needed for supervisor creation
	endpointID := e.ID
	dataDir := e.DataDir
	certsDir := ""
	if e.SharedDir != "" {
		certsDir = filepath.Join(e.SharedDir, "certs")
	}
	network := e.Network
	tags := e.tagsFor(info)
	e.mu.Unlock()

	if err := checkStackScope(info, network, tags); err != nil {
		return permanentError{err}
	}
	authKey := func() (string, error) { return network.Credential(dataDir) }

	stateDir, err := e.prepareStateDir(info, network)
	if err != nil {
		return err
	}

	logger.Infof("StartTailscale: endpoint=%s hostname=%s service=%s endpoints=%d direct=%v",
		endpointID[:12], info.Hostname, info.Service, len(info.Endpoints), info.Direct)

	// Validate service configuration
	if info.Service != "" && len(info.Endpoints) == 0 {
		return permanentError{
			errors.New("tslink.service requires at least one tslink.serve.<port> endpoint"),
		}
	}

	tailscaleBin, tailscaledBin, err := tailscale.BundledBinaries()
	if err != nil {
		return err
	}

	// Create supervisor (handles daemon lifecycle with auto-recovery)
	supervisor := tailscale.NewDaemonSupervisor(tailscale.DaemonConfig{
		EndpointID:    endpointID,
		StateDir:      stateDir,
		SocketPath:    socketPathFor(dataDir, endpointID),
		Hostname:      info.Hostname,
		AuthKey:       authKey,
		CertsDir:      certsDir,
		NetNSPath:     sandboxKey,
		TailscaleBin:  tailscaleBin,
		TailscaledBin: tailscaledBin,
		Tags:          tags,
		Service:       info.Service,
		Endpoints:     info.Endpoints,
		Direct:        info.Direct,
		LoginServer:   e.Network.LoginServer, // set once, at creation
		ContainerDNS:  info.DNS,
		Warn:          e.setWarning,
	})

	tailscaleIP, err := e.startSupervisor(supervisor)
	if err != nil {
		return err
	}

	// Save auth key hash for future comparisons
	if !network.UsesClusterCredential() {
		if err := tailscale.SaveAuthKeyHash(stateDir, network.AuthKey); err != nil {
			logger.Warnf("Failed to save auth key hash: %v", err)
		}
	}

	// Now lock briefly to store results
	e.mu.Lock()
	// Double-check we didn't race with another caller
	if e.tailscaleStarted {
		e.mu.Unlock()
		if stopErr := supervisor.Stop(); stopErr != nil {
			logger.Warnf("failed to stop duplicate supervisor: %v", stopErr)
		}
		logger.Warnf("Tailscale was started by another goroutine for endpoint %s", endpointID[:12])
		return nil
	}
	e.supervisor = supervisor
	e.TailscaleIP = tailscaleIP
	e.Hostname = info.Hostname
	e.Tags = tags
	e.Service = info.Service
	e.Endpoints = info.Endpoints
	e.Direct = info.Direct
	e.StateDir = stateDir
	e.tailscaleStarted = true
	e.mu.Unlock()

	logger.Infof(
		"Endpoint %s got Tailscale IP: %s (hostname=%s)",
		endpointID[:12],
		tailscaleIP,
		info.Hostname,
	)
	return nil
}

// RunTailscale starts Tailscale for the endpoint, retrying with backoff until
// it succeeds, fails in a way retrying cannot fix, or the endpoint leaves. A
// transient failure, such as the control plane being unreachable when the task
// starts, must not leave the task without its identity for good. Only one run
// is active per endpoint, however often it is triggered, and none once the
// endpoint has begun to leave.
func (e *Endpoint) RunTailscale(info *ContainerInfo) {
	e.mu.Lock()
	if e.running || e.tailscaleStarted || e.leaving {
		e.mu.Unlock()
		return
	}
	if e.runCtx == nil {
		e.runCtx, e.stopRun = context.WithCancel(context.Background())
	}
	ctx := e.runCtx
	start := e.startFn
	if start == nil {
		start = e.StartTailscale
	}
	e.running = true
	e.mu.Unlock()

	defer func() {
		e.mu.Lock()
		e.running = false
		e.mu.Unlock()
		if ctx.Err() != nil {
			// Left while starting: Leave left the state directory, and a
			// supervisor stored after it looked, to the run
			logger.Infof("Endpoint %s left while Tailscale started, stopping it", e.ID[:12])
			e.stopTailscale() //nolint:contextcheck // ctx is done: cleanup outlives it
		}
	}()

	// Before Tailscale, so the container's healthcheck sees it start, or fail
	e.startHealth(info)

	delay := startRetryInitial
	for attempt := 1; ; attempt++ {
		err := start(info)
		if err == nil {
			e.writeStatus(info, StatusRunning, attempt-1, nil)
			return
		}
		if ctx.Err() != nil {
			return
		}
		if _, ok := errors.AsType[permanentError](err); ok {
			logger.Errorf("Endpoint %s: cannot start Tailscale: %v", e.ID[:12], err)
			e.writeStatus(info, StatusFailed, attempt, err)
			return
		}
		e.writeStatus(info, StatusRetrying, attempt, err)
		logger.Errorf("Endpoint %s: starting Tailscale failed (attempt %d), retrying in %v: %v",
			e.ID[:12], attempt, delay, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, startRetryMax)
	}
}

// ClaimStateDir records the state directory the container's tailscaled
// uses, so garbage collection leaves it alone. A directory serves one
// endpoint at a time: two replicas with the same tslink.hostname on a node
// would otherwise share one node key. An invalid name claims nothing;
// starting fails on it.
func (e *Endpoint) ClaimStateDir(info *ContainerInfo) error {
	dir, err := stateDirFor(e.DataDir, info.Stack, info.Hostname)
	if err != nil {
		return nil //nolint:nilerr // an invalid name claims nothing: starting fails on it
	}

	if owner, ok := e.claims.claim(dir, e.ID); !ok {
		if owner == gcClaim {
			return fmt.Errorf("state directory %s is being garbage collected", dir)
		}
		return fmt.Errorf(
			"state directory %s is in use by endpoint %s: is tslink.hostname %q set on a replicated service?",
			dir,
			owner[:12],
			info.Hostname,
		)
	}

	e.mu.Lock()
	e.StateDir = dir
	e.mu.Unlock()
	return nil
}

// GetStateDir returns the endpoint's state directory safely.
func (e *Endpoint) GetStateDir() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.StateDir
}

// IsTailscaleStarted returns whether Tailscale has been started for this endpoint.
func (e *Endpoint) IsTailscaleStarted() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.tailscaleStarted
}

// MarkStartRequested records that the driver asked to start Tailscale for
// the endpoint, so recovery leaves it to that start.
func (e *Endpoint) MarkStartRequested() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.startRequested = true
}

// StartRequested reports whether the driver asked to start Tailscale for the
// endpoint. One that joined and was never asked missed its connect event.
func (e *Endpoint) StartRequested() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.startRequested
}

// GetSandboxKey returns the sandbox key (container netns path) safely.
func (e *Endpoint) GetSandboxKey() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.SandboxKey
}

// Recover adopts an endpoint whose Join ran in an earlier plugin instance:
// it records the namespace and the veth Join created, so Leave cleans them
// up, and reapplies the routes Join installs, which an older version may not
// have.
func (e *Endpoint) Recover(sandboxKey string) error {
	vethHost, vethContainer := "veth"+e.ID[:8], "veth"+e.ID[:8]+"c"
	// Join may have picked another subnet than the preferred one
	hostVethIP, err := netutil.VethIPv4(vethHost)
	if err != nil {
		return fmt.Errorf("failed to read the address of %s: %w", vethHost, err)
	}

	e.mu.Lock()
	e.SandboxKey = sandboxKey
	e.VethName = vethHost
	e.mu.Unlock()

	if err := netutil.SetupBypassRoute(sandboxKey, vethContainer, hostVethIP); err != nil {
		return fmt.Errorf("failed to route tailscaled via veth: %w", err)
	}
	if err := netutil.SetupTailnetBlackhole(sandboxKey); err != nil {
		return fmt.Errorf("failed to blackhole tailnet ranges: %w", err)
	}
	return nil
}

// GetInfo returns TailscaleIP and Hostname safely for status queries.
func (e *Endpoint) GetInfo() (string, string) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.TailscaleIP, e.Hostname
}

// Leave is called when a container leaves the network.
// It stops the Tailscale supervisor and cleans up networking resources.
func (e *Endpoint) Leave() error {
	// From now on a late connect event or watchdog start starts nothing:
	// Tailscale started after stopTailscale would run until DeleteEndpoint
	e.mu.Lock()
	e.leaving = true
	vethName := e.VethName
	sandboxKey := e.SandboxKey
	e.mu.Unlock()

	e.cancelRun()

	logger.Infof("Endpoint %s leaving", e.ID)

	// Stop the Tailscale supervisor first
	e.stopTailscale()
	e.stopHealth()
	e.removeStatus()

	// Clean up NAT rules for this veth
	if vethName != "" {
		if err := netutil.CleanupNAT(vethName); err != nil {
			logger.Warnf("Failed to cleanup NAT for %s: %v", vethName, err)
		}
	}

	if sandboxKey != "" {
		if err := netutil.CleanupTailnetBlackhole(sandboxKey); err != nil {
			logger.Warnf("Failed to remove tailnet blackhole routes: %v", err)
		}
	}

	// Clean up veth (this also removes the container-side interface)
	if vethName != "" {
		if err := netutil.DeleteVeth(vethName); err != nil {
			logger.Warnf("Failed to delete veth %s: %v", vethName, err)
		}
	}

	return nil
}

// DrainService stops new connections to the endpoint's Tailscale Service
// backend, if it has one; existing connections continue.
func (e *Endpoint) DrainService() {
	e.mu.RLock()
	supervisor, service := e.supervisor, e.Service
	e.mu.RUnlock()

	if supervisor == nil || service == "" {
		return
	}
	logger.Infof("Endpoint %s stopping, draining %s", e.ID[:12], service)
	if err := supervisor.Drain(service); err != nil {
		logger.Warnf("Failed to drain %s for endpoint %s: %v", service, e.ID[:12], err)
	}
}

// Stop stops the endpoint and cleans up resources.
func (e *Endpoint) Stop() error {
	// A start queued before the endpoint was deleted must start nothing:
	// nothing would stop it again
	e.mu.Lock()
	e.leaving = true
	e.mu.Unlock()
	e.cancelRun()
	e.stopHealth()

	e.mu.Lock()
	supervisor := e.supervisor
	// A start in flight releases the state directory when it returns
	inFlight := supervisor == nil && e.running
	e.supervisor = nil
	e.tailscaleStarted = false
	e.mu.Unlock()
	if !inFlight {
		defer e.releaseStateDir()
	}

	logger.Infof("Stopping endpoint %s", e.ID)

	if supervisor != nil {
		if err := supervisor.Stop(); err != nil {
			logger.Warnf("Failed to stop supervisor: %v", err)
		}
	}

	return nil
}

// startSupervisor starts the supervisor and waits for the node's Tailscale
// IP, stopping the supervisor again if either fails. It starts nothing for an
// endpoint that is leaving, and Leave aborts a start in progress, its login
// included.
func (e *Endpoint) startSupervisor(supervisor *tailscale.DaemonSupervisor) (string, error) {
	e.mu.RLock()
	leaving, ctx := e.leaving, e.runCtx
	e.mu.RUnlock()
	if leaving {
		return "", errors.New("endpoint is leaving")
	}
	if ctx == nil {
		ctx = context.Background() // Started outside RunTailscale
	}

	// Start supervisor (blocks until initial daemon startup succeeds or fails - up to 90s!)
	if err := supervisor.StartContext(ctx); err != nil {
		// The supervisor keeps retrying after a failed first start. Nothing
		// would stop it later, since it is not stored on the endpoint.
		if stopErr := supervisor.Stop(); stopErr != nil {
			logger.Warnf("failed to stop supervisor after start error: %v", stopErr)
		}
		return "", fmt.Errorf("failed to start tailscale supervisor: %w", err)
	}

	// Wait for Tailscale to connect and get IP (can take up to 60s!)
	status, err := supervisor.WaitForIP(ctx)
	if err != nil {
		if stopErr := supervisor.Stop(); stopErr != nil {
			logger.Warnf("failed to stop supervisor after WaitForIP error: %v", stopErr)
		}
		return "", fmt.Errorf("failed to get Tailscale IP: %w", err)
	}
	return status.IP, nil
}

// checkStackScope keeps a task to its own stack's network: another stack
// cannot attach to it and take its credentials and tags. A stack using the
// cluster credential is also kept to its own tags.
func checkStackScope(info *ContainerInfo, network *Network, tags []string) error {
	if info.NetworkStack != "" && info.Stack != info.NetworkStack {
		return fmt.Errorf(
			"container stack %q does not match network stack %q",
			info.Stack,
			info.NetworkStack,
		)
	}
	if network.UsesClusterCredential() {
		return CheckTagScope(info.NetworkStack, tags, network.StrictTagScope)
	}
	return nil
}

// wipeStateOnKeyChange wipes the state if the network's auth key changed, for
// a fresh registration. A replaced cluster credential is a rotation, which a
// logged-in node survives, so its state is kept.
// syncEphemeralMarker marks stateDir as holding an ephemeral node, or clears
// the mark, as the network using it says. A hostname's directory may move from
// an ephemeral network to a persistent one; a stale mark would have garbage
// collection log the persistent node out and delete its state.
func syncEphemeralMarker(stateDir string, ephemeral bool) {
	if ephemeral {
		if err := tailscale.MarkEphemeral(stateDir); err != nil {
			logger.Warnf("Failed to mark %s as ephemeral: %v", stateDir, err)
		}
		return
	}
	if err := tailscale.ClearEphemeral(stateDir); err != nil {
		logger.Warnf("Failed to clear the ephemeral mark of %s: %v", stateDir, err)
	}
}

func wipeStateOnKeyChange(stateDir string, network *Network) {
	if network.UsesClusterCredential() || !tailscale.StateExists(stateDir) ||
		tailscale.CheckAuthKeyMatch(stateDir, network.AuthKey) {
		return
	}
	if err := tailscale.WipeState(stateDir); err != nil {
		logger.Warnf("Failed to wipe state after auth key change: %v", err)
	}
}

// prepareStateDir claims the container's state directory and readies it for
// the network: its ephemeral marker set to match, wiped if the auth key changed.
func (e *Endpoint) prepareStateDir(info *ContainerInfo, network *Network) (string, error) {
	stateDir, err := stateDirFor(e.DataDir, info.Stack, info.Hostname)
	if err != nil {
		return "", permanentError{err}
	}
	// Claim the directory now, so garbage collection leaves it alone. If
	// another endpoint holds it, retry: it may be leaving, as in a start-first
	// update of a service with a fixed hostname.
	if err := e.ClaimStateDir(info); err != nil {
		return "", err
	}
	syncEphemeralMarker(stateDir, network.Ephemeral())

	wipeStateOnKeyChange(stateDir, network)
	return stateDir, nil
}

// tagsFor returns the tags for the container's node. Tags set on the network
// win, so a container cannot choose its own. The caller holds e.mu.
func (e *Endpoint) tagsFor(info *ContainerInfo) []string {
	if len(e.Network.Tags) == 0 {
		return info.Tags
	}
	if len(info.Tags) > 0 && !slices.Equal(info.Tags, e.Network.Tags) {
		logger.Warnf("Endpoint %s: ignoring tslink.tags label %v, network sets %v",
			e.ID[:12], info.Tags, e.Network.Tags)
	}
	return e.Network.Tags
}

// logoutAttempts bounds logoutWithRetry; startRetryInitial spaces the attempts.
const logoutAttempts = 3

// logoutWithRetry retries a logout that failed: detaching the container's
// other networks is a link change, and tailscaled drops its control
// connection, and with it an in-flight logout, in response.
func logoutWithRetry(logout func() error) error {
	var err error
	for attempt := 1; attempt <= logoutAttempts; attempt++ {
		if err = logout(); err == nil {
			return nil
		}
		if attempt < logoutAttempts {
			time.Sleep(min(startRetryInitial, time.Second))
		}
	}
	return err
}

// cancelRun stops a RunTailscale retry loop, and lets a later Join start a new one.
func (e *Endpoint) cancelRun() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopRun != nil {
		e.stopRun()
	}
	e.runCtx, e.stopRun = nil, nil
}

// releaseStateDir gives up the endpoint's claim on its state directory.
func (e *Endpoint) releaseStateDir() {
	e.claims.release(e.GetStateDir(), e.ID)
}

// unstartedLogoutTimeout bounds logging out the node of a start that failed.
const unstartedLogoutTimeout = 30 * time.Second

// removeUnstartedState removes the state of an ephemeral node whose Tailscale
// never started. The start may have created the state directory it claimed,
// and registered the node before it failed: that node is logged out first.
// A directory another endpoint claimed meanwhile is left alone.
func (e *Endpoint) removeUnstartedState(stateDir string) {
	if e.claims != nil {
		if _, ok := e.claims.claim(stateDir, e.ID); !ok {
			return
		}
	}
	if !tailscale.IsMarkedEphemeral(stateDir) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), unstartedLogoutTimeout)
	defer cancel()
	if err := logoutNode(ctx, e.DataDir, stateDir); err != nil {
		logger.Warnf("Failed to log out ephemeral node of %s, removing its state anyway: %v",
			stateDir, err)
	}
	if err := os.RemoveAll(stateDir); err != nil {
		logger.Warnf("Failed to remove state of ephemeral node %s: %v", stateDir, err)
	}
}

// stopTailscale stops the endpoint's tailscaled, if it runs. A Service backend
// is drained first, unless its stop signal did that, and control given time to
// learn of it. An ephemeral node is then logged out and its state deleted,
// since nothing will reuse it.
func (e *Endpoint) stopTailscale() {
	e.mu.Lock()
	stateDir := e.StateDir
	service := e.Service
	ephemeral := e.Network != nil && e.Network.Ephemeral()
	supervisor := e.supervisor
	inFlight := supervisor == nil && e.running
	e.supervisor = nil
	e.tailscaleStarted = false
	e.mu.Unlock()

	if inFlight {
		// A start in flight may still create the state directory and register
		// the node: RunTailscale stops it once the start returns, and the
		// directory stays claimed until then
		return
	}
	defer e.releaseStateDir()

	if supervisor == nil {
		if ephemeral {
			e.removeUnstartedState(stateDir)
		}
		return
	}
	// A task that exits on its own gets no stop signal, and a node that goes
	// before control learned of its drain can keep callers of the Service
	// pointed at it
	if service != "" {
		supervisor.DrainAndWait(service)
	}
	// An ephemeral node would linger offline until Tailscale removes it;
	// logging out deletes it now
	if ephemeral {
		logger.Infof("Endpoint %s: logging out ephemeral node", e.ID[:12])
		if err := logoutWithRetry(supervisor.Logout); err != nil {
			logger.Warnf("Failed to log out ephemeral node during Leave: %v", err)
		}
	}
	if err := supervisor.Stop(); err != nil {
		logger.Warnf("Failed to stop supervisor during Leave: %v", err)
	}
	// Nothing reuses an ephemeral node's state, and it holds the node key.
	// stateDir is only this endpoint's once its supervisor has started.
	if ephemeral {
		if err := os.RemoveAll(stateDir); err != nil {
			logger.Warnf("Failed to remove state of ephemeral node %s: %v", stateDir, err)
		}
	}
}
