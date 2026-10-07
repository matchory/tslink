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

func TestEnsureBinariesBundled(t *testing.T) {
	dir := t.TempDir()
	orig := bundledDir
	bundledDir = dir
	t.Cleanup(func() { bundledDir = orig })

	if _, _, err := EnsureBinaries(BundledVersion, ""); err == nil {
		t.Fatal("want an error while the bundled binaries are missing")
	}
	for _, b := range []string{"tailscale", "tailscaled"} {
		if err := os.WriteFile(filepath.Join(dir, b), nil, 0755); err != nil {
			t.Fatal(err)
		}
	}
	ts, tsd, err := EnsureBinaries(BundledVersion, "")
	if err != nil || ts != filepath.Join(dir, "tailscale") || tsd != filepath.Join(dir, "tailscaled") {
		t.Errorf("EnsureBinaries = %q, %q, %v", ts, tsd, err)
	}
}
