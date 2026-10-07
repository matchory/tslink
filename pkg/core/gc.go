package core

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aaomidi/tslink/pkg/logger"
	"github.com/aaomidi/tslink/pkg/tailscale"
)

// CollectGarbage removes what endpoints leave behind when Leave never runs,
// as when the host loses power: the state of ephemeral nodes no endpoint
// uses, and sockets and status files of unknown endpoints. Non-ephemeral state is kept, since
// a container with the same hostname reuses its identity. Anything modified
// within minAge is kept too, as it may belong to an endpoint still starting.
func CollectGarbage(dataDir string, stateInUse, socketsInUse map[string]bool, minAge time.Duration) {
	cutoff := time.Now().Add(-minAge)
	stale := func(path string) bool {
		st, err := os.Stat(path)
		return err == nil && st.ModTime().Before(cutoff)
	}

	dirs, _ := filepath.Glob(filepath.Join(dataDir, "by-stack", "*", "*"))
	hostDirs, _ := filepath.Glob(filepath.Join(dataDir, "by-hostname", "*"))
	for _, dir := range append(dirs, hostDirs...) {
		if stateInUse[dir] || !tailscale.IsMarkedEphemeral(dir) || !stale(dir) {
			continue
		}
		logger.Info("Removing state of ephemeral node no endpoint uses: %s", dir)
		if err := os.RemoveAll(dir); err != nil {
			logger.Warn("Failed to remove %s: %v", dir, err)
		}
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
}
