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

func TestParseTagsEmpty(t *testing.T) {
	if tags := ParseTags(""); tags != nil {
		t.Errorf("ParseTags(\"\") = %q, want nil", tags)
	}
}
