package core

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// mustParseNetworkOptions parses opts, failing the test on an error.
func mustParseNetworkOptions(t *testing.T, opts map[string]any) NetworkOptions {
	t.Helper()
	o, err := ParseNetworkOptions(opts)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// A misspelt option or one whose value is not a string fails the network,
// rather than leaving it with another credential or other tags than meant.
// Docker's own options pass.
func TestParseNetworkOptionsRejectsUnknown(t *testing.T) {
	for _, opts := range []map[string]any{
		{"tslink.authkye": "tskey-auth-x"},
		{"tslink.tag": "tag:web"},
		{TagsOption: 1},
	} {
		if _, err := ParseNetworkOptions(map[string]any{GenericOptionsKey: opts}); err == nil {
			t.Errorf("%v accepted", opts)
		}
	}
	o := mustParseNetworkOptions(t, map[string]any{GenericOptionsKey: map[string]any{
		AuthKeyOption:                 "tskey-auth-x",
		"com.docker.network.internal": "true",
	}})
	if o.AuthKey != "tskey-auth-x" {
		t.Errorf("AuthKey = %q", o.AuthKey)
	}
}

// tslink takes no endpoint options: a container's settings are its labels.
func TestCheckEndpointOptions(t *testing.T) {
	if err := CheckEndpointOptions(map[string]any{HostnameLabel: "web"}); err == nil {
		t.Error("tslink.hostname as an endpoint option accepted")
	}
	if err := CheckEndpointOptions(map[string]any{
		"com.docker.network.endpoint.exposedports": nil,
	}); err != nil {
		t.Error(err)
	}
}

// referenceKeys returns the keys a reference page's table lists, in the
// first column of rows such as "| [`tslink.tags`](#tslinktags) | ...".
func referenceKeys(t *testing.T, page string) []string {
	t.Helper()
	b, err := os.ReadFile(
		"../../docs/reference/" + page,
	) // #nosec G304 -- the repository's own file
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, m := range regexp.MustCompile("(?m)^\\| \\[`([^`]+)`\\]").FindAllStringSubmatch(string(b), -1) {
		keys = append(keys, m[1])
	}
	slices.Sort(keys)
	return keys
}

// The network options reference lists exactly the options tslink parses.
func TestNetworkOptionsMatchReference(t *testing.T) {
	var keys []string
	for key := range networkOptions {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	if got := referenceKeys(t, "network-options.md"); !slices.Equal(got, keys) {
		t.Errorf("network-options.md lists %q, tslink parses %q", got, keys)
	}
	for _, key := range keys {
		if !strings.HasPrefix(key, KeyPrefix) && key != MTUOption {
			t.Errorf("option %s outside tslink's prefix", key)
		}
	}
}
