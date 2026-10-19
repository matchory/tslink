package main

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// prepareDataDir creates the data directory, docker-plugins/tailscale in the
// host's /var/lib, mounts it at d.Data, and unmounts the host's /var/lib:
// the plugin no longer sees it once it serves. If the shared mount is the
// default, the host's /var/lib too, it is unmounted as well and the data
// directory is the shared directory, as before. It returns the shared
// directory. Any failure is fatal: the plugin never uses the host's /var/lib
// in place of its data directory.
func prepareDataDir(d pluginDirs) (string, error) {
	onHost := filepath.Join(d.HostVarLib, "docker-plugins", "tailscale")
	// The mode the installation guide created it with before
	if err := os.MkdirAll(onHost, 0o755); err != nil { //nolint:gosec // see above
		return "", fmt.Errorf("failed to create the data directory: %w", err)
	}
	if err := unix.Mount(onHost, d.Data, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return "", fmt.Errorf("failed to mount the data directory on %s: %w", d.Data, err)
	}
	shared := d.Shared
	if shared != "" {
		isDefault, err := sharedDirIsDefault(d.Shared, d.HostVarLib)
		if err != nil {
			return "", err
		}
		if isDefault {
			if err := unix.Unmount(d.Shared, unix.MNT_DETACH); err != nil {
				return "", fmt.Errorf("failed to unmount the host's /var/lib from %s: %w",
					d.Shared, err)
			}
			shared = d.Data
		}
	}
	if err := unix.Unmount(d.HostVarLib, unix.MNT_DETACH); err != nil {
		return "", fmt.Errorf("failed to unmount the host's /var/lib from %s: %w",
			d.HostVarLib, err)
	}
	return shared, nil
}
