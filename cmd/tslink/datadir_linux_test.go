package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

// TestPrepareDataDirInMountNamespace mounts temporary directories as Docker
// mounts the host's /var/lib and the shared directory into the plugin, in a
// mount namespace of its own, and prepares the data directory in them.
func TestPrepareDataDirInMountNamespace(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root for a mount namespace")
	}
	for _, ownShared := range []bool{false, true} {
		name := "default shared directory"
		if ownShared {
			name = "own shared directory"
		}
		t.Run(name, func(t *testing.T) {
			done := make(chan struct{})
			// The thread stays locked: Go terminates it with its namespace
			go func() {
				defer close(done)
				runtime.LockOSThread()
				testPrepareDataDir(t, ownShared)
			}()
			<-done
		})
	}
}

// testPrepareDataDir runs in a thread of its own: t.Fatal is not allowed.
func testPrepareDataDir(t *testing.T, ownShared bool) {
	t.Helper()
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		t.Errorf("unshare: %v", err)
		return
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		t.Errorf("make / private: %v", err)
		return
	}
	root := t.TempDir()
	dir := func(name string) string {
		p := filepath.Join(root, name)
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Error(err)
		}
		return p
	}
	// The host's /var/lib, and the plugin's mount points
	host, varLib, data, shared, own := dir("host"), dir("varlib"), dir("data"), dir("shared"),
		dir("own")
	sharedSource := host
	if ownShared {
		sharedSource = own
		// An install that had the data directory before
		if err := os.MkdirAll(
			filepath.Join(host, "docker-plugins", "tailscale"),
			0o700,
		); err != nil {
			t.Error(err)
		}
	}
	for _, m := range [][2]string{{host, varLib}, {sharedSource, shared}} {
		if err := unix.Mount(m[0], m[1], "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			t.Errorf("mount %s on %s: %v", m[0], m[1], err)
			return
		}
	}

	got, err := prepareDataDir(pluginDirs{HostVarLib: varLib, Data: data, Shared: shared})
	if err != nil {
		t.Errorf("prepareDataDir: %v", err)
		return
	}

	onHost := filepath.Join(host, "docker-plugins", "tailscale")
	if info, err := os.Stat(onHost); err != nil || !info.IsDir() {
		t.Errorf("the data directory on the host: %v, %v", info, err)
	}
	if err := os.WriteFile(filepath.Join(data, "state"), []byte("x"), 0o600); err != nil {
		t.Errorf("write to the data directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(onHost, "state")); err != nil {
		t.Errorf("a file in the data directory is not on the host: %v", err)
	}
	if entries, err := os.ReadDir(varLib); err != nil || len(entries) > 0 {
		t.Errorf("the host's /var/lib is still mounted: %v, %v", entries, err)
	}
	wantShared := shared
	if !ownShared {
		wantShared = data
		if entries, err := os.ReadDir(shared); err != nil || len(entries) > 0 {
			t.Errorf("the default shared mount, the host's /var/lib, is still mounted: %v, %v",
				entries, err)
		}
	} else if same, err := sharedDirIsDefault(shared, own); err != nil || !same {
		t.Errorf("the own shared directory is no longer mounted: %v, %v", same, err)
	}
	if got != wantShared {
		t.Errorf("prepareDataDir = %q, want %q", got, wantShared)
	}
}
