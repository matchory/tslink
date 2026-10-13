// Package tailscale runs and supervises tailscaled for each endpoint.
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
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/moby/moby/client/pkg/stringid"
	"tailscale.com/client/local"

	"github.com/matchory/tslink/internal/ctxtime"
	"github.com/matchory/tslink/pkg/logger"
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

	// HostEndpoints returns the endpoints on the host tailscaled advertises
	// besides its own (netutil.HostEndpoints); nil for none. Read when
	// tailscaled starts, as tailscaled reads them only then, and again by the
	// supervisor every hostEndpointsCheckInterval, to detect a change and
	// restart tailscaled with the new value.
	HostEndpoints func() ([]netip.AddrPort, error)

	// Warn records a warning in the endpoint's status under key, or clears it
	// if message is empty; nil ignores warnings
	Warn func(key, message string)

	gate   *serviceGate // Shared by the daemons of one supervisor; newDaemon creates one if nil
	timing *timing      // Shared like gate; nil for defaultTiming
}

// daemon manages a tailscaled process.
type daemon struct {
	config     DaemonConfig
	cmd        *exec.Cmd
	socketPath string

	hostEndpoints []netip.AddrPort // what tailscaled was started with

	// Context for goroutine lifecycle management
	ctx    context.Context //nolint:containedctx // lifecycle context, cancelled on shutdown
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

	// Closed when watchProcess has waited for tailscaled to exit: only one
	// goroutine may wait for a process
	exited chan struct{}

	// Start the certificate expiry watcher at most once
	certWatch sync.Once
	// Set while the Service waits for its certificate to be configured
	certWaiting atomic.Bool

	// runCLI runs the tailscale CLI and returns its output; tests replace it.
	// Nil runs config.TailscaleBin.
	runCLI func(ctx context.Context, c cliCall) (cliOutput, error)

	// lc talks to tailscaled's LocalAPI on its socket
	lc *local.Client
}

// newDaemon creates a new daemon instance.
func newDaemon(cfg DaemonConfig) (*daemon, error) {
	// Create state directory
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create state dir: %w", err)
	}

	socketPath := cfg.SocketPath
	if socketPath == "" {
		socketPath = filepath.Join(cfg.StateDir, "tailscaled.sock")
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return nil, fmt.Errorf("failed to create socket dir: %w", err)
	}

	// Link <StateDir>/tailscaled.sock to the socket, so tools find a daemon
	// from its state directory. The link is relative, so it resolves wherever
	// the data directory is mounted. Connect through the target: the link's
	// own path may exceed the socket path limit.
	if cfg.SocketPath != "" {
		link := filepath.Join(cfg.StateDir, "tailscaled.sock")
		if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Warnf("Failed to remove stale socket link %s: %v", link, err)
		}
		if target, err := filepath.Rel(cfg.StateDir, socketPath); err != nil {
			logger.Warnf("Failed to link socket into state dir: %v", err)
		} else if err := os.Symlink(target, link); err != nil {
			logger.Warnf("Failed to link socket into state dir: %v", err)
		}
	}

	// Clean stale socket from previous run (prevents "address in use" errors)
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		logger.Warnf("Failed to remove stale socket %s: %v", socketPath, err)
	}

	if cfg.gate == nil {
		cfg.gate = &serviceGate{}
	}
	ctx, cancel := context.WithCancel(context.Background())

	d := &daemon{
		config:     cfg,
		socketPath: socketPath,
		lc:         &local.Client{Socket: socketPath, UseSocketOnly: true},
		ctx:        ctx,
		cancel:     cancel,
	}

	if cfg.HostEndpoints != nil {
		eps, err := cfg.HostEndpoints()
		if err != nil {
			logger.Warnf("Endpoint %s: advertising no host endpoints: %v",
				stringid.TruncateID(cfg.EndpointID), err)
			eps = nil
		}
		d.hostEndpoints = eps
	}

	return d, nil
}

// Start starts the tailscaled process in the target network namespace.
func (d *daemon) Start() error {
	logger.Infof(
		"Starting tailscaled for endpoint %s in netns %s",
		d.config.EndpointID,
		d.config.NetNSPath,
	)

	if err := LinkCertsDir(d.config.StateDir, d.config.CertsDir); err != nil {
		return err
	}
	d.noteSurvivingState()

	if err := d.startProcess(d.tailscaledCommand(d.tailscaledArgs())); err != nil {
		return err
	}

	if err := d.waitForSocket(); err != nil {
		// Write timeout debug info
		d.writeDebug(fmt.Sprintf("SOCKET TIMEOUT\nSocket: %s\nPID: %d\n",
			d.socketPath, d.cmd.Process.Pid))

		if stopErr := d.Stop(); stopErr != nil {
			logger.Warnf("Failed to stop daemon after socket timeout: %v", stopErr)
		}
		return fmt.Errorf("failed waiting for tailscaled socket: %w", err)
	}

	if err := d.setUp(); err != nil {
		if stopErr := d.Stop(); stopErr != nil {
			logger.Warnf("Failed to stop daemon after setup error (%v): %v", err, stopErr)
		}
		return err
	}

	return nil
}

// HostEndpoints returns the endpoints on the host this tailscaled advertises.
func (d *daemon) HostEndpoints() []netip.AddrPort { return d.hostEndpoints }

// IsRunning checks if the tailscaled process is still running.
func (d *daemon) IsRunning() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if !d.running || d.exited == nil {
		return false
	}
	select {
	case <-d.exited:
		return false
	default:
		return true
	}
}

// Stop stops the tailscaled process.
func (d *daemon) Stop() error {
	logger.Infof("Stopping tailscaled for endpoint %s", d.config.EndpointID)

	// Mark as not running and get current state
	d.mu.Lock()
	d.running = false
	cmd := d.cmd
	d.mu.Unlock()

	// Cancel context to signal all goroutines to stop
	if d.cancel != nil {
		d.cancel()
	}

	if cmd != nil && cmd.Process != nil {
		d.killProcess(cmd)
	}

	// Wait for all goroutines to finish (with timeout)
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		logger.Debugf("All daemon goroutines stopped")
	case <-time.After(5 * time.Second):
		logger.Warnf("Timeout waiting for daemon goroutines to stop")
		return errors.New("timeout waiting for daemon goroutines")
	}

	return nil
}

// startProcess starts cmd as tailscaled, drains its output and watches for
// its exit.
func (d *daemon) startProcess(cmd *exec.Cmd) error {
	d.cmd = cmd

	// Graceful stop: tailscaled gets SIGTERM when its context is cancelled,
	// and is killed only if it is still running stopGrace later. unshare, sh
	// and nsenter exec each other, so the signal reaches tailscaled itself
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = d.config.times().stopGrace

	// Both streams go through one pipe to one reader. Wait returns only once
	// exec has copied everything tailscaled wrote into it: a pipe from
	// StdoutPipe is closed by Wait as soon as tailscaled exits, losing what
	// the reader had not read yet, often the lines that tell why it exited
	output, out := io.Pipe()
	d.cmd.Stdout = out
	d.cmd.Stderr = out

	if err := d.cmd.Start(); err != nil {
		closePipe(output, out)
		return fmt.Errorf("failed to start tailscaled: %w", err)
	}

	d.mu.Lock()
	d.running = true
	d.mu.Unlock()

	logger.Infof("tailscaled started with PID %d", d.cmd.Process.Pid)

	d.drainOutput(output)

	d.watchProcess(out)
	return nil
}

// watchProcess records tailscaled's PID, and its exit if it fails, in the
// debug file. Once tailscaled exited and all its output is in the pipe, it
// closes out, so the reader ends after the last line.
func (d *daemon) watchProcess(out *io.PipeWriter) {
	// Write debug info to a file (cancellable via context)
	d.wg.Go(func() {
		select {
		case <-d.ctx.Done():
			return
		case <-time.After(2 * time.Second):
			d.writeDebug(fmt.Sprintf("PID: %d\nSocket: %s\nEndpoint: %s\n",
				d.cmd.Process.Pid, d.socketPath, d.config.EndpointID))
		}
	})

	// Monitor for process exit (cancellable via context)
	exited := make(chan struct{})
	d.mu.Lock()
	d.exited = exited
	d.mu.Unlock()
	d.wg.Go(func() {
		done := make(chan error, 1)
		go func() {
			err := d.cmd.Wait()
			if cerr := out.Close(); cerr != nil {
				logger.Debugf("Failed to close the output pipe: %v", cerr)
			}
			done <- err
			close(exited)
		}()

		select {
		case <-d.ctx.Done():
			return
		case err := <-done:
			if err != nil {
				logger.Errorf("tailscaled exited with error: %v", err)
				d.writeDebug(fmt.Sprintf("EXITED WITH ERROR\nPID: %d\nError: %v\n",
					d.cmd.Process.Pid, err))
			}
		}
	})
}

// writeDebug replaces the debug file in the state directory with info.
func (d *daemon) writeDebug(info string) {
	path := filepath.Join(d.config.StateDir, "debug.log")
	if err := os.WriteFile(path, []byte(info), 0o600); err != nil {
		logger.Warnf("Failed to write debug file: %v", err)
	}
}

// setUp brings the node up and configures its serve endpoints and service.
func (d *daemon) setUp() error {
	if err := d.bringUp(); err != nil {
		return fmt.Errorf("failed to bring up tailscale: %w", err)
	}
	return d.configureServing()
}

// configureServing configures direct serve and the Service backend, as the
// container asks, once the node has logged in.
func (d *daemon) configureServing() error {
	// Configure direct machine serve if enabled (HTTPS on machine hostname)
	logger.Infof(
		"Checking direct serve: Direct=%v Endpoints=%d",
		d.config.Direct,
		len(d.config.Endpoints),
	)
	if d.config.Direct && len(d.config.Endpoints) > 0 {
		logger.Infof("Configuring direct serve...")
		if err := d.configureDirectServe(); err != nil {
			return fmt.Errorf("failed to configure direct serve: %w", err)
		}
		logger.Infof("Direct serve configured successfully")
	}

	// Configure service backend if specified
	// Services is a beta feature - add delay to let control plane fully register the node
	logger.Infof("Checking service: Service=%q", d.config.Service)
	if d.config.Service != "" {
		logger.Infof("Waiting for control plane sync before configuring service backend...")
		if !ctxtime.Sleep(d.ctx, 2*time.Second) {
			return fmt.Errorf("failed to configure Tailscale service: %w", d.ctx.Err())
		}
		logger.Infof("Configuring service backend...")
		if err := d.configureServiceWhenCertified(); err != nil {
			return fmt.Errorf("failed to configure Tailscale service: %w", err)
		}
		logger.Infof("Service backend configured successfully")
	}

	return nil
}

// waitForSocket waits for the tailscaled socket to be ready.
func (d *daemon) waitForSocket() error {
	logger.Debugf("Waiting for tailscaled socket at %s", d.socketPath)

	ctx, cancel := context.WithTimeout(d.ctx, 10*time.Second)
	defer cancel()

	exists := func() bool {
		_, err := os.Stat(d.socketPath)
		return err == nil
	}
	if !ctxtime.Poll(ctx, 100*time.Millisecond, exists) {
		if err := d.ctx.Err(); err != nil {
			return err
		}
		return errors.New("timeout waiting for tailscaled socket")
	}
	logger.Infof("tailscaled socket is ready")
	// Give it a bit more time to fully initialize
	if !ctxtime.Sleep(d.ctx, 500*time.Millisecond) {
		return d.ctx.Err()
	}
	return nil
}

// exitedChan returns the channel closed once tailscaled exits, or nil if it
// was not started.
func (d *daemon) exitedChan() <-chan struct{} {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.exited
}

// killProcess stops tailscaled: SIGTERM, then SIGKILL if it is still running
// stopGrace later. It waits for it to exit and removes its socket. It does not
// take the node down first: that would persist WantRunning=false in a
// persistent node's state.
func (d *daemon) killProcess(cmd *exec.Cmd) {
	if err := killError(cmd.Process.Signal(syscall.SIGTERM)); err != nil {
		logger.Warnf("Failed to stop tailscaled: %v", err)
	}

	// Wait for process with timeout to avoid blocking forever. If
	// watchProcess waits for it, wait for that instead
	grace := d.config.times().stopGrace
	waitDone := d.exitedChan()
	if waitDone == nil {
		ch := make(chan struct{})
		go func() {
			if err := cmd.Wait(); err != nil {
				logger.Debugf("tailscaled process exited: %v", err)
			}
			close(ch)
		}()
		waitDone = ch
	}

	select {
	case <-waitDone:
	case <-time.After(grace):
		logger.Warnf("tailscaled did not exit within %v, killing it", grace)
		if err := killError(cmd.Process.Kill()); err != nil {
			logger.Warnf("Failed to kill tailscaled: %v", err)
		}
		select {
		case <-waitDone:
		case <-time.After(5 * time.Second):
			logger.Warnf("Timeout waiting for tailscaled to exit")
		}
	}

	// The socket outlives a killed tailscaled; sockets no longer sit in
	// the state directory, so nothing else would clean it up
	if err := os.Remove(d.socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		logger.Warnf("Failed to remove socket %s: %v", d.socketPath, err)
	}
}

// killError returns the error of killing tailscaled, or nil if it had exited
// already, as it does by itself after "tailscale down" or a logout.
func killError(err error) error {
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
