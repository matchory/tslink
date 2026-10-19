package tailscale

import "time"

// timing holds the intervals and timeouts of a supervisor and its daemons,
// which tests shorten through DaemonConfig.timing.
type timing struct {
	initialBackoff time.Duration // First restart delay, doubled up to maxBackoff
	maxBackoff     time.Duration

	// Pause after a crash loop before trying again; the container keeps
	// running, so giving up would leave it without its identity for good.
	crashLoopCooldown time.Duration

	healthCheckInterval time.Duration

	// hostEndpointsCheckInterval is how often the supervisor compares the
	// endpoints on the host tailscaled advertises with the current ones.
	hostEndpointsCheckInterval time.Duration

	// Startup jitter to prevent thundering herd on host reboot.
	maxStartupJitter time.Duration

	// stopGrace is how long Stop waits for tailscaled to exit on SIGTERM
	// before it kills it.
	stopGrace time.Duration

	// loginTimeout is how long a login, from the settings to a running node,
	// may take before tslink warns. tailscaled keeps a registration control
	// has not answered going: the login waits loginGrace longer for it, since
	// restarting tailscaled would register a second node.
	loginTimeout time.Duration
	loginGrace   time.Duration

	// certPollInterval spaces the checks for a certificate, and
	// certDomainGrace is how long the lease holder waits, unadvertised, for
	// control to make the Service's name one of its certificate domains.
	certPollInterval time.Duration
	certDomainGrace  time.Duration

	// certCheckInterval is how often the certificate's expiry is checked.
	certCheckInterval time.Duration

	// approvalGrace is how long control may take to approve a backend
	// before tslink warns that it has not.
	approvalGrace time.Duration

	// controlSync is how long a node waits after its login before configuring
	// its Services: they are a beta feature, and control needs a moment to
	// register the node.
	controlSync time.Duration

	// Leaving waits at least drainAckFloor and at most drainAckTimeout for
	// control to fetch the drained Service list.
	drainAckFloor   time.Duration
	drainAckTimeout time.Duration
}

var defaultTiming = timing{
	initialBackoff:             100 * time.Millisecond,
	maxBackoff:                 30 * time.Second,
	crashLoopCooldown:          5 * time.Minute,
	healthCheckInterval:        5 * time.Second,
	hostEndpointsCheckInterval: 60 * time.Second,
	maxStartupJitter:           500 * time.Millisecond,
	stopGrace:                  5 * time.Second,
	loginTimeout:               60 * time.Second,
	loginGrace:                 4 * time.Minute,
	certPollInterval:           5 * time.Second,
	certDomainGrace:            30 * time.Second,
	certCheckInterval:          time.Hour,
	approvalGrace:              2 * time.Minute,
	controlSync:                2 * time.Second,
	drainAckFloor:              time.Second,
	drainAckTimeout:            10 * time.Second,
}

// times returns a copy of the config's timing, or of the defaults without
// one: changing it changes nobody's timing.
func (c *DaemonConfig) times() timing {
	if c.timing == nil {
		return defaultTiming
	}
	return *c.timing
}
