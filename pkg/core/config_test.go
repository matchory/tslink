package core

import (
	"slices"
	"testing"
)

func TestParseNetworkOptionsTags(t *testing.T) {
	opts := ParseNetworkOptions(map[string]any{
		GenericOptionsKey: map[string]any{
			"tslink.authkey": "tskey-client-x",
			"tslink.tags":    " tag:svc-billing, tag:prod ,",
		},
	})
	if opts.AuthKey != "tskey-client-x" {
		t.Errorf("AuthKey = %q", opts.AuthKey)
	}
	if want := []string{"tag:svc-billing", "tag:prod"}; !slices.Equal(opts.Tags, want) {
		t.Errorf("Tags = %q, want %q", opts.Tags, want)
	}
}

func TestParseNetworkOptionsEphemeral(t *testing.T) {
	opts := ParseNetworkOptions(
		map[string]any{GenericOptionsKey: map[string]any{EphemeralOption: "true"}},
	)
	if opts.Ephemeral != "true" {
		t.Errorf("Ephemeral = %q, want true", opts.Ephemeral)
	}
}

func TestParseTagsEmpty(t *testing.T) {
	if tags := ParseTags(""); tags != nil {
		t.Errorf("ParseTags(\"\") = %q, want nil", tags)
	}
}

func TestParseNetworkOptionsMTU(t *testing.T) {
	tests := map[string]int{"1450": 1450, "": 0, "abc": 0, "100": 0, "70000": 0}
	for in, want := range tests {
		opts := ParseNetworkOptions(
			map[string]any{GenericOptionsKey: map[string]any{MTUOption: in}},
		)
		if opts.MTU != want {
			t.Errorf("MTU %q parsed as %d, want %d", in, opts.MTU, want)
		}
	}
}

func TestLoadConfigIgnoresDownloadSettings(t *testing.T) {
	// Settings of the removed runtime download must not keep the plugin
	// from starting after an upgrade; it runs the bundled binaries.
	t.Setenv("TS_VERSION", "1.76.6")
	t.Setenv("TS_PATH", "/opt/tailscale/bin")
	if _, err := LoadConfig(); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
}
