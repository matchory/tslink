package tailscale

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/types/key"
	"tailscale.com/types/opt"

	"github.com/matchory/tslink/pkg/logger"
)

// nodeNotFoundLog is what tailscaled logs while control does not know its
// node key. BackendState stays Running meanwhile.
const nodeNotFoundLog = "404: node not found"

// needsLogin is tailscaled's backend state while the node is logged out.
const needsLogin = "NeedsLogin"

// bringUp logs the node in, or brings a node that is still logged in up
// with its settings. With existing state it first tries without the auth key,
// then wipes the state and retries on auth failures.
func (d *Daemon) bringUp() error {
	// A node that is still logged in comes up without the auth key. An OAuth
	// client secret is exchanged for a key through Tailscale's API, so
	// using it would make every restart depend on the API and on the secret
	// still being valid: a revoked or rotated secret would take down nodes
	// that have working keys.
	if StateExists(d.config.StateDir) {
		if st := d.waitBackendState(); st != "" && st != needsLogin && st != "NoState" {
			if d.registeredAsConfigured() {
				err := d.applySettings()
				if err == nil {
					return nil
				}
				logger.Warnf("Applying settings failed, logging in again: %v", err)
			}
			err := d.logIn(false, false)
			if err == nil {
				return nil
			}
			logger.Warnf("Logging in with the existing login failed, using the auth key: %v", err)
		}
	}

	// First attempt with existing state
	err := d.logIn(true, false)
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
		logger.Infof("Retrying the login with fresh state...")
		return d.logIn(true, false)
	}

	return err
}

// defaultControlURL is the control server tailscaled names in its prefs when
// none is set.
const defaultControlURL = "https://controlplane.tailscale.com"

// registeredAsConfigured reports whether the node's prefs name the configured
// control server and tags, so that only its settings may differ. Other tags
// or another server need a new login.
func (d *Daemon) registeredAsConfigured() bool {
	ctx, cancel := context.WithTimeout(d.ctx, 10*time.Second)
	defer cancel()
	prefs, err := d.lc.GetPrefs(ctx)
	if err != nil {
		return false
	}
	want := cmp.Or(d.config.LoginServer, defaultControlURL)
	return strings.TrimSuffix(prefs.ControlURL, "/") == strings.TrimSuffix(want, "/") &&
		slices.Equal(slices.Sorted(slices.Values(prefs.AdvertiseTags)),
			slices.Sorted(slices.Values(d.config.Tags)))
}

// settings returns the node's settings as tailscaled's config file states
// them, the way the Kubernetes operator configures its proxies. Netfilter is
// off: the plugin handles routing via veth pairs.
func (d *Daemon) settings() *ipn.ConfigVAlpha {
	return &ipn.ConfigVAlpha{
		Version:             "alpha0",
		ServerURL:           new(cmp.Or(d.config.LoginServer, defaultControlURL)),
		Hostname:            new(d.config.Hostname),
		AcceptDNS:           opt.NewBool(true),
		AcceptRoutes:        opt.NewBool(true),
		NetfilterMode:       new("off"),
		NoStatefulFiltering: opt.NewBool(true),
	}
}

// settingsPrefs returns the prefs edit the settings make, as tailscaled would
// apply them from its config file, but leaving the node's Services alone: the
// config file always sets them, and tslink's serve configuration advertises
// them instead.
func (d *Daemon) settingsPrefs() (*ipn.MaskedPrefs, error) {
	mp, err := d.settings().ToPrefs()
	if err != nil {
		return nil, fmt.Errorf("invalid settings: %w", err)
	}
	mp.AdvertiseServicesSet = false
	mp.RelayServerPortSet = false
	mp.RelayServerStaticEndpointsSet = false
	return &mp, nil
}

// applySettings applies the settings to a node that is logged in. Unlike a
// new login, editing prefs leaves the control client running: tailscaled
// dials control as it starts, and a client restarted within 2 minutes of a
// dial dials port 443 only, retry after retry, whatever the login server's
// port.
func (d *Daemon) applySettings() error {
	ctx, cancel := context.WithTimeout(d.ctx, 60*time.Second)
	defer cancel()
	mp, err := d.settingsPrefs()
	if err != nil {
		return err
	}
	if _, err := d.lc.EditPrefs(ctx, mp); err != nil {
		return fmt.Errorf("failed to apply settings: %w", err)
	}
	return nil
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
		st, err := d.lc.StatusWithoutPeers(ctx)
		cancel()
		if err == nil && st.BackendState != "NoState" {
			return st.BackendState
		}
		if time.Now().After(deadline) || d.ctx.Err() != nil {
			return ""
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// loginTimeout bounds a login, from the settings to a running node.
var loginTimeout = 60 * time.Second

// logIn logs the node in as "tailscale up" does: it applies the settings and
// tags, starts tailscaled's login and waits until the node runs. Without
// withKey, it relies on the node still having its node key. With
// forceReauth, the node logs in as a new device, with a new node key.
//
// The auth key reaches tailscaled over its socket, never through arguments,
// the environment or a file. An OAuth client secret is first exchanged for a
// single-use key with the node's tags.
func (d *Daemon) logIn(withKey, forceReauth bool) error {
	logger.Infof("Bringing up Tailscale for endpoint %s", d.config.EndpointID)
	ctx, cancel := context.WithTimeout(d.ctx, loginTimeout)
	defer cancel()

	// Watch first, or the notifications the login causes may be missed
	watcher, err := d.lc.WatchIPNBus(ctx, 0)
	if err != nil {
		return fmt.Errorf("failed to watch tailscaled: %w", err)
	}
	defer func() {
		if err := watcher.Close(); err != nil {
			logger.Debugf("Closing the IPN bus watch failed: %v", err)
		}
	}()

	st, err := d.lc.StatusWithoutPeers(ctx)
	if err != nil {
		return fmt.Errorf("tailscale status failed: %w", err)
	}
	var oldKey key.NodePublic
	if st.Self != nil {
		oldKey = st.Self.PublicKey
	}

	mp, err := d.settingsPrefs()
	if err != nil {
		return err
	}
	mp.AdvertiseTags, mp.AdvertiseTagsSet = d.config.Tags, true
	if _, err := d.lc.EditPrefs(ctx, mp); err != nil {
		return fmt.Errorf("failed to apply settings: %w", err)
	}

	var authKey string
	if withKey {
		secret, err := d.config.AuthKey()
		if err != nil {
			return err
		}
		if authKey, err = resolveAuthKey(ctx, secret, d.config.Tags); err != nil {
			return err
		}
	}
	if err := d.lc.Start(ctx, ipn.Options{AuthKey: authKey}); err != nil {
		return fmt.Errorf("failed to start the login: %w", err)
	}
	if forceReauth || !st.HaveNodeKey {
		if err := d.lc.StartLoginInteractive(ctx); err != nil {
			return fmt.Errorf("failed to start the login: %w", err)
		}
	}

	if err := waitRunning(ctx, watcher, oldKey, forceReauth); err != nil {
		logger.Errorf("Login for endpoint %s failed: %v", d.config.EndpointID, err)
		return err
	}
	logger.Infof("Tailscale is up for endpoint %s", d.config.EndpointID)
	return nil
}

// waitRunning waits on the IPN bus until the node runs and, with newKey, has
// a node key other than oldKey. An error tailscaled reports fails the login.
func waitRunning(
	ctx context.Context,
	watcher *local.IPNBusWatcher,
	oldKey key.NodePublic,
	newKey bool,
) error {
	running := false
	for {
		n, err := watcher.Next()
		if err != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				return fmt.Errorf("login aborted: %w", ctx.Err())
			}
			if ctx.Err() != nil {
				return errors.New("timeout waiting for Tailscale to run")
			}
			return fmt.Errorf("lost tailscaled's IPN bus: %w", err)
		}
		if n.ErrMessage != nil {
			return fmt.Errorf("login failed: %s", *n.ErrMessage)
		}
		if n.State != nil {
			running = *n.State == ipn.Running
		}
		if n.SelfChange != nil && n.SelfChange.Key != oldKey {
			newKey = false
		}
		if running && !newKey {
			return nil
		}
	}
}

// Logout logs the node out, which deletes an ephemeral node from the tailnet.
func (d *Daemon) Logout() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := d.lc.Logout(ctx); err != nil {
		return fmt.Errorf("tailscale logout failed: %w", err)
	}
	return nil
}

// LoggedOut reports whether the node has lost its login: tailscaled needs a
// login, or control recently said it does not know the node. Being offline
// is not enough: the control plane may just be unreachable, and the node key
// still valid.
func (d *Daemon) LoggedOut() bool {
	if d.nodeNotFoundRecently() {
		return true
	}
	state, err := d.BackendState()
	return err == nil && state == needsLogin
}

// nodeNotFoundRecently reports whether control said in the last 30 seconds
// that it does not know the node.
func (d *Daemon) nodeNotFoundRecently() bool {
	return time.Since(time.Unix(d.nodeNotFound.Load(), 0)) < 30*time.Second
}

// Reauthenticate logs a node in again with the auth key, as a new device, and
// restores what it serves.
func (d *Daemon) Reauthenticate() error {
	if err := d.logIn(true, true); err != nil {
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
