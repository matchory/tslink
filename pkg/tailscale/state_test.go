package tailscale

import "testing"

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
