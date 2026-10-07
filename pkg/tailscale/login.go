package tailscale

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/matchory/tslink/pkg/logger"
)

// nodeNotFoundLog is what tailscaled logs while control does not know its
// node key. BackendState stays Running meanwhile.
const nodeNotFoundLog = "404: node not found"

// bringUp runs "tailscale up" with retry logic for state reuse.
// First attempts with existing state, then wipes and retries on auth failures.
func (d *Daemon) bringUp() error {
	// A node that is still logged in comes up without the auth key. The CLI
	// exchanges an OAuth client secret for a key before it looks at the
	// node, so passing it would make every restart depend on the API and on
	// the secret still being valid: a revoked or rotated secret would take
	// down nodes that have working keys.
	if StateExists(d.config.StateDir) {
		if st := d.waitBackendState(); st != "" && st != "NeedsLogin" && st != "NoState" {
			err := d.tryBringUp(false)
			if err == nil {
				return nil
			}
			logger.Warnf("tailscale up with existing login failed, using the auth key: %v", err)
		}
	}

	// First attempt with existing state
	err := d.tryBringUp(true)
	if err == nil {
		return nil
	}

	// Check if it's an auth/state error that warrants a retry
	if isStateError(err) {
		logger.Warnf("Auth failed with existing state, wiping and retrying: %v", err)
		if err := WipeState(d.config.StateDir); err != nil {
			logger.Warnf("Failed to wipe state: %v", err)
		}

		// Second attempt with fresh state
		logger.Infof("Retrying tailscale up with fresh state...")
		return d.tryBringUp(true)
	}

	return err
}

// isStateError checks if the error indicates stale/invalid state that should trigger a retry.
func isStateError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	// Common auth/state errors that indicate we should wipe and retry
	return strings.Contains(errStr, "not logged in") ||
		strings.Contains(errStr, "key expired") ||
		strings.Contains(errStr, "node not found") ||
		strings.Contains(errStr, "node key mismatch") ||
		strings.Contains(errStr, "unauthorized") ||
		strings.Contains(errStr, "register request") ||
		strings.Contains(errStr, "invalid node key")
}

// waitBackendState returns tailscaled's backend state once it has loaded its
// state, or "" if it cannot tell within a few seconds.
func (d *Daemon) waitBackendState() string {
	deadline := time.Now().Add(5 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(d.ctx, 5*time.Second)
		out, err := exec.CommandContext(ctx, d.config.TailscaleBin, "--socket="+d.socketPath, "status", "--json").
			Output()
		cancel()
		var st struct {
			BackendState string `json:"BackendState"`
		}
		if err == nil && json.Unmarshal(out, &st) == nil && st.BackendState != "NoState" {
			return st.BackendState
		}
		if time.Now().After(deadline) {
			return ""
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// tryBringUp runs "tailscale up" to connect to the network (single attempt).
// Without withKey, it relies on the node still being logged in.
func (d *Daemon) tryBringUp(withKey bool, extraArgs ...string) error {
	logger.Infof("Bringing up Tailscale for endpoint %s", d.config.EndpointID)

	// The tailscale CLI communicates with tailscaled via the socket.
	// Since the socket is on the host filesystem, we don't need nsenter.
	// Disable netfilter mode since our plugin handles routing via veth pairs and NAT
	args := []string{
		"--socket=" + d.socketPath,
		"up",
		"--hostname=" + d.config.Hostname,
		"--accept-routes",
		"--netfilter-mode=off",
	}
	if withKey {
		// From stdin: arguments are visible to every process on the host
		args = append(args, "--authkey=file:/dev/stdin")
	}
	if d.config.LoginServer != "" {
		args = append(args, "--login-server="+d.config.LoginServer)
	}
	args = append(args, extraArgs...)

	// Add tags if configured (required for Services)
	if len(d.config.Tags) > 0 {
		tagsArg := strings.Join(d.config.Tags, ",")
		args = append(args, "--advertise-tags="+tagsArg)
	}

	// Log with redacted authkey
	redactedArgs := make([]string, len(args))
	for i, arg := range args {
		if strings.HasPrefix(arg, "--authkey=") {
			redactedArgs[i] = "--authkey=(redacted)"
		} else {
			redactedArgs[i] = arg
		}
	}
	logger.Debugf("Running: %s %v", d.config.TailscaleBin, redactedArgs)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, d.config.TailscaleBin, args...)
	if withKey {
		key, err := d.config.AuthKey()
		if err != nil {
			return err
		}
		cmd.Stdin = strings.NewReader(key)
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		logger.Errorf("tailscale up failed with output: %s", string(output))
		return fmt.Errorf("tailscale up failed: %w (output: %s)", err, string(output))
	}

	logger.Infof("tailscale up succeeded: %s", string(output))
	return nil
}

// Logout logs the node out, which deletes an ephemeral node from the tailnet.
func (d *Daemon) Logout() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := d.tailscale(ctx, "logout", "--socket="+d.socketPath, "logout")
	if err != nil {
		return fmt.Errorf("tailscale logout failed: %w (output: %s)", err, strings.TrimSpace(out))
	}
	return nil
}

// LoggedOut reports whether the node has lost its login: tailscaled needs a
// login, or control recently said it does not know the node. Being offline
// is not enough: the control plane may just be unreachable, and the node key
// still valid.
func (d *Daemon) LoggedOut() bool {
	if time.Since(time.Unix(d.nodeNotFound.Load(), 0)) < 30*time.Second {
		return true
	}
	state, err := d.BackendState()
	return err == nil && state == "NeedsLogin"
}

// Reauthenticate logs a node in again with the auth key, as a new device, and
// restores what it serves.
func (d *Daemon) Reauthenticate() error {
	if err := d.tryBringUp(true, "--force-reauth"); err != nil {
		return err
	}
	d.nodeNotFound.Store(0)
	if d.config.Direct && len(d.config.Endpoints) > 0 {
		if err := d.configureDirectServe(); err != nil {
			return fmt.Errorf("failed to configure direct serve: %w", err)
		}
	}
	if d.config.Service != "" {
		if err := d.configureServiceWhenCertified(); err != nil {
			return fmt.Errorf("failed to configure Tailscale service: %w", err)
		}
	}
	return nil
}
