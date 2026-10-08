package tailscale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"tailscale.com/client/local"

	"github.com/matchory/tslink/pkg/logger"
)

// Keys of tailscaled's state file (tailscale.com/ipn), a JSON object whose
// values are base64-encoded. "_current-profile" holds the key of the current
// profile's prefs; the node key is in the prefs' "Config".
const (
	stateMachineKey     = "_machinekey"
	stateCurrentProfile = "_current-profile"
	stateProfiles       = "_profiles"
)

// LogoutState logs out the node whose state is in stateDir, which deletes an
// ephemeral node from the tailnet and frees its name, for a node whose
// container is gone. tailscaled runs on a copy of the state that logoutState
// sanitized, with userspace networking and its socket at socket, so the node
// does not come up: it contacts control only to log out. A state without a
// registered node is nothing to log out.
func LogoutState(ctx context.Context, tailscaledBin, stateDir, socket string) error {
	raw, err := os.ReadFile(filepath.Join(stateDir, "tailscaled.state"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	state, err := logoutState(raw)
	if err != nil || state == nil {
		return err
	}

	// Inside stateDir, so the copy goes with the state even after a crash
	workDir, err := os.MkdirTemp(stateDir, "logout-")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(workDir); err != nil {
			logger.Warnf("Failed to remove %s: %v", workDir, err)
		}
	}()
	if err := os.WriteFile(filepath.Join(workDir, "tailscaled.state"), state, 0o600); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		return err
	}
	defer func() {
		if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
			logger.Warnf("Failed to remove %s: %v", socket, err)
		}
	}()
	return runLogout(ctx, tailscaledBin, workDir, socket)
}

// logoutState returns a copy of a tailscaled state file with only what
// logging the node out needs: the machine key and the current profile, whose
// prefs keep the node key and control server but are logged out, not
// running, and advertise no services. Serve config and everything else is
// dropped. tailscaled then starts its control client without logging in
// (LocalBackend.Start calls Login only if !LoggedOut), so the node polls no
// network map and is never online, and Logout still sends control the
// logout for the node key. It returns nil if the state holds no node key.
// Errors never include the state's contents: it holds private keys.
func logoutState(raw []byte) ([]byte, error) {
	var state map[string][]byte
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, errors.New("tailscaled.state is not a valid state file")
	}
	prefsKey := string(state[stateCurrentProfile])
	prefsRaw := state[prefsKey]
	if prefsKey == "" || prefsRaw == nil || state[stateMachineKey] == nil ||
		state[stateProfiles] == nil {
		return nil, nil
	}

	var prefs map[string]json.RawMessage
	if err := json.Unmarshal(prefsRaw, &prefs); err != nil {
		return nil, errors.New("tailscaled.state holds invalid prefs")
	}
	var persist struct {
		PrivateNodeKey string `json:"PrivateNodeKey"`
	}
	if cfg := prefs["Config"]; cfg != nil {
		if err := json.Unmarshal(cfg, &persist); err != nil {
			return nil, errors.New("tailscaled.state holds invalid prefs")
		}
	}
	if strings.Trim(strings.TrimPrefix(persist.PrivateNodeKey, "privkey:"), "0") == "" {
		return nil, nil
	}

	prefs["WantRunning"] = json.RawMessage("false")
	prefs["LoggedOut"] = json.RawMessage("true")
	delete(prefs, "AdvertiseServices")
	prefsOut, err := json.Marshal(prefs)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string][]byte{
		stateMachineKey:     state[stateMachineKey],
		stateCurrentProfile: state[stateCurrentProfile],
		stateProfiles:       state[stateProfiles],
		prefsKey:            prefsOut,
	})
}

// runLogout runs tailscaled on the state in workDir until it has logged out,
// or ctx ends. It is a variable so tests can replace it.
var runLogout = func(ctx context.Context, tailscaledBin, workDir, socket string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// --port=0: the plugin shares the host's network, where 41641 may be taken
	daemon := exec.CommandContext(ctx, tailscaledBin,
		"--state="+filepath.Join(workDir, "tailscaled.state"),
		"--statedir="+workDir,
		"--socket="+socket,
		"--tun=userspace-networking",
		"--port=0",
	)
	daemon.Env = withoutAuthKey(os.Environ())
	if err := daemon.Start(); err != nil {
		return fmt.Errorf("failed to start tailscaled: %w", err)
	}
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		// Killed below once the logout is done: its exit status says nothing
		if err := daemon.Wait(); err != nil && ctx.Err() == nil {
			logger.Debugf("tailscaled for logout exited: %v", err)
		}
	}()
	defer func() {
		cancel()
		<-exited
	}()

	lc := &local.Client{Socket: socket, UseSocketOnly: true}
	// The LocalAPI answers once the backend has started
	for {
		if _, err := lc.StatusWithoutPeers(ctx); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("tailscaled did not start: %w", ctx.Err())
		case <-exited:
			return errors.New("tailscaled exited before logging out")
		case <-time.After(200 * time.Millisecond):
		}
	}
	if err := lc.Logout(ctx); err != nil {
		return fmt.Errorf("tailscale logout failed: %w", err)
	}
	return nil
}

// withoutAuthKey returns env without TS_AUTHKEY: logging out needs no key.
func withoutAuthKey(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, "TS_AUTHKEY=") {
			out = append(out, kv)
		}
	}
	return out
}
