package tailscale

import (
	"fmt"
	"os"
	"path/filepath"
)

// BundledVersion is the only supported TS_VERSION: the Tailscale shipped in
// the plugin image.
const BundledVersion = "bundled"

// bundledDir holds the bundled binaries; a variable so tests can replace it.
var bundledDir = "/usr/local/bin"

// BundledBinaries returns the paths of the tailscale and tailscaled binaries
// shipped in the plugin image, so every node runs the same version.
func BundledBinaries() (tailscale, tailscaled string, err error) {
	ts := filepath.Join(bundledDir, "tailscale")
	tsd := filepath.Join(bundledDir, "tailscaled")
	if !fileExists(ts) || !fileExists(tsd) {
		return "", "", fmt.Errorf("bundled Tailscale binaries not found in %s", bundledDir)
	}
	return ts, tsd, nil
}

// fileExists checks if a file exists and is not a directory.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}
