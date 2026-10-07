package core

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io/fs"
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
// within minAge is kept too, as it may belong to an endpoint still starting,
// and so is state claimed in claims. Collection claims the state it removes.
// In the shared certificate directory, it removes certificates and files of
// names no task uses any more.
func CollectGarbage(
	ctx context.Context,
	cfg *Config,
	claims *StateClaims,
	stateInUse, socketsInUse map[string]bool,
	minAge time.Duration,
) {
	dataDir := cfg.DataDir
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
		if stateInUse[dir] || !tailscale.IsMarkedEphemeral(dir) || !stale(dir) ||
			// An endpoint may have claimed it since the caller listed those in use
			!claims.claimFree(dir, gcClaim) {
			continue
		}
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			removeEphemeral(ctx, claims, dataDir, dir)
		})
	}

	socks, _ := filepath.Glob(filepath.Join(dataDir, "sock", "*.sock"))
	statuses, _ := filepath.Glob(filepath.Join(dataDir, "status", "*.json"))
	for _, file := range append(socks, statuses...) {
		id := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
		if socketsInUse[id] || !stale(file) {
			continue
		}
		logger.Infof("Removing file of unknown endpoint: %s", file)
		if err := os.Remove(file); err != nil {
			logger.Warnf("Failed to remove %s: %v", file, err)
		}
	}

	if cfg.SharedDir != "" {
		collectCertificates(filepath.Join(cfg.SharedDir, "certs"), time.Now())
	}
	wg.Wait()
}

// The shared certificate directory holds tailscaled's <domain>.crt and
// <domain>.key and its ACME account key, and tslink's <domain>.lease and
// .tmp-* files (see tailscale/certs.go).
const (
	acmeAccountKey  = "acme-account.key.pem"
	certExpiredKeep = 7 * 24 * time.Hour // how long an expired certificate is kept
	certOrphanAge   = 24 * time.Hour     // age of a key without certificate, or a temporary file, to remove
	certLeaseMaxAge = time.Hour          // a lease held is refreshed every 15 seconds
)

// collectCertificates removes what accumulates in the shared certificate
// directory, as certificates of per-task names do: certificates expired more
// than certExpiredKeep ago, keys without a certificate and temporary files
// older than certOrphanAge, and leases not refreshed for certLeaseMaxAge. It
// leaves the ACME account key, and files it does not know, alone. Hosts
// sharing the directory collect it at the same time, so a file gone already
// is no error.
func collectCertificates(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logger.Warnf("Failed to read the certificate directory %s: %v", dir, err)
		}
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(dir, name)
		switch {
		case !entry.Type().IsRegular() || name == acmeAccountKey:
		case strings.HasPrefix(name, ".tmp-"):
			removeOlder(path, now.Add(-certOrphanAge))
		case strings.HasSuffix(name, ".lease"):
			removeOlder(path, now.Add(-certLeaseMaxAge))
		case strings.HasSuffix(name, ".key"):
			if _, err := os.Lstat(
				strings.TrimSuffix(path, ".key") + ".crt",
			); errors.Is(
				err,
				fs.ErrNotExist,
			) {
				removeOlder(path, now.Add(-certOrphanAge))
			}
		case strings.HasSuffix(name, ".crt"):
			removeExpiredCert(path, now.Add(-certExpiredKeep))
		}
	}
}

// removeExpiredCert removes a certificate and its key if the certificate
// expired before cutoff. tailscaled renewing it on another host writes the
// key, then the certificate: both are kept if either was written since
// cutoff, or if the certificate changed while it was read.
func removeExpiredCert(crt string, cutoff time.Time) {
	key := strings.TrimSuffix(crt, ".crt") + ".key"
	st, err := os.Stat(crt)
	if err != nil || !st.ModTime().Before(cutoff) || !olderThan(key, cutoff) {
		return
	}
	notAfter, err := certNotAfter(crt)
	if err != nil || !notAfter.Before(cutoff) {
		return
	}
	if again, err := os.Stat(crt); err != nil || !again.ModTime().Equal(st.ModTime()) {
		return
	}
	logger.Infof("Removing certificate %s, expired %s", crt, notAfter.Format(time.DateOnly))
	removeFile(crt)
	if olderThan(key, cutoff) {
		removeFile(key)
	}
}

// certNotAfter returns the expiry of the first certificate in a PEM file.
func certNotAfter(path string) (time.Time, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- a file in the certificate directory
	if err != nil {
		return time.Time{}, err
	}
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return time.Time{}, errors.New("no certificate")
		}
		if block.Type == "CERTIFICATE" {
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return time.Time{}, err
			}
			return cert.NotAfter, nil
		}
	}
}

// olderThan reports whether path was last modified before cutoff, or is gone.
func olderThan(path string, cutoff time.Time) bool {
	st, err := os.Stat(path)
	return errors.Is(err, fs.ErrNotExist) || err == nil && st.ModTime().Before(cutoff)
}

// removeOlder removes path if it was last modified before cutoff.
func removeOlder(path string, cutoff time.Time) {
	st, err := os.Stat(path)
	if err != nil || !st.ModTime().Before(cutoff) {
		return
	}
	logger.Infof("Removing %s, unchanged since %s", path, st.ModTime().Format(time.DateTime))
	removeFile(path)
}

// removeFile removes path, which another host may have removed already.
func removeFile(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		logger.Warnf("Failed to remove %s: %v", path, err)
	}
}

// RemoveDownloadCache removes tailscale-bin, where versions before the
// bundled binaries cached the Tailscale releases they downloaded. Hosts
// upgraded from them keep it otherwise. It runs once at startup.
func RemoveDownloadCache(dataDir string) {
	cache := filepath.Join(dataDir, "tailscale-bin")
	if _, err := os.Lstat(cache); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			logger.Warnf("Failed to check for the old Tailscale download cache %s: %v", cache, err)
		}
		return
	}
	logger.Infof(
		"Removing the old Tailscale download cache %s: tslink uses its bundled binaries",
		cache,
	)
	if err := os.RemoveAll(cache); err != nil {
		logger.Warnf("Failed to remove %s: %v", cache, err)
	}
}

// removeEphemeral logs the ephemeral node whose state is in dir out, then
// removes the state. Without the logout the node would stay in the tailnet,
// offline, until Tailscale deletes it, and a container with the same
// tslink.hostname would come back under a new name. If the logout fails, as
// when the node is deleted already, the state goes anyway; when the plugin
// stops, it stays for the next collection.
func removeEphemeral(ctx context.Context, claims *StateClaims, dataDir, dir string) {
	defer claims.release(dir, gcClaim)

	logger.Infof("Logging out and removing ephemeral node no endpoint uses: %s", dir)
	logoutCtx, cancel := context.WithTimeout(ctx, gcLogoutTimeout)
	defer cancel()
	if err := logoutNode(logoutCtx, dataDir, dir); err != nil {
		if ctx.Err() != nil {
			logger.Infof("Logging out ephemeral node of %s interrupted, keeping its state", dir)
			return
		}
		logger.Warnf(
			"Failed to log out ephemeral node of %s, removing its state anyway: %v",
			dir,
			err,
		)
	}
	if err := os.RemoveAll(dir); err != nil {
		logger.Warnf("Failed to remove %s: %v", dir, err)
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
