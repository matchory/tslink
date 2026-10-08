package tailscale

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestIsEphemeralKey(t *testing.T) {
	tests := map[string]bool{
		"tskey-client-abc": true, // OAuth default
		"tskey-client-abc?ephemeral=true&preauthorized=true":  true,
		"tskey-client-abc?preauthorized=true":                 true,
		"tskey-client-abc?ephemeral=false&preauthorized=true": false,
		"tskey-auth-abc":                false, // unknown from the key
		"tskey-auth-abc?ephemeral=true": false, // passed on unchanged, not parsed
		"":                              false,
	}
	for key, want := range tests {
		if got := IsEphemeralKey(key); got != want {
			t.Errorf("IsEphemeralKey(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestClearEphemeral(t *testing.T) {
	dir := t.TempDir()
	if err := ClearEphemeral(dir); err != nil {
		t.Fatalf("ClearEphemeral on an unmarked directory: %v", err)
	}
	if err := MarkEphemeral(dir); err != nil {
		t.Fatal(err)
	}
	if err := ClearEphemeral(dir); err != nil {
		t.Fatal(err)
	}
	if IsMarkedEphemeral(dir) {
		t.Error("directory still marked ephemeral")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("directory removed: %v", err)
	}
}

func TestBundledBinaries(t *testing.T) {
	dir := t.TempDir()
	orig := bundledDir
	bundledDir = dir
	t.Cleanup(func() { bundledDir = orig })

	if _, _, err := BundledBinaries(); err == nil {
		t.Fatal("want an error while the bundled binaries are missing")
	}
	for _, b := range []string{"tailscale", "tailscaled"} {
		if err := os.WriteFile(filepath.Join(dir, b), nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ts, tsd, err := BundledBinaries()
	if err != nil || ts != filepath.Join(dir, "tailscale") ||
		tsd != filepath.Join(dir, "tailscaled") {
		t.Errorf("BundledBinaries = %q, %q, %v", ts, tsd, err)
	}
}

func TestCheckAuthKeyMatch(t *testing.T) {
	dir := t.TempDir()
	// State from before the hash was stored keeps its identity
	if !CheckAuthKeyMatch(dir, "tskey-auth-a") {
		t.Error("no stored hash must count as a match")
	}
	if err := SaveAuthKeyHash(dir, "tskey-auth-a"); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(filepath.Join(dir, authKeyHashFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), "tskey-auth-a") {
		t.Error("the key itself was stored")
	}
	if !CheckAuthKeyMatch(dir, "tskey-auth-a") {
		t.Error("the same key must match")
	}
	if CheckAuthKeyMatch(dir, "tskey-auth-b") {
		t.Error("a changed key must not match")
	}
}

func TestWipeState(t *testing.T) {
	dir := t.TempDir()
	wiped := []string{"tailscaled.state", "tailscaled.sock", "debug.log", authKeyHashFile}
	kept := []string{"tailscaled.log", ephemeralMarker}
	for _, f := range slices.Concat(wiped, kept) {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := WipeState(dir); err != nil {
		t.Fatal(err)
	}
	for _, f := range wiped {
		if _, err := os.Stat(filepath.Join(dir, f)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s not wiped: %v", f, err)
		}
	}
	for _, f := range kept {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s removed: %v", f, err)
		}
	}
	if StateExists(dir) {
		t.Error("state still exists after the wipe")
	}
	// Wiping again, with nothing left to remove, is no error
	if err := WipeState(dir); err != nil {
		t.Errorf("second wipe: %v", err)
	}
}
