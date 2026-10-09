package tailscale

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"sync"
	"time"

	"github.com/matchory/tslink/pkg/logger"
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

// Backoff configuration; variables, so tests can shorten them.
var (
	initialBackoff = 100 * time.Millisecond
	maxBackoff     = 30 * time.Second

	// Crash loop detection: crashLoopThreshold crashes in crashLoopWindow.
	crashLoopWindow = 30 * time.Second

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

const crashLoopThreshold = 5

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

	// start starts a daemon; tests replace it.
	start func(*Daemon) error
}

// NewDaemonSupervisor creates a new supervisor for managing a tailscaled daemon.
func NewDaemonSupervisor(cfg DaemonConfig) *DaemonSupervisor {
	cfg.gate = &serviceGate{}
	ctx, cancel := context.WithCancel(context.Background())
	return &DaemonSupervisor{
		cfg:         cfg,
		ctx:         ctx,
		cancel:      cancel,
		status:      StatusStopped,
		crashTimes:  make([]time.Time, 0),
		startupDone: make(chan error, 1),
		start:       (*Daemon).Start,
	}
}

// Start begins the supervision loop and waits for initial daemon startup.
// Returns error if the initial startup fails.
func (s *DaemonSupervisor) Start() error {
	return s.StartContext(context.Background())
}

// StartContext is Start, but cancelling ctx before the initial startup
// succeeded, as the endpoint leaving does, stops the supervisor and aborts a
// daemon start in progress, its login included. Once started, the supervisor
// no longer depends on ctx. The caller still calls Stop after an error.
func (s *DaemonSupervisor) StartContext(ctx context.Context) error {
	s.mu.Lock()
	if s.status != StatusStopped {
		s.mu.Unlock()
		return fmt.Errorf("supervisor already running (status=%s)", s.status)
	}
	s.status = StatusStarting
	s.mu.Unlock()

	stop := context.AfterFunc(ctx, s.cancel)
	defer stop()

	// Start supervision loop in background. It outlives ctx: stop cancels
	// it with ctx only until the initial startup succeeded.
	s.wg.Add(1)
	go s.supervisionLoop() //nolint:contextcheck // see above

	// Wait for initial startup result
	select {
	case err := <-s.startupDone:
		if err != nil {
			return fmt.Errorf("initial daemon startup failed: %w", err)
		}
	case <-s.ctx.Done():
		return fmt.Errorf("initial daemon startup aborted: %w", s.ctx.Err())
	case <-time.After(90 * time.Second):
		s.cancel()
		return errors.New("timeout waiting for initial daemon startup")
	}
	if !stop() {
		return fmt.Errorf("initial daemon startup aborted: %w", ctx.Err())
	}
	return nil
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
// The backend stays drained, even if tailscaled is down and the drain fails:
// a daemon the supervisor starts later does not advertise it.
func (s *DaemonSupervisor) Drain(service string) error {
	s.cfg.gate.drained.Store(true)
	d := s.GetDaemon()
	if d == nil {
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

// Drained reports whether the node's Service backend has been drained.
func (s *DaemonSupervisor) Drained() bool {
	return s.cfg.gate.drained.Load()
}

// Readiness reports how far the node is from serving what its container asks
// for. A daemon not started, or restarting, is starting.
func (s *DaemonSupervisor) Readiness(ctx context.Context) Readiness {
	daemon := s.GetDaemon()
	if daemon == nil || !daemon.IsRunning() {
		return ReadyStarting
	}
	return daemon.readiness(ctx)
}

// WaitForIP waits for the daemon to get a Tailscale IP, or ctx to be done.
// Delegates to the underlying daemon.
func (s *DaemonSupervisor) WaitForIP(ctx context.Context) (*Status, error) {
	s.mu.RLock()
	daemon := s.daemon
	s.mu.RUnlock()

	if daemon == nil {
		return nil, errors.New("no daemon running")
	}

	return daemon.WaitForIP(ctx)
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
		if !firstStart && !s.waitBeforeRestart() {
			return
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

// waitBeforeRestart records a crash and waits out the backoff, or the
// crash-loop cooldown. It reports false if the supervisor stopped meanwhile.
func (s *DaemonSupervisor) waitBeforeRestart() bool {
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
			return false
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
		return false
	case <-time.After(delay):
		return true
	}
}

// startDaemon creates and starts a new daemon instance.
func (s *DaemonSupervisor) startDaemon() error {
	// Stopped: NewDaemon would create the state directory again, which the
	// endpoint may have removed
	if err := s.ctx.Err(); err != nil {
		return err
	}
	daemon, err := NewDaemon(s.cfg)
	if err != nil {
		return fmt.Errorf("failed to create daemon: %w", err)
	}

	// Stopping the supervisor aborts the start, and its login. Once started,
	// the daemon's own context lets Stop take tailscaled down gracefully.
	stop := context.AfterFunc(s.ctx, daemon.cancel)
	err = s.start(daemon)
	stop()
	if err != nil {
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
