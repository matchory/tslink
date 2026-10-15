package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The control server is the plugin's: a network that names one is built, so
// one created before the option was removed is known, but refused, whatever
// its credential, and its nodes would use the plugin's server.
//
// Guards: G4
func TestNetworkRefusesLoginServerOption(t *testing.T) {
	dir := t.TempDir()
	writeClusterCredential(t, dir, "tskey-client-x")
	cfg := &Config{DataDir: dir, LoginServer: "https://hs.example"}
	for _, key := range []string{"", "hskey-auth-x"} { // "": the cluster credential
		for _, server := range []string{"https://control.example", ""} {
			opts := mustParseNetworkOptions(t, map[string]any{GenericOptionsKey: map[string]any{
				AuthKeyOption: key, TagsOption: "tag:app", LoginServerOption: server,
			}})
			n, err := NewNetwork("net", opts, cfg)
			if err != nil {
				t.Fatalf("key %q, server %q: NewNetwork: %v", key, server, err)
			}
			if err := n.Refused(); err == nil ||
				!strings.Contains(err.Error(), LoginServerSetting) {
				t.Errorf("key %q, server %q: Refused() = %v, want an error naming %s",
					key, server, err, LoginServerSetting)
			}
			if n.LoginServer != cfg.LoginServer {
				t.Errorf("key %q, server %q: LoginServer = %q, want the plugin's %q",
					key, server, n.LoginServer, cfg.LoginServer)
			}
		}
	}
}

// Every network uses the plugin's control server, with the cluster
// credential and with a key of its own alike.
//
// Guards: G4
func TestNewNetworkUsesPluginLoginServer(t *testing.T) {
	dir := t.TempDir()
	writeClusterCredential(t, dir, "tskey-client-x")
	for _, server := range []string{"https://hs.example", ""} {
		cfg := &Config{DataDir: dir, LoginServer: server}
		for _, key := range []string{"", "hskey-auth-x"} {
			n, err := NewNetwork(
				"net",
				NetworkOptions{AuthKey: key, Tags: []string{"tag:app"}},
				cfg,
			)
			if err != nil {
				t.Fatal(err)
			}
			if n.LoginServer != server || n.Refused() != nil {
				t.Errorf("key %q: LoginServer = %q, Refused() = %v; want %q and nil",
					key, n.LoginServer, n.Refused(), server)
			}
		}
	}
}

// An endpoint of a network that names a control server starts nothing, and
// its status says why.
//
// Guards: G4
func TestRunTailscaleRefusesLoginServerOption(t *testing.T) {
	data := t.TempDir()
	opts := mustParseNetworkOptions(t, map[string]any{
		AuthKeyOption: "hskey-auth-x", LoginServerOption: "https://control.example",
	})
	n, err := NewNetwork("net", opts, &Config{DataDir: data})
	if err != nil {
		t.Fatal(err)
	}
	e := NewEndpoint(
		"0123456789abcdef", n, EndpointOptions{}, &Config{DataDir: data}, NewStateClaims(),
	)
	e.SetSandboxKey("/var/run/docker/netns/test")
	// Bounded: an endpoint that does not refuse retries its start
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	e.RunTailscale(ctx, &ContainerInfo{Hostname: "web"})

	b, err := os.ReadFile(StatusPath(data, e.ID))
	if err != nil {
		t.Fatal(err)
	}
	var st EndpointStatusFile
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	if st.State != StatusFailed || !strings.Contains(st.Error, LoginServerSetting) {
		t.Errorf("status %+v, want failed naming %s", st, LoginServerSetting)
	}
	if _, err := os.Stat(filepath.Join(data, "by-hostname", "web")); !os.IsNotExist(err) {
		t.Errorf("the endpoint prepared a state directory (err=%v)", err)
	}
}

func TestValidateLoginServer(t *testing.T) {
	valid := []string{
		"",
		"https://headscale.example.com",
		"http://10.0.0.1:8080",
		"https://hs.example.com/prefix",
	}
	for _, s := range valid {
		if err := ValidateLoginServer(s); err != nil {
			t.Errorf("ValidateLoginServer(%q) = %v, want nil", s, err)
		}
	}
	invalid := []string{
		"headscale.example.com",
		"ftp://headscale.example.com",
		"https://",
		"http://[::1",
	}
	for _, s := range invalid {
		if err := ValidateLoginServer(s); err == nil {
			t.Errorf("ValidateLoginServer(%q) = nil, want an error", s)
		}
	}
}
