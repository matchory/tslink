package tailscale

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/aaomidi/tslink/pkg/logger"
)

const authKeyHashFile = "authkey.sha256"

// IsEphemeralKey reports whether a node registered with key is ephemeral, as
// far as the key itself says. OAuth client secrets register ephemeral nodes
// unless "?ephemeral=false" is appended; an auth key's ephemerality is set
// when it is created and cannot be read from it, so it counts as not ephemeral.
func IsEphemeralKey(key string) bool {
	base, query, _ := strings.Cut(key, "?")
	if params, err := url.ParseQuery(query); err == nil && params.Has("ephemeral") {
		return params.Get("ephemeral") == "true"
	}
	return strings.HasPrefix(base, "tskey-client-")
}

// GetHostnameStateDir returns the state directory path for a hostname.
// The directory structure is: <dataDir>/by-hostname/<hostname>/.
func GetHostnameStateDir(dataDir, hostname string) string {
	return filepath.Join(dataDir, "by-hostname", hostname)
}

// StateExists checks if valid Tailscale state exists in the given directory.
// Returns true if tailscaled.state file exists and is non-empty.
func StateExists(stateDir string) bool {
	statePath := filepath.Join(stateDir, "tailscaled.state")
	info, err := os.Stat(statePath)
	if err != nil {
		return false
	}
	return info.Size() > 0
}

// WipeState removes all state files from the directory to allow fresh registration.
// Removes tailscaled.state, tailscaled.sock, debug.log, and authkey hash.
func WipeState(stateDir string) error {
	logger.Info("Wiping state directory: %s", stateDir)

	files := []string{
		filepath.Join(stateDir, "tailscaled.state"),
		filepath.Join(stateDir, "tailscaled.sock"),
		filepath.Join(stateDir, "debug.log"),
		filepath.Join(stateDir, authKeyHashFile),
	}

	for _, f := range files {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			logger.Warn("Failed to remove %s: %v", f, err)
		}
	}

	return nil
}

// hashAuthKey returns the SHA256 hash of an auth key.
func hashAuthKey(authKey string) string {
	h := sha256.Sum256([]byte(authKey))
	return hex.EncodeToString(h[:])
}

// SaveAuthKeyHash stores the SHA256 hash of the auth key in the state directory.
func SaveAuthKeyHash(stateDir, authKey string) error {
	hashPath := filepath.Join(stateDir, authKeyHashFile)
	hash := hashAuthKey(authKey)
	return os.WriteFile(hashPath, []byte(hash), 0600)
}

// CheckAuthKeyMatch checks if the provided auth key matches the stored hash.
// Returns true if they match or if no hash is stored (new state).
// Returns false if they don't match (auth key changed).
func CheckAuthKeyMatch(stateDir, authKey string) bool {
	hashPath := filepath.Join(stateDir, authKeyHashFile)
	stored, err := os.ReadFile(hashPath)
	if err != nil {
		if os.IsNotExist(err) {
			// No stored hash - this is a new state or old state without hash
			return true
		}
		logger.Warn("Failed to read auth key hash: %v", err)
		return true // Assume match on error to avoid breaking existing setups
	}

	currentHash := hashAuthKey(authKey)
	match := strings.TrimSpace(string(stored)) == currentHash
	if !match {
		logger.Info("Auth key changed - state will be wiped for fresh registration")
	}
	return match
}

// ephemeralMarker marks a state directory whose node is ephemeral, so it can
// be deleted when no endpoint uses it, as after a crash where Leave never ran.
const ephemeralMarker = "ephemeral"

// MarkEphemeral marks stateDir as holding an ephemeral node.
func MarkEphemeral(stateDir string) error {
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(stateDir, ephemeralMarker), nil, 0600)
}

// ClearEphemeral removes the mark MarkEphemeral set, if any, when stateDir is
// used by a network whose nodes are not ephemeral.
func ClearEphemeral(stateDir string) error {
	if err := os.Remove(filepath.Join(stateDir, ephemeralMarker)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// IsMarkedEphemeral reports whether stateDir was marked by MarkEphemeral.
func IsMarkedEphemeral(stateDir string) bool {
	_, err := os.Stat(filepath.Join(stateDir, ephemeralMarker))
	return err == nil
}
