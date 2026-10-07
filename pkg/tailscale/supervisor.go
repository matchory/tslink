package tailscale

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"sync"
	"time"

	"github.com/aaomidi/tslink/pkg/logger"
)

// SupervisorStatus represents the current state of the supervisor.
type SupervisorStatus string

// Supervisor states.
const (
	StatusStopped   SupervisorStatus = "stopped"
	StatusStarting  SupervisorStatus = "starting"
	StatusRunning   SupervisorStatus = "running"
	StatusFailed    SupervisorStatus = "failed"
	StatusCrashLoop SupervisorStatus = "crash_loop"
)

// Backoff configuration.
const (
	initialBackoff = 100 * time.Millisecond
	maxBackoff     = 30 * time.Second

	// Crash loop detection: 5 crashes in 30 seconds.
	crashLoopWindow    = 30 * time.Second
	crashLoopThreshold = 5

	// Pause after a crash loop before trying again; the container keeps
	// running, so giving up would leave it without its identity for good.
	crashLoopCooldown = 5 * time.Minute

	// Health check interval.
	healthCheckInterval = 5 * time.Second

	// Minimum uptime before backoff is reset (prevents rapid restart loops from resetting backoff).
	minUptimeForBackoffReset = 30 * time.Second

	// Startup jitter to prevent thundering herd on host reboot (0-500ms).
	maxStartupJitter = 500 * time.Millisecond
)

// needsLoginChecks is how many health checks in a row must find the node
// logged out, as when its device was deleted or expired, before it logs in
// again; maxLoginBackoff caps the checks skipped after a failed login.
const (
	needsLoginChecks = 2
	maxLoginBackoff  = 24
)

// loginWatch decides when a logged-out node should log in again.
type loginWatch struct {
	count   int
	backoff int // checks to skip after a failed login
	skip    int
}

// observe records whether the node is logged out and reports whether to log
// in again now.
func (w *loginWatch) observe(loggedOut bool) bool {
	if !loggedOut {
		*w = loginWatch{}
		return false
	}
	if w.skip > 0 {
		w.skip--
		return false
	}
	w.count++
	if w.count < needsLoginChecks {
		return false
	}
	w.count = 0
	return true
}

// failed records a failed login, so the next waits longer.
func (w *loginWatch) failed() {
	w.backoff = min(max(w.backoff*2, needsLoginChecks), maxLoginBackoff)
	w.skip = w.backoff
}

// DaemonSupervisor manages a Daemon with automatic restart on failure.
type DaemonSupervisor struct {
	cfg DaemonConfig

	// Lifecycle management
	ctx    context.Context //nolint:containedctx // lifecycle context, cancelled on shutdown
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// State (protected by mu)
	mu            sync.RWMutex
	daemon        *Daemon
	status        SupervisorStatus
	restartCount  int
	crashTimes    []time.Time
	lastStartTime time.Time // When daemon last started (for uptime-based backoff reset)

	// Backoff state
	backoffAttempt int

	// Channel to signal initial startup complete (protected by Once)
	startupDone     chan error
	startupDoneOnce sync.Once
}

// NewDaemonSupervisor creates a new supervisor for managing a tailscaled daemon.
func NewDaemonSupervisor(cfg DaemonConfig) *DaemonSupervisor {
	ctx, cancel := context.WithCancel(context.Background())
	return &DaemonSupervisor{
		cfg:         cfg,
		ctx:         ctx,
		cancel:      cancel,
		status:      StatusStopped,
		crashTimes:  make([]time.Time, 0),
		startupDone: make(chan error, 1),
	}
}

// Start begins the supervision loop and waits for initial daemon startup.
// Returns error if the initial startup fails.
func (s *DaemonSupervisor) Start() error {
	s.mu.Lock()
	if s.status != StatusStopped {
		s.mu.Unlock()
		return fmt.Errorf("supervisor already running (status=%s)", s.status)
	}
	s.status = StatusStarting
	s.mu.Unlock()

	// Start supervision loop in background
	s.wg.Add(1)
	go s.supervisionLoop()

	// Wait for initial startup result
	select {
	case err := <-s.startupDone:
		if err != nil {
			return fmt.Errorf("initial daemon startup failed: %w", err)
		}
		return nil
	case <-time.After(90 * time.Second):
		s.cancel()
		return errors.New("timeout waiting for initial daemon startup")
	}
}

// Stop gracefully stops the supervisor and daemon.
func (s *DaemonSupervisor) Stop() error {
	logger.Infof("Stopping supervisor for endpoint %s", s.cfg.EndpointID[:8])

	// Signal supervision loop to stop
	s.cancel()

	// Wait for supervision loop to finish (with timeout)
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		logger.Debugf("Supervisor stopped cleanly")
	case <-time.After(10 * time.Second):
		logger.Warnf("Timeout waiting for supervisor to stop")
	}

	s.mu.Lock()
	s.status = StatusStopped
	s.mu.Unlock()

	return nil
}

// Logout logs the running daemon's node out.
func (s *DaemonSupervisor) Logout() error {
	d := s.GetDaemon()
	if d == nil || !d.IsRunning() {
		return errors.New("tailscaled is not running")
	}
	return d.Logout()
}

// Drain stops new connections to the node's backend for a Tailscale Service.
func (s *DaemonSupervisor) Drain(service string) error {
	d := s.GetDaemon()
	if d == nil || !d.IsRunning() {
		return errors.New("tailscaled is not running")
	}
	return d.Drain(service)
}

// Status returns the current supervisor status and restart count.
func (s *DaemonSupervisor) Status() (SupervisorStatus, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status, s.restartCount
}

// GetDaemon returns the current daemon (may be nil if not running).
func (s *DaemonSupervisor) GetDaemon() *Daemon {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.daemon
}

// WaitForIP waits for the daemon to get a Tailscale IP.
// Delegates to the underlying daemon.
func (s *DaemonSupervisor) WaitForIP() (*Status, error) {
	s.mu.RLock()
	daemon := s.daemon
	s.mu.RUnlock()

	if daemon == nil {
		return nil, errors.New("no daemon running")
	}

	return daemon.WaitForIP()
}

// SetHostname updates the Tailscale hostname.
// Delegates to the underlying daemon.
func (s *DaemonSupervisor) SetHostname(hostname string) error {
	s.mu.RLock()
	daemon := s.daemon
	s.mu.RUnlock()

	if daemon == nil {
		return errors.New("no daemon running")
	}

	return daemon.SetHostname(hostname)
}

// ConfigureServeEndpoints configures serve endpoints.
// Delegates to the underlying daemon.
func (s *DaemonSupervisor) ConfigureServeEndpoints(
	service string,
	endpoints []ServeEndpoint,
	tags []string,
	direct bool,
) error {
	s.mu.RLock()
	daemon := s.daemon
	s.mu.RUnlock()

	if daemon == nil {
		return errors.New("no daemon running")
	}

	return daemon.ConfigureServeEndpoints(service, endpoints, tags, direct)
}

// signalStartup safely sends the startup result exactly once.
func (s *DaemonSupervisor) signalStartup(err error) {
	s.startupDoneOnce.Do(func() {
		s.startupDone <- err
	})
}

// applyStartupJitter adds random delay to prevent thundering herd on mass restart.
func (s *DaemonSupervisor) applyStartupJitter() {
	//nolint:gosec // jitter needs no crypto randomness
	jitter := time.Duration(rand.Int64N(int64(maxStartupJitter)))
	if jitter > 0 {
		logger.Debugf("Applying startup jitter: %v", jitter)
		time.Sleep(jitter)
	}
}

// netnsExists checks if the network namespace still exists.
func (s *DaemonSupervisor) netnsExists() bool {
	_, err := os.Stat(s.cfg.NetNSPath)
	return err == nil
}

// supervisionLoop is the main loop that manages daemon lifecycle.
func (s *DaemonSupervisor) supervisionLoop() {
	defer s.wg.Done()

	firstStart := true

	// Apply jitter on first start to prevent thundering herd
	s.applyStartupJitter()

	for {
		select {
		case <-s.ctx.Done():
			s.stopDaemon()
			return
		default:
		}

		// Check if network namespace still exists (container might be gone)
		if !s.netnsExists() {
			logger.Errorf(
				"Network namespace %s no longer exists, stopping supervisor",
				s.cfg.NetNSPath,
			)
			s.mu.Lock()
			s.status = StatusFailed
			s.mu.Unlock()
			s.signalStartup(errors.New("network namespace no longer exists"))
			return
		}

		// Apply backoff (skip on first attempt)
		if !firstStart {
			// Record crash and check for crash loop AFTER recording
			s.recordCrash()

			if s.isCrashLoop() {
				s.mu.Lock()
				s.status = StatusCrashLoop
				s.mu.Unlock()
				logger.Errorf(
					"Crash loop detected for endpoint %s (5+ crashes in 30s), retrying in %v",
					s.cfg.EndpointID[:8],
					crashLoopCooldown,
				)
				s.signalStartup(errors.New("crash loop detected"))
				s.stopDaemon()

				select {
				case <-s.ctx.Done():
					return
				case <-time.After(crashLoopCooldown):
				}
				s.mu.Lock()
				s.crashTimes = s.crashTimes[:0]
				s.mu.Unlock()
				s.resetBackoff()
			}

			delay := s.nextBackoff()
			logger.Infof("Restarting tailscaled in %v (attempt %d)", delay, s.restartCount+1)

			select {
			case <-s.ctx.Done():
				return
			case <-time.After(delay):
			}
		}

		// Start daemon
		s.mu.Lock()
		s.status = StatusStarting
		s.mu.Unlock()

		err := s.startDaemon()
		if err != nil {
			logger.Errorf("Failed to start tailscaled: %v", err)

			if firstStart {
				s.signalStartup(err)
				firstStart = false
			}
			continue
		}

		// Daemon started successfully - record start time
		startTime := time.Now()
		s.mu.Lock()
		s.status = StatusRunning
		s.restartCount++
		s.lastStartTime = startTime
		s.mu.Unlock()

		if firstStart {
			s.signalStartup(nil)
			firstStart = false
		}

		logger.Infof("tailscaled running (restart count: %d)", s.restartCount)

		// Monitor daemon until it exits or we're stopped
		exitErr := s.waitForExit()
		if exitErr == nil {
			// Clean exit (Stop() was called)
			return
		}

		// Unexpected exit - check uptime before resetting backoff
		uptime := time.Since(startTime)
		if uptime >= minUptimeForBackoffReset {
			s.resetBackoff()
			logger.Debugf("Backoff reset after %v uptime", uptime)
		}

		logger.Warnf("tailscaled exited unexpectedly after %v: %v", uptime, exitErr)
	}
}

// startDaemon creates and starts a new daemon instance.
func (s *DaemonSupervisor) startDaemon() error {
	daemon, err := NewDaemon(s.cfg)
	if err != nil {
		return fmt.Errorf("failed to create daemon: %w", err)
	}

	if err := daemon.Start(); err != nil {
		return fmt.Errorf("failed to start daemon: %w", err)
	}

	s.mu.Lock()
	s.daemon = daemon
	s.mu.Unlock()

	return nil
}

// stopDaemon stops the current daemon if running.
func (s *DaemonSupervisor) stopDaemon() {
	s.mu.Lock()
	daemon := s.daemon
	s.daemon = nil
	s.mu.Unlock()

	if daemon != nil {
		if err := daemon.Stop(); err != nil {
			logger.Warnf("Error stopping daemon: %v", err)
		}
	}
}

// waitForExit monitors the daemon and returns when it exits.
// Returns nil if context was cancelled (clean shutdown).
// Returns error if daemon exited unexpectedly.
func (s *DaemonSupervisor) waitForExit() error {
	ticker := time.NewTicker(healthCheckInterval)
	defer ticker.Stop()
	var login loginWatch

	for {
		select {
		case <-s.ctx.Done():
			s.stopDaemon()
			return nil

		case <-ticker.C:
			s.mu.RLock()
			daemon := s.daemon
			s.mu.RUnlock()

			if daemon == nil {
				return errors.New("daemon is nil")
			}

			if !daemon.IsRunning() {
				return errors.New("daemon process exited")
			}

			// tailscaled keeps running when its node is deleted, logged out
			if login.observe(daemon.LoggedOut()) {
				logger.Warnf(
					"Endpoint %s: node is logged out, logging in again",
					s.cfg.EndpointID[:8],
				)
				if err := daemon.Reauthenticate(); err != nil {
					logger.Errorf(
						"Endpoint %s: logging in again failed: %v",
						s.cfg.EndpointID[:8],
						err,
					)
					login.failed()
				}
			}
		}
	}
}

// isCrashLoop checks if we've had too many crashes in the time window.
func (s *DaemonSupervisor) isCrashLoop() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cutoff := time.Now().Add(-crashLoopWindow)
	count := 0
	for _, t := range s.crashTimes {
		if t.After(cutoff) {
			count++
		}
	}
	return count >= crashLoopThreshold
}

// recordCrash records a crash timestamp for crash-loop detection.
func (s *DaemonSupervisor) recordCrash() {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	s.crashTimes = append(s.crashTimes, now)

	// Prune old crash times (keep last 10)
	if len(s.crashTimes) > 10 {
		s.crashTimes = s.crashTimes[len(s.crashTimes)-10:]
	}
}

// nextBackoff returns the next backoff duration using exponential backoff.
func (s *DaemonSupervisor) nextBackoff() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.backoffAttempt == 0 {
		s.backoffAttempt = 1
		return initialBackoff
	}

	// Exponential backoff: initialBackoff * 2^attempt
	delay := initialBackoff << s.backoffAttempt
	s.backoffAttempt++
	return min(delay, maxBackoff)
}

// resetBackoff resets the backoff counter after a successful start.
func (s *DaemonSupervisor) resetBackoff() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.backoffAttempt = 0
}
