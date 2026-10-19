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

// The control server URL uses https, or http with an IP literal: tailscaled
// resolves the control server's name with the container's resolvers, which a
// stack chooses, and plain http does not authenticate the server, so with a
// name a stack could have its nodes send their keys to a server of its own.
//
// Guards: G4
func TestValidateLoginServer(t *testing.T) {
	for s, ok := range map[string]bool{
		"":                              true,
		"https://headscale.example.com": true,
		"https://hs.example.com/prefix": true,
		"https://hs.example.com:8443":   true,
		"https://10.0.0.1":              true,
		"http://10.0.0.1:8080":          true,
		"http://10.0.0.1":               true,
		"http://[::1]:8080":             true,
		"http://[fd00::1]":              true,
		"http://headscale.example.com":  false, // a name: the container's resolvers answer
		"http://hs.example.com:8080":    false,
		"http://localhost:8080":         false, // a name too
		"http://localhost":              false,
		"http://0x7f.1":                 false, // no IP literal
		"http://010.0.0.1":              false,
		"http://10.0.0.1@evil.example":  false, // userinfo; the host is evil.example
		"https://user:pw@hs.example":    false,
		"http://user@10.0.0.1:8080":     false,
		"headscale.example.com":         false,
		"ftp://headscale.example.com":   false,
		"ws://10.0.0.1":                 false,
		"https://":                      false,
		"http://":                       false,
		"http://[::1":                   false,
		"http://::1":                    false,
		"//10.0.0.1":                    false,
	} {
		err := ValidateLoginServer(s)
		if (err == nil) != ok {
			t.Errorf("ValidateLoginServer(%q) = %v, want ok %v", s, err, ok)
		}
		if err != nil && !strings.Contains(err.Error(), LoginServerSetting) {
			t.Errorf("ValidateLoginServer(%q) = %v, want an error naming %s",
				s, err, LoginServerSetting)
		}
	}
}
