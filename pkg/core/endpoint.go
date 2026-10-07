package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/docker/go-plugins-helpers/network"

	"github.com/aaomidi/tslink/pkg/logger"
	"github.com/aaomidi/tslink/pkg/netutil"
	"github.com/aaomidi/tslink/pkg/tailscale"
)

// ServeEndpoint represents a single Tailscale serve configuration.
// Configured via labels like: tslink.serve.443=https:8080/api.
type ServeEndpoint struct {
	Proto  string // http, https, tcp, tls-terminated-tcp, tun
	Port   string // External port Tailscale exposes
	Target string // Container port or address to forward to
	Path   string // L7 only - path prefix (e.g., "/api")

	ProxyProtocol string // L4 only - PROXY protocol version sent to the target ("1", "2" or "")
}

// Endpoint represents a container endpoint with Tailscale connectivity.
type Endpoint struct {
	mu sync.RWMutex // Protects concurrent access to mutable fields

	ID          string
	Network     *Network
	Hostname    string
	Tags        []string        // ACL tags for tailscale up --advertise-tags
	Service     string          // Service name (e.g., "svc:hello-world")
	Endpoints   []ServeEndpoint // Serve endpoints (replaces ServePort)
	Direct      bool            // Enable direct machine serve (default: true)
	TailscaleIP string
	VethName    string
	StateDir    string
	TSVersion   string // Tailscale version to use
	TSPath      string // Optional custom Tailscale binary path
	SandboxKey  string // Container's network namespace path, stored during Join
	DataDir     string // Base data directory for state

	supervisor       *tailscale.DaemonSupervisor
	tailscaleStarted bool // Whether Tailscale setup has been completed

	running bool                       // Whether RunTailscale is active
	runCtx  context.Context            // Cancelled when the endpoint leaves
	stopRun context.CancelFunc         // Cancels runCtx
	startFn func(*ContainerInfo) error // Replaces StartTailscale in tests
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
	Name      string            // Container name (without leading /)
	Labels    map[string]string // All container labels
	Hostname  string            // Parsed tslink.hostname label or container name
	Tags      []string          // Parsed tslink.tags label (comma-separated)
	Service   string            // Parsed tslink.service label (e.g., "svc:hello-world")
	Endpoints []ServeEndpoint   // Parsed tslink.serve.<port> labels
	Direct    bool              // Enable direct machine serve (default: true, set tslink.direct=false to disable)

	Stack        string // Container's com.docker.stack.namespace label
	NetworkStack string // Network's com.docker.stack.namespace label
}

// StackLabel is the label docker stack deploy puts on a stack's services,
// containers and networks. It overrides any value from the compose file.
const StackLabel = "com.docker.stack.namespace"

// NewEndpoint creates a new endpoint with the given configuration.
func NewEndpoint(id string, net *Network, opts EndpointOptions, cfg *Config) (*Endpoint, error) {
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
		TSVersion: cfg.TSVersion,
		TSPath:    cfg.TSPath,
	}, nil
}

// generateVethIPs generates unique IP addresses for a veth pair based on endpoint ID.
// Uses 10.200.0.0/16 range, with each endpoint getting a /30 subnet.
// Returns (hostIP, containerIP).
func generateVethIPs(endpointID string) (string, string) {
	// Use first 4 bytes of endpoint ID to generate a unique subnet
	// Each /30 has 4 IPs: network, host, container, broadcast
	// So we can have 65536/4 = 16384 unique subnets
	var hash uint16
	for i := 0; i < len(endpointID) && i < 8; i++ {
		hash = hash*31 + uint16(endpointID[i])
	}

	// Ensure we don't use .0 or .255 subnets
	subnetNum := (hash % 16380) + 1 // 1 to 16380
	baseIP := subnetNum * 4         // Each /30 uses 4 IPs

	// 10.200.x.y where x.y comes from baseIP
	thirdOctet := baseIP / 256
	fourthOctet := baseIP % 256

	hostIP := fmt.Sprintf("10.200.%d.%d", thirdOctet, fourthOctet+1)
	containerIP := fmt.Sprintf("10.200.%d.%d", thirdOctet, fourthOctet+2)

	return hostIP, containerIP
}

// Join is called when a container joins the network.
// It sets up basic networking (veth, IPs, routing, NAT) and stores the sandbox key.
// Tailscale setup is deferred until container info is available via Docker events.
// This allows Join() to return quickly while Tailscale is configured asynchronously.
func (e *Endpoint) Join(sandboxKey string) (*network.JoinResponse, error) {
	// NOTE: We intentionally do NOT hold the lock during network operations.
	// Network syscalls can block for seconds, which would block all other endpoint operations.
	// We only lock briefly to read/write state.

	logger.Info("Endpoint %s joining with sandbox %s", e.ID, sandboxKey)

	// Generate unique IPs for this endpoint's veth pair (uses only e.ID which is immutable)
	hostVethIP, containerVethIP := generateVethIPs(e.ID)
	logger.Info("Using veth IPs: host=%s container=%s", hostVethIP, containerVethIP)

	// Create veth pair (blocking network operation - no lock held)
	vethHost, vethContainer, err := netutil.CreateVethPair(e.ID[:8], e.Network.MTU)
	if err != nil {
		return nil, fmt.Errorf("failed to create veth pair: %w", err)
	}

	logger.Info("Created veth pair: host=%s container=%s", vethHost, vethContainer)

	// Move container end of veth into container's network namespace
	if err := netutil.MoveToNetNS(vethContainer, sandboxKey); err != nil {
		if cleanupErr := netutil.DeleteVeth(vethHost); cleanupErr != nil {
			logger.Warn("failed to cleanup veth %s after error: %v", vethHost, cleanupErr)
		}
		return nil, fmt.Errorf("failed to move veth to container ns: %w", err)
	}

	// Set up host side of veth with IP
	if err := netutil.SetupHostRouting(vethHost, hostVethIP); err != nil {
		if cleanupErr := netutil.DeleteVeth(vethHost); cleanupErr != nil {
			logger.Warn("failed to cleanup veth %s after error: %v", vethHost, cleanupErr)
		}
		return nil, fmt.Errorf("failed to set up host routing: %w", err)
	}

	// Set up container side with IP, bring it up, and add default route
	if err := netutil.SetupInterfaceInNS(sandboxKey, vethContainer, ""); err != nil {
		if cleanupErr := netutil.DeleteVeth(vethHost); cleanupErr != nil {
			logger.Warn("failed to cleanup veth %s after error: %v", vethHost, cleanupErr)
		}
		return nil, fmt.Errorf("failed to set up container interface: %w", err)
	}

	if err := netutil.SetupContainerRouting(sandboxKey, vethContainer, containerVethIP, hostVethIP); err != nil {
		if cleanupErr := netutil.DeleteVeth(vethHost); cleanupErr != nil {
			logger.Warn("failed to cleanup veth %s after error: %v", vethHost, cleanupErr)
		}
		return nil, fmt.Errorf("failed to set up container routing: %w", err)
	}

	// tailscaled's own traffic must not depend on the container's other networks
	if err := netutil.SetupBypassRoute(sandboxKey, vethContainer, hostVethIP); err != nil {
		if cleanupErr := netutil.DeleteVeth(vethHost); cleanupErr != nil {
			logger.Warn("failed to cleanup veth %s after error: %v", vethHost, cleanupErr)
		}
		return nil, fmt.Errorf("failed to route tailscaled via veth: %w", err)
	}

	// Fail closed: tailnet traffic must not fall through to the host's tailscaled
	if err := netutil.SetupTailnetBlackhole(sandboxKey); err != nil {
		if cleanupErr := netutil.DeleteVeth(vethHost); cleanupErr != nil {
			logger.Warn("failed to cleanup veth %s after error: %v", vethHost, cleanupErr)
		}
		return nil, fmt.Errorf("failed to blackhole tailnet ranges: %w", err)
	}

	// Set up NAT/MASQUERADE for internet access
	if err := netutil.SetupNAT(vethHost); err != nil {
		logger.Info("Warning: failed to set up NAT: %v", err)
		// Continue anyway - tailscaled might still work via DERP
	}

	// Now lock briefly to store results
	e.mu.Lock()
	e.SandboxKey = sandboxKey
	e.VethName = vethHost
	e.mu.Unlock()

	logger.Info(
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
		logger.Debug("Tailscale already started for endpoint %s", e.ID[:12])
		return nil
	}
	if e.supervisor != nil {
		e.mu.Unlock()
		logger.Debug("Tailscale startup already in progress for endpoint %s", e.ID[:12])
		return nil
	}
	sandboxKey := e.SandboxKey
	if sandboxKey == "" {
		e.mu.Unlock()
		return fmt.Errorf("no sandbox key set - Join() must be called first")
	}

	// Copy immutable config needed for supervisor creation
	endpointID := e.ID
	dataDir := e.DataDir
	tsVersion := e.TSVersion
	tsPath := e.TSPath
	authKey := e.Network.AuthKey
	tags := info.Tags
	if len(e.Network.Tags) > 0 {
		// Tags set on the network win, so a container cannot choose its own
		if len(info.Tags) > 0 && !slices.Equal(info.Tags, e.Network.Tags) {
			logger.Warn("Endpoint %s: ignoring tslink.tags label %v, network sets %v",
				e.ID[:12], info.Tags, e.Network.Tags)
		}
		tags = e.Network.Tags
	}
	e.mu.Unlock()

	// A stack's network serves only that stack's tasks, so another stack
	// cannot attach to it and take its credentials and tags.
	if info.NetworkStack != "" && info.Stack != info.NetworkStack {
		return permanentError{fmt.Errorf("container stack %q does not match network stack %q", info.Stack, info.NetworkStack)}
	}

	stateDir, err := stateDirFor(dataDir, info.Stack, info.Hostname)
	if err != nil {
		return permanentError{err}
	}
	// Claim the directory now, so garbage collection leaves it alone. If
	// another endpoint holds it, retry: it may be leaving, as in a start-first
	// update of a service with a fixed hostname.
	if err := e.ClaimStateDir(info); err != nil {
		return err
	}
	if tailscale.IsEphemeralKey(authKey) {
		if err := tailscale.MarkEphemeral(stateDir); err != nil {
			logger.Warn("Failed to mark %s as ephemeral: %v", stateDir, err)
		}
	}

	// Check if auth key changed - if so, wipe state for fresh registration
	if tailscale.StateExists(stateDir) && !tailscale.CheckAuthKeyMatch(stateDir, authKey) {
		if err := tailscale.WipeState(stateDir); err != nil {
			logger.Warn("Failed to wipe state after auth key change: %v", err)
		}
	}

	logger.Info("StartTailscale: endpoint=%s hostname=%s service=%s endpoints=%d direct=%v",
		endpointID[:12], info.Hostname, info.Service, len(info.Endpoints), info.Direct)

	// Validate service configuration
	if info.Service != "" && len(info.Endpoints) == 0 {
		return permanentError{fmt.Errorf("tslink.service requires at least one tslink.serve.<port> endpoint")}
	}

	// Ensure Tailscale binaries are available (may download - blocking!)
	tailscaleBin, tailscaledBin, err := tailscale.EnsureBinaries(tsVersion, tsPath)
	if err != nil {
		return fmt.Errorf("failed to ensure Tailscale binaries: %w", err)
	}

	// Version check only when Services are used
	if info.Service != "" {
		version, err := tailscale.GetInstalledVersion(tailscaleBin)
		if err != nil {
			logger.Info("Warning: could not determine Tailscale version: %v", err)
		} else if err := tailscale.CheckVersionForServices(version); err != nil {
			return permanentError{err}
		}
	}

	// Convert core.ServeEndpoint to tailscale.ServeEndpoint
	tsEndpoints := make([]tailscale.ServeEndpoint, len(info.Endpoints))
	for i, ep := range info.Endpoints {
		tsEndpoints[i] = tailscale.ServeEndpoint{
			Proto:  ep.Proto,
			Port:   ep.Port,
			Target: ep.Target,
			Path:   ep.Path,

			ProxyProtocol: ep.ProxyProtocol,
		}
	}

	// Create supervisor (handles daemon lifecycle with auto-recovery)
	supervisor := tailscale.NewDaemonSupervisor(tailscale.DaemonConfig{
		EndpointID:    endpointID,
		StateDir:      stateDir,
		SocketPath:    socketPathFor(dataDir, endpointID),
		Hostname:      info.Hostname,
		AuthKey:       authKey,
		NetNSPath:     sandboxKey,
		TailscaleBin:  tailscaleBin,
		TailscaledBin: tailscaledBin,
		Tags:          tags,
		Service:       info.Service,
		Endpoints:     tsEndpoints,
		Direct:        info.Direct,
	})

	// Start supervisor (blocks until initial daemon startup succeeds or fails - up to 90s!)
	if err := supervisor.Start(); err != nil {
		// The supervisor keeps retrying after a failed first start. Nothing
		// would stop it later, since it is not stored on the endpoint.
		if stopErr := supervisor.Stop(); stopErr != nil {
			logger.Warn("failed to stop supervisor after start error: %v", stopErr)
		}
		return fmt.Errorf("failed to start tailscale supervisor: %w", err)
	}

	// Wait for Tailscale to connect and get IP (can take up to 60s!)
	status, err := supervisor.WaitForIP()
	if err != nil {
		if stopErr := supervisor.Stop(); stopErr != nil {
			logger.Warn("failed to stop supervisor after WaitForIP error: %v", stopErr)
		}
		return fmt.Errorf("failed to get Tailscale IP: %w", err)
	}

	// Save auth key hash for future comparisons
	if err := tailscale.SaveAuthKeyHash(stateDir, authKey); err != nil {
		logger.Warn("Failed to save auth key hash: %v", err)
	}

	// Now lock briefly to store results
	e.mu.Lock()
	// Double-check we didn't race with another caller
	if e.tailscaleStarted {
		e.mu.Unlock()
		if stopErr := supervisor.Stop(); stopErr != nil {
			logger.Warn("failed to stop duplicate supervisor: %v", stopErr)
		}
		logger.Warn("Tailscale was started by another goroutine for endpoint %s", endpointID[:12])
		return nil
	}
	e.supervisor = supervisor
	e.TailscaleIP = status.IP
	e.Hostname = info.Hostname
	e.Tags = tags
	e.Service = info.Service
	e.Endpoints = info.Endpoints
	e.Direct = info.Direct
	e.StateDir = stateDir
	e.tailscaleStarted = true
	e.mu.Unlock()

	logger.Info("Endpoint %s got Tailscale IP: %s (hostname=%s)", endpointID[:12], status.IP, info.Hostname)
	return nil
}

// RunTailscale starts Tailscale for the endpoint, retrying with backoff until
// it succeeds, fails in a way retrying cannot fix, or the endpoint leaves. A
// transient failure, such as the control plane being unreachable when the task
// starts, must not leave the task without its identity for good. Only one run
// is active per endpoint, however often it is triggered.
func (e *Endpoint) RunTailscale(info *ContainerInfo) {
	e.mu.Lock()
	if e.running || e.tailscaleStarted {
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
	}()

	delay := startRetryInitial
	for attempt := 1; ; attempt++ {
		err := start(info)
		if err == nil {
			e.writeStatus(info, StatusRunning, attempt-1, nil)
			if ctx.Err() != nil {
				// Left while starting: Leave found no supervisor to stop
				logger.Info("Endpoint %s left while Tailscale started, stopping it", e.ID[:12])
				e.stopTailscale()
			}
			return
		}
		if ctx.Err() != nil {
			return
		}
		if _, ok := errors.AsType[permanentError](err); ok {
			logger.Error("Endpoint %s: cannot start Tailscale: %v", e.ID[:12], err)
			e.writeStatus(info, StatusFailed, attempt, err)
			return
		}
		e.writeStatus(info, StatusRetrying, attempt, err)
		logger.Error("Endpoint %s: starting Tailscale failed (attempt %d), retrying in %v: %v",
			e.ID[:12], attempt, delay, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, startRetryMax)
	}
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

// stateClaims maps each state directory in use to the endpoint using it.
var (
	stateClaimsMu sync.Mutex
	stateClaims   = map[string]string{}
)

// ClaimStateDir records the state directory the container's tailscaled
// uses, so garbage collection leaves it alone. A directory serves one
// endpoint at a time: two replicas with the same tslink.hostname on a node
// would otherwise share one node key. An invalid name claims nothing;
// starting fails on it.
func (e *Endpoint) ClaimStateDir(info *ContainerInfo) error {
	dir, err := stateDirFor(e.DataDir, info.Stack, info.Hostname)
	if err != nil {
		return nil
	}

	stateClaimsMu.Lock()
	defer stateClaimsMu.Unlock()
	if owner, ok := stateClaims[dir]; ok && owner != e.ID {
		return fmt.Errorf("state directory %s is in use by endpoint %s: is tslink.hostname %q set on a replicated service?",
			dir, owner[:12], info.Hostname)
	}
	stateClaims[dir] = e.ID

	e.mu.Lock()
	e.StateDir = dir
	e.mu.Unlock()
	return nil
}

// releaseStateDir gives up the endpoint's claim on its state directory.
func (e *Endpoint) releaseStateDir() {
	dir := e.GetStateDir()
	stateClaimsMu.Lock()
	defer stateClaimsMu.Unlock()
	if stateClaims[dir] == e.ID {
		delete(stateClaims, dir)
	}
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
	hostVethIP, _ := generateVethIPs(e.ID)

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
func (e *Endpoint) GetInfo() (tailscaleIP, hostname string) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.TailscaleIP, e.Hostname
}

// Leave is called when a container leaves the network.
// It stops the Tailscale supervisor and cleans up networking resources.
func (e *Endpoint) Leave() error {
	e.cancelRun()

	e.mu.Lock()
	vethName := e.VethName
	sandboxKey := e.SandboxKey
	e.mu.Unlock()

	logger.Info("Endpoint %s leaving", e.ID)

	// Stop the Tailscale supervisor first
	e.stopTailscale()
	e.removeStatus()

	// Clean up NAT rules for this veth
	if vethName != "" {
		if err := netutil.CleanupNAT(vethName); err != nil {
			logger.Info("Warning: failed to cleanup NAT for %s: %v", vethName, err)
		}
	}

	if sandboxKey != "" {
		if err := netutil.CleanupTailnetBlackhole(sandboxKey); err != nil {
			logger.Info("Warning: failed to remove tailnet blackhole routes: %v", err)
		}
	}

	// Clean up veth (this also removes the container-side interface)
	if vethName != "" {
		if err := netutil.DeleteVeth(vethName); err != nil {
			logger.Info("Warning: failed to delete veth %s: %v", vethName, err)
		}
	}

	return nil
}

// stopTailscale stops the endpoint's tailscaled, if it runs. An ephemeral node
// is logged out and its state deleted, since nothing will reuse it.
func (e *Endpoint) stopTailscale() {
	defer e.releaseStateDir()

	e.mu.Lock()
	stateDir := e.StateDir
	ephemeral := e.Network != nil && tailscale.IsEphemeralKey(e.Network.AuthKey)
	supervisor := e.supervisor
	e.supervisor = nil
	e.tailscaleStarted = false
	e.mu.Unlock()

	if supervisor == nil {
		// Tailscale never started, but the start may have created the state
		// directory it claimed
		if ephemeral && tailscale.IsMarkedEphemeral(stateDir) {
			if err := os.RemoveAll(stateDir); err != nil {
				logger.Warn("Failed to remove state of ephemeral node %s: %v", stateDir, err)
			}
		}
		return
	}
	// An ephemeral node would linger offline until Tailscale removes it;
	// logging out deletes it now
	if ephemeral {
		if err := logoutWithRetry(supervisor.Logout); err != nil {
			logger.Warn("Failed to log out ephemeral node during Leave: %v", err)
		}
	}
	if err := supervisor.Stop(); err != nil {
		logger.Warn("Failed to stop supervisor during Leave: %v", err)
	}
	// Nothing reuses an ephemeral node's state, and it holds the node key.
	// stateDir is only this endpoint's once its supervisor has started.
	if ephemeral {
		if err := os.RemoveAll(stateDir); err != nil {
			logger.Warn("Failed to remove state of ephemeral node %s: %v", stateDir, err)
		}
	}
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
	logger.Info("Endpoint %s stopping, draining %s", e.ID[:12], service)
	if err := supervisor.Drain(service); err != nil {
		logger.Warn("Failed to drain %s for endpoint %s: %v", service, e.ID[:12], err)
	}
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

// Stop stops the endpoint and cleans up resources.
func (e *Endpoint) Stop() error {
	e.cancelRun()
	defer e.releaseStateDir()

	e.mu.Lock()
	supervisor := e.supervisor
	e.supervisor = nil
	e.tailscaleStarted = false
	e.mu.Unlock()

	logger.Info("Stopping endpoint %s", e.ID)

	if supervisor != nil {
		if err := supervisor.Stop(); err != nil {
			logger.Info("Warning: failed to stop supervisor: %v", err)
		}
	}

	return nil
}

// ApplyHostnameChange changes the hostname of a running Tailscale instance.
func (e *Endpoint) ApplyHostnameChange(newHostname string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.supervisor == nil {
		return fmt.Errorf("no supervisor running")
	}

	logger.Info("ApplyHostnameChange: changing hostname from %s to %s", e.Hostname, newHostname)

	if err := e.supervisor.SetHostname(newHostname); err != nil {
		return fmt.Errorf("failed to set hostname: %w", err)
	}

	e.Hostname = newHostname
	return nil
}

// ApplyServiceConfig configures Tailscale service endpoints after initial startup.
// This is called when container info is obtained from cache after Join().
func (e *Endpoint) ApplyServiceConfig(info *ContainerInfo) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.supervisor == nil {
		return fmt.Errorf("no supervisor running")
	}

	if info.Service == "" {
		return nil // No service to configure
	}

	logger.Info("ApplyServiceConfig: configuring service %s with %d endpoints", info.Service, len(info.Endpoints))

	// Convert core.ServeEndpoint to tailscale.ServeEndpoint
	tsEndpoints := make([]tailscale.ServeEndpoint, len(info.Endpoints))
	for i, ep := range info.Endpoints {
		tsEndpoints[i] = tailscale.ServeEndpoint{
			Proto:  ep.Proto,
			Port:   ep.Port,
			Target: ep.Target,
			Path:   ep.Path,

			ProxyProtocol: ep.ProxyProtocol,
		}
	}

	if err := e.supervisor.ConfigureServeEndpoints(info.Service, tsEndpoints, info.Tags, info.Direct); err != nil {
		return fmt.Errorf("failed to configure service: %w", err)
	}

	e.Service = info.Service
	e.Tags = info.Tags
	e.Endpoints = info.Endpoints
	e.Direct = info.Direct
	return nil
}
