package tailscale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/matchory/tslink/pkg/logger"
)

// Status represents the status of a Tailscale connection.
type Status struct {
	IP       string
	Hostname string
	Online   bool
}

// WaitForIP waits for Tailscale to get an IP address.
func (d *Daemon) WaitForIP() (*Status, error) {
	logger.Debugf("Waiting for Tailscale IP for endpoint %s", d.config.EndpointID)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return nil, errors.New("timeout waiting for Tailscale IP")
		default:
			status, err := d.getStatus()
			if err == nil && status.IP != "" {
				return status, nil
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
}

// getStatus gets the current Tailscale status.
func (d *Daemon) getStatus() (*Status, error) {
	ctx, cancel := context.WithTimeout(d.ctx, 10*time.Second)
	defer cancel()

	out, err := d.statusJSON(ctx)
	if err != nil {
		return nil, fmt.Errorf(
			"tailscale status failed: %w (output: %s)",
			err,
			strings.TrimSpace(out.combined()),
		)
	}

	var result struct {
		Self struct {
			TailscaleIPs []string `json:"TailscaleIPs"`
			HostName     string   `json:"HostName"`
			Online       bool     `json:"Online"`
		} `json:"Self"`
	}

	if err := json.Unmarshal([]byte(out.stdout), &result); err != nil {
		return nil, fmt.Errorf("failed to parse status: %w", err)
	}

	status := &Status{
		Hostname: result.Self.HostName,
		Online:   result.Self.Online,
	}

	if len(result.Self.TailscaleIPs) > 0 {
		status.IP = result.Self.TailscaleIPs[0]
	}

	return status, nil
}

// BackendState returns tailscaled's backend state, such as Running or
// NeedsLogin, from its LocalAPI: cheaper than running the CLI on every
// health check.
func (d *Daemon) BackendState() (string, error) {
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", d.socketPath)
			},
		},
	}
	defer client.CloseIdleConnections()

	req, err := http.NewRequestWithContext(d.ctx, http.MethodGet,
		"http://local-tailscaled.sock/localapi/v0/status?peers=false", nil)
	if err != nil {
		return "", fmt.Errorf("failed to build status request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to query tailscaled: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tailscaled status: %s", resp.Status)
	}
	var status struct {
		BackendState string `json:"BackendState"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return "", fmt.Errorf("failed to parse tailscaled status: %w", err)
	}
	return status.BackendState, nil
}
