package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aaomidi/tslink/pkg/logger"
	"github.com/aaomidi/tslink/pkg/tailscale"
)

// gcClaim is the owner of a state directory garbage collection removes, so no
// endpoint starts on it while its node is logged out.
const gcClaim = "garbage collection"

// Logging a node out starts a tailscaled; collection waits for the logouts.
const (
	gcLogoutTimeout     = 30 * time.Second
	gcLogoutConcurrency = 4
)

// CollectGarbage removes what endpoints leave behind when Leave never runs,
// as when the host loses power: the state of ephemeral nodes no endpoint
// uses, and sockets and status files of unknown endpoints. It runs after
// recovery and then periodically. Non-ephemeral state is kept, since
// a container with the same hostname reuses its identity. Anything modified
// within minAge is kept too, as it may belong to an endpoint still starting.
func CollectGarbage(ctx context.Context, dataDir string, stateInUse, socketsInUse map[string]bool, minAge time.Duration) {
	cutoff := time.Now().Add(-minAge)
	stale := func(path string) bool {
		st, err := os.Stat(path)
		return err == nil && st.ModTime().Before(cutoff)
	}

	var wg sync.WaitGroup
	slots := make(chan struct{}, gcLogoutConcurrency)
	dirs, _ := filepath.Glob(filepath.Join(dataDir, "by-stack", "*", "*"))
	hostDirs, _ := filepath.Glob(filepath.Join(dataDir, "by-hostname", "*"))
	for _, dir := range append(dirs, hostDirs...) {
		if stateInUse[dir] || !tailscale.IsMarkedEphemeral(dir) || !stale(dir) || !claimUnclaimed(dir) {
			continue
		}
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			removeEphemeral(ctx, dataDir, dir)
		})
	}

	socks, _ := filepath.Glob(filepath.Join(dataDir, "sock", "*.sock"))
	statuses, _ := filepath.Glob(filepath.Join(dataDir, "status", "*.json"))
	for _, file := range append(socks, statuses...) {
		id := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
		if socketsInUse[id] || !stale(file) {
			continue
		}
		logger.Info("Removing file of unknown endpoint: %s", file)
		if err := os.Remove(file); err != nil {
			logger.Warn("Failed to remove %s: %v", file, err)
		}
	}
	wg.Wait()
}

// claimUnclaimed claims a state directory for garbage collection unless an
// endpoint has claimed it since the caller listed the endpoints in use.
func claimUnclaimed(dir string) bool {
	stateClaimsMu.Lock()
	defer stateClaimsMu.Unlock()
	if _, ok := stateClaims[dir]; ok {
		return false
	}
	stateClaims[dir] = gcClaim
	return true
}

// removeEphemeral logs the ephemeral node whose state is in dir out, then
// removes the state. Without the logout the node would stay in the tailnet,
// offline, until Tailscale deletes it, and a container with the same
// tslink.hostname would come back under a new name. If the logout fails, as
// when the node is deleted already, the state goes anyway; when the plugin
// stops, it stays for the next collection.
func removeEphemeral(ctx context.Context, dataDir, dir string) {
	defer func() {
		stateClaimsMu.Lock()
		defer stateClaimsMu.Unlock()
		if stateClaims[dir] == gcClaim {
			delete(stateClaims, dir)
		}
	}()

	logger.Info("Logging out and removing ephemeral node no endpoint uses: %s", dir)
	logoutCtx, cancel := context.WithTimeout(ctx, gcLogoutTimeout)
	defer cancel()
	if err := logoutNode(logoutCtx, dataDir, dir); err != nil {
		if ctx.Err() != nil {
			logger.Info("Logging out ephemeral node of %s interrupted, keeping its state", dir)
			return
		}
		logger.Warn("Failed to log out ephemeral node of %s, removing its state anyway: %v", dir, err)
	}
	if err := os.RemoveAll(dir); err != nil {
		logger.Warn("Failed to remove %s: %v", dir, err)
	}
}

// logoutNode logs the node whose state is in dir out. Its tailscaled's socket
// is in the data directory's sock/, so its path stays short; collection
// removes it if tailscaled leaves it behind. It is a variable so tests can
// replace it.
var logoutNode = func(ctx context.Context, dataDir, dir string) error {
	if !tailscale.StateExists(dir) {
		return nil
	}
	tailscaleBin, tailscaledBin, err := tailscale.BundledBinaries()
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(dir))
	socket := filepath.Join(dataDir, "sock", "gc-"+hex.EncodeToString(sum[:6])+".sock")
	return tailscale.LogoutState(ctx, tailscaleBin, tailscaledBin, dir, socket)
}
