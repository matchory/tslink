//go:build !linux

package main

import "errors"

// prepareDataDir needs Linux's mounts; the plugin runs only on Linux.
func prepareDataDir(pluginDirs) (string, error) {
	return "", errors.New("the plugin runs only on Linux")
}
