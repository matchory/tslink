package tailscale

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aaomidi/tslink/pkg/logger"
)

// Limits for tailscaled's output: a line longer than maxLogLine is cut, and
// its log is rotated at tailscaledLogMaxBytes.
const (
	maxLogLine            = 16 << 10
	tailscaledLogMaxBytes = 10 << 20
)

// DaemonConfig holds configuration for a tailscaled instance.
type DaemonConfig struct {
	EndpointID    string
	StateDir      string
	SocketPath    string // tailscaled socket; defaults to <StateDir>/tailscaled.sock
	Hostname      string
	AuthKey       func() (string, error) // Read at each login, so a replaced credential takes effect
	CertsDir      string                 // Shared certificate directory; empty for tailscaled's own
	NetNSPath     string
	TailscaleBin  string          // Path to tailscale CLI binary
	TailscaledBin string          // Path to tailscaled daemon binary
	Tags          []string        // ACL tags for --advertise-tags
	Service       string          // Service name for tailscale serve (e.g., "svc:hello-world")
	Endpoints     []ServeEndpoint // Serve endpoints (L3/L4/L7)
	Direct        bool            // Enable direct machine serve (HTTPS on machine hostname)
	LoginServer   string          // Control server URL for --login-server; empty for Tailscale's
	ContainerDNS  []netip.Addr    // DNS servers the container was given, as with docker run --dns

	gate *serviceGate // Shared by the daemons of one supervisor; NewDaemon creates one if nil
}

// Daemon manages a tailscaled process.
type Daemon struct {
	config     DaemonConfig
	cmd        *exec.Cmd
	socketPath string

	// Context for goroutine lifecycle management
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// Mutex protects cmd and running state
	mu      sync.RWMutex
	running bool

	// When tailscaled last reported that control does not know its node
	// (deleted, or an expired ephemeral node), as Unix seconds
	nodeNotFound atomic.Int64

	// How often control has fetched the node's Service list, and a channel
	// closed at the next fetch
	fetchMu sync.Mutex
	fetches int64
	fetched chan struct{}

	// Pipe references for cleanup (closing unblocks reader goroutines)
	stdoutPipe io.ReadCloser
	stderrPipe io.ReadCloser

	// runCLI runs the tailscale CLI and returns its output; tests replace it.
	// Nil runs config.TailscaleBin.
	runCLI func(ctx context.Context, prefix string, args ...string) (string, error)
}

// NewDaemon creates a new Daemon instance.
func NewDaemon(cfg DaemonConfig) (*Daemon, error) {
	// Create state directory
	if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create state dir: %w", err)
	}

	socketPath := cfg.SocketPath
	if socketPath == "" {
		socketPath = filepath.Join(cfg.StateDir, "tailscaled.sock")
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0700); err != nil {
		return nil, fmt.Errorf("failed to create socket dir: %w", err)
	}

	// Link <StateDir>/tailscaled.sock to the socket, so tools find a daemon
	// from its state directory. The link is relative, so it resolves wherever
	// the data directory is mounted. Connect through the target: the link's
	// own path may exceed the socket path limit.
	if cfg.SocketPath != "" {
		link := filepath.Join(cfg.StateDir, "tailscaled.sock")
		if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Warn("Failed to remove stale socket link %s: %v", link, err)
		}
		if target, err := filepath.Rel(cfg.StateDir, socketPath); err != nil {
			logger.Warn("Failed to link socket into state dir: %v", err)
		} else if err := os.Symlink(target, link); err != nil {
			logger.Warn("Failed to link socket into state dir: %v", err)
		}
	}

	// Clean stale socket from previous run (prevents "address in use" errors)
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		logger.Warn("Failed to remove stale socket %s: %v", socketPath, err)
	}

	if cfg.gate == nil {
		cfg.gate = &serviceGate{}
	}
	ctx, cancel := context.WithCancel(context.Background())

	return &Daemon{
		config:     cfg,
		socketPath: socketPath,
		ctx:        ctx,
		cancel:     cancel,
	}, nil
}

// Start starts the tailscaled process in the target network namespace.
func (d *Daemon) Start() error {
	logger.Info("Starting tailscaled for endpoint %s in netns %s", d.config.EndpointID, d.config.NetNSPath)

	statePath := filepath.Join(d.config.StateDir, "tailscaled.state")
	if err := LinkCertsDir(d.config.StateDir, d.config.CertsDir); err != nil {
		return err
	}

	// Build tailscaled arguments
	// Use a real tun device (tailscale0) so containers can use Tailscale networking directly
	tailscaledArgs := []string{
		"--state=" + statePath,
		"--socket=" + d.socketPath,
		"--tun=tailscale0",
		"--statedir=" + d.config.StateDir,
	}

	d.cmd = d.tailscaledCommand(tailscaledArgs)
	d.cmd.Env = os.Environ()

	// Set up streaming output - logs each line as it arrives
	stdoutPipe, err := d.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdout pipe: %w", err)
	}
	stderrPipe, err := d.cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to get stderr pipe: %w", err)
	}

	// Store pipe references for cleanup in Stop()
	d.mu.Lock()
	d.stdoutPipe = stdoutPipe
	d.stderrPipe = stderrPipe
	d.mu.Unlock()

	if err := d.cmd.Start(); err != nil {
		return fmt.Errorf("failed to start tailscaled: %w", err)
	}

	d.mu.Lock()
	d.running = true
	d.mu.Unlock()

	logger.Info("tailscaled started with PID %d", d.cmd.Process.Pid)

	debugFile := filepath.Join(d.config.StateDir, "debug.log")

	// tailscaled's output goes to its own size-capped log: it is verbose, and
	// the plugin log is shared by every endpoint on the node
	logPath := filepath.Join(d.config.StateDir, "tailscaled.log")
	logFile, err := logger.OpenRotating(logPath, tailscaledLogMaxBytes)
	if err != nil {
		logger.Warn("Failed to open %s, discarding tailscaled output: %v", logPath, err)
	}
	writeLine := func(line string) {
		d.handleLine(line)
		if logFile != nil {
			if _, err := logFile.Write([]byte(line + "\n")); err != nil {
				logger.Debug("Failed to write %s: %v", logPath, err)
			}
		}
	}

	// Drain both pipes until tailscaled exits or Stop() closes them
	var drains sync.WaitGroup
	drains.Go(func() { drainLines(stdoutPipe, maxLogLine, writeLine) })
	drains.Go(func() { drainLines(stderrPipe, maxLogLine, writeLine) })
	d.wg.Go(func() {
		drains.Wait()
		if logFile != nil {
			if err := logFile.Close(); err != nil {
				logger.Debug("Failed to close %s: %v", logPath, err)
			}
		}
	})

	// Write debug info to a file (cancellable via context)
	d.wg.Go(func() {
		select {
		case <-d.ctx.Done():
			return
		case <-time.After(2 * time.Second):
			debugInfo := fmt.Sprintf("PID: %d\nSocket: %s\nEndpoint: %s\n",
				d.cmd.Process.Pid, d.socketPath, d.config.EndpointID)
			if err := os.WriteFile(debugFile, []byte(debugInfo), 0644); err != nil {
				logger.Warn("Failed to write debug file: %v", err)
			}
		}
	})

	// Monitor for process exit (cancellable via context)
	d.wg.Go(func() {
		done := make(chan error, 1)
		go func() {
			done <- d.cmd.Wait()
		}()

		select {
		case <-d.ctx.Done():
			return
		case err := <-done:
			if err != nil {
				logger.Error("tailscaled exited with error: %v", err)
				debugInfo := fmt.Sprintf("EXITED WITH ERROR\nPID: %d\nError: %v\n",
					d.cmd.Process.Pid, err)
				if writeErr := os.WriteFile(debugFile, []byte(debugInfo), 0644); writeErr != nil {
					logger.Warn("Failed to write debug file: %v", writeErr)
				}
			}
		}
	})

	if err := d.waitForSocket(); err != nil {
		// Write timeout debug info
		debugInfo := fmt.Sprintf("SOCKET TIMEOUT\nSocket: %s\nPID: %d\n",
			d.socketPath, d.cmd.Process.Pid)
		if writeErr := os.WriteFile(debugFile, []byte(debugInfo), 0644); writeErr != nil {
			logger.Warn("Failed to write debug file: %v", writeErr)
		}

		if stopErr := d.Stop(); stopErr != nil {
			logger.Warn("Failed to stop daemon after socket timeout: %v", stopErr)
		}
		return fmt.Errorf("failed waiting for tailscaled socket: %w", err)
	}

	if err := d.bringUp(); err != nil {
		if stopErr := d.Stop(); stopErr != nil {
			logger.Warn("Failed to stop daemon after bringUp error: %v", stopErr)
		}
		return fmt.Errorf("failed to bring up tailscale: %w", err)
	}

	// Configure direct machine serve if enabled (HTTPS on machine hostname)
	logger.Info("Checking direct serve: Direct=%v Endpoints=%d", d.config.Direct, len(d.config.Endpoints))
	if d.config.Direct && len(d.config.Endpoints) > 0 {
		logger.Info("Configuring direct serve...")
		if err := d.configureDirectServe(); err != nil {
			if stopErr := d.Stop(); stopErr != nil {
				logger.Warn("Failed to stop daemon after direct serve error: %v", stopErr)
			}
			return fmt.Errorf("failed to configure direct serve: %w", err)
		}
		logger.Info("Direct serve configured successfully")
	}

	// Configure service backend if specified
	// Services is a beta feature - add delay to let control plane fully register the node
	logger.Info("Checking service: Service=%q", d.config.Service)
	if d.config.Service != "" {
		logger.Info("Waiting for control plane sync before configuring service backend...")
		time.Sleep(2 * time.Second)
		logger.Info("Configuring service backend...")
		if err := d.configureServiceWhenCertified(); err != nil {
			if stopErr := d.Stop(); stopErr != nil {
				logger.Warn("Failed to stop daemon after service config error: %v", stopErr)
			}
			return fmt.Errorf("failed to configure Tailscale service: %w", err)
		}
		logger.Info("Service backend configured successfully")
	}

	return nil
}

// handleLine notes what tslink acts on in a line of tailscaled's output.
func (d *Daemon) handleLine(line string) {
	switch {
	case strings.Contains(line, nodeNotFoundLog):
		d.nodeNotFound.Store(time.Now().Unix())
	case strings.Contains(line, servicesFetchLog):
		d.fetchMu.Lock()
		d.fetches++
		if d.fetched != nil {
			close(d.fetched)
			d.fetched = nil
		}
		d.fetchMu.Unlock()
	}
}

// servicesFetches returns how often control has fetched the node's Service
// list, and a channel closed at the next fetch.
func (d *Daemon) servicesFetches() (int64, <-chan struct{}) {
	d.fetchMu.Lock()
	defer d.fetchMu.Unlock()
	if d.fetched == nil {
		d.fetched = make(chan struct{})
	}
	return d.fetches, d.fetched
}

// tailscaledCommand returns the command running tailscaled with args in the
// container's network namespace.
//
// tailscaled reads /etc/resolv.conf for its own lookups, of the ACME server for
// one, and as the upstreams of its DNS forwarder. The plugin's is the host's,
// whose resolver may not exist in that namespace, such as systemd-resolved's
// 127.0.0.53; and Docker's embedded resolver loops back to tailscaled when the
// container uses Tailscale's resolver. So tailscaled gets a resolv.conf of its
// own (see tailscaledResolvConf), in a mount namespace of its own. It gets its
// own backup of it too: tailscaled replaces /etc/resolv.conf and then reads its
// upstreams from the backup, which the endpoints would otherwise share. Without
// the mounts, tailscaled keeps the plugin's files.
func (d *Daemon) tailscaledCommand(args []string) *exec.Cmd {
	host, err := os.ReadFile(hostResolvConfPath)
	if err != nil {
		logger.Warn("Failed to read %s: %v", hostResolvConfPath, err)
	}
	resolved, err := os.ReadFile(resolvedResolvConfPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		logger.Warn("Failed to read %s: %v", resolvedResolvConfPath, err)
	}
	conf, warning := tailscaledResolvConf(host, resolved, d.config.ContainerDNS)
	if warning != "" {
		logger.Warn("Endpoint %s: %s", d.config.EndpointID[:min(12, len(d.config.EndpointID))], warning)
	}
	resolvConf := filepath.Join(d.config.StateDir, "resolv.conf")
	if err := os.WriteFile(resolvConf, conf, 0644); err != nil { // #nosec G306 -- not secret
		logger.Warn("Failed to write %s: %v", resolvConf, err)
	}
	backup := filepath.Join(d.config.StateDir, "resolv.pre-tailscale-backup.conf")
	if err := os.WriteFile(backup, nil, 0644); err != nil { // #nosec G306 -- not secret
		logger.Warn("Failed to write %s: %v", backup, err)
	}
	// unshare, sh and nsenter each exec the next, so the process is tailscaled
	unshareArgs := []string{
		"--mount", "--propagation", "private", "--",
		"sh", "-c", `mount --bind "$0" /etc/resolv.conf || echo "tslink: tailscaled keeps the plugin's resolv.conf" >&2
b=/etc/resolv.pre-tailscale-backup.conf
{ [ -e $b ] || touch $b; } && mount --bind "$1" $b || echo "tslink: tailscaled shares the plugin's resolv.conf backup" >&2
netns=$2; shift 2; exec nsenter --net="$netns" -- "$@"`,
		resolvConf, backup, d.config.NetNSPath, d.config.TailscaledBin,
	}
	unshareArgs = append(unshareArgs, args...)
	logger.Debug("Running: unshare %v", unshareArgs)
	return exec.CommandContext(d.ctx, "unshare", unshareArgs...)
}

// waitForSocket waits for the tailscaled socket to be ready.
func (d *Daemon) waitForSocket() error {
	logger.Debug("Waiting for tailscaled socket at %s", d.socketPath)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for tailscaled socket")
		default:
			if _, err := os.Stat(d.socketPath); err == nil {
				logger.Info("tailscaled socket is ready")
				// Give it a bit more time to fully initialize
				time.Sleep(500 * time.Millisecond)
				return nil
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// IsRunning checks if the tailscaled process is still running.
func (d *Daemon) IsRunning() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if !d.running || d.cmd == nil || d.cmd.Process == nil {
		return false
	}
	// Signal 0 checks if process exists without killing it
	err := d.cmd.Process.Signal(syscall.Signal(0))
	return err == nil
}

// Stop stops the tailscaled process.
func (d *Daemon) Stop() error {
	logger.Info("Stopping tailscaled for endpoint %s", d.config.EndpointID)

	// Mark as not running and get current state
	d.mu.Lock()
	d.running = false
	cmd := d.cmd
	stdoutPipe := d.stdoutPipe
	stderrPipe := d.stderrPipe
	d.stdoutPipe = nil
	d.stderrPipe = nil
	d.mu.Unlock()

	// Cancel context to signal all goroutines to stop
	if d.cancel != nil {
		d.cancel()
	}

	if cmd != nil && cmd.Process != nil {
		// Try graceful shutdown first
		args := []string{
			"--socket=" + d.socketPath,
			"down",
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		downCmd := exec.CommandContext(ctx, d.config.TailscaleBin, args...)
		_ = downCmd.Run() // Ignore errors

		// Kill the process
		if err := cmd.Process.Kill(); err != nil {
			logger.Warn("Failed to kill tailscaled: %v", err)
		}

		// Wait for process with timeout to avoid blocking forever
		waitDone := make(chan struct{})
		go func() {
			if err := cmd.Wait(); err != nil {
				logger.Debug("tailscaled process exited: %v", err)
			}
			close(waitDone)
		}()

		select {
		case <-waitDone:
			// Process exited cleanly
		case <-time.After(5 * time.Second):
			logger.Warn("Timeout waiting for tailscaled to exit")
		}

		// The socket outlives a killed tailscaled; sockets no longer sit in
		// the state directory, so nothing else would clean it up
		if err := os.Remove(d.socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Warn("Failed to remove socket %s: %v", d.socketPath, err)
		}
	}

	// Close pipes to unblock reader goroutines
	if stdoutPipe != nil {
		if err := stdoutPipe.Close(); err != nil {
			logger.Debug("Failed to close stdout pipe: %v", err)
		}
	}
	if stderrPipe != nil {
		if err := stderrPipe.Close(); err != nil {
			logger.Debug("Failed to close stderr pipe: %v", err)
		}
	}

	// Wait for all goroutines to finish (with timeout)
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		logger.Debug("All daemon goroutines stopped")
	case <-time.After(5 * time.Second):
		logger.Warn("Timeout waiting for daemon goroutines to stop")
		return fmt.Errorf("timeout waiting for daemon goroutines")
	}

	return nil
}
