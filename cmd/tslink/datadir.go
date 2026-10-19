package main

import (
	"fmt"
	"os"
)

// hostVarLib is where Docker mounts the host's /var/lib into the plugin
// (docker/config.json). The plugin needs it only to create its data
// directory, and unmounts it before it serves.
const hostVarLib = "/host/var/lib"

// pluginDirs are the mount points the plugin's data directory is prepared in.
type pluginDirs struct {
	HostVarLib string // the host's /var/lib
	Data       string // where the data directory goes: TS_DATA_DIR
	Shared     string // the shared mount, TS_SHARED_DIR; empty for none
}

// sharedDirIsDefault reports whether the shared mount is the host's /var/lib,
// the default of shared.source: then no shared directory is set.
func sharedDirIsDefault(shared, varLib string) (bool, error) {
	sharedInfo, err := os.Stat(shared)
	if err != nil {
		return false, fmt.Errorf("failed to read the shared directory: %w", err)
	}
	varLibInfo, err := os.Stat(varLib)
	if err != nil {
		return false, fmt.Errorf("failed to read the host's /var/lib: %w", err)
	}
	return os.SameFile(sharedInfo, varLibInfo), nil
}
