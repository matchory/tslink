package tailscale

import (
	"context"
	"errors"
	"fmt"
	"time"

	"tailscale.com/ipn/ipnstate"

	"github.com/matchory/tslink/pkg/logger"
)

// Status represents the status of a Tailscale connection.
type Status struct {
	IP       string
	Hostname string
	Online   bool
}

// WaitForIP waits for Tailscale to get an IP address, or ctx to be done.
func (d *Daemon) WaitForIP(ctx context.Context) (*Status, error) {
	logger.Debugf("Waiting for Tailscale IP for endpoint %s", d.config.EndpointID)

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil, fmt.Errorf("waiting for Tailscale IP: %w", ctx.Err())
			}
			return nil, errors.New("timeout waiting for Tailscale IP")
		default:
			status, err := d.getStatus(ctx)
			if err == nil && status.IP != "" {
				return status, nil
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
}

// getStatus gets the current Tailscale status.
func (d *Daemon) getStatus(ctx context.Context) (*Status, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	st, err := d.lc.StatusWithoutPeers(ctx)
	if err != nil {
		return nil, fmt.Errorf("tailscale status failed: %w", err)
	}
	status := &Status{}
	if st.Self != nil {
		status.Hostname, status.Online = st.Self.HostName, st.Self.Online
		if len(st.Self.TailscaleIPs) > 0 {
			status.IP = st.Self.TailscaleIPs[0].String()
		}
	}
	return status, nil
}

// BackendState returns tailscaled's backend state, such as Running or
// NeedsLogin.
func (d *Daemon) BackendState() (string, error) {
	ctx, cancel := context.WithTimeout(d.ctx, 5*time.Second)
	defer cancel()
	st, err := d.lc.StatusWithoutPeers(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to query tailscaled: %w", err)
	}
	return st.BackendState, nil
}

// magicDNSSuffix returns the tailnet's MagicDNS suffix from a status, or "".
func magicDNSSuffix(st *ipnstate.Status) string {
	if st.CurrentTailnet == nil {
		return ""
	}
	return st.CurrentTailnet.MagicDNSSuffix
}
