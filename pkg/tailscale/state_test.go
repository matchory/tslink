package tailscale

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsEphemeralKey(t *testing.T) {
	tests := map[string]bool{
		"tskey-client-abc": true, // OAuth default
		"tskey-client-abc?ephemeral=true&preauthorized=true":  true,
		"tskey-client-abc?preauthorized=true":                 true,
		"tskey-client-abc?ephemeral=false&preauthorized=true": false,
		"tskey-auth-abc":                false, // unknown from the key
		"tskey-auth-abc?ephemeral=true": true,
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
		if err := os.WriteFile(filepath.Join(dir, b), nil, 0755); err != nil {
			t.Fatal(err)
		}
	}
	ts, tsd, err := BundledBinaries()
	if err != nil || ts != filepath.Join(dir, "tailscale") || tsd != filepath.Join(dir, "tailscaled") {
		t.Errorf("BundledBinaries = %q, %q, %v", ts, tsd, err)
	}
}
