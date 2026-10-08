package core

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func writeClusterCredential(t *testing.T, dir, secret string) {
	t.Helper()
	if err := os.WriteFile(
		filepath.Join(dir, ClusterCredentialFile),
		[]byte(secret),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
}

func TestNewNetworkPrecedence(t *testing.T) {
	tags := []string{"tag:web"}
	tests := []struct {
		name        string
		option      string
		env         string
		clusterFile bool
		wantKey     string // "" means the cluster credential
		wantErr     bool
	}{
		{
			name:        "option wins over cluster file",
			option:      "tskey-auth-opt",
			env:         "tskey-auth-env",
			clusterFile: true,
			wantKey:     "tskey-auth-opt",
		},
		{name: "cluster file wins over env", env: "tskey-auth-env", clusterFile: true, wantKey: ""},
		{name: "env as last resort", env: "tskey-auth-env", wantKey: "tskey-auth-env"},
		{name: "no credential", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.clusterFile {
				writeClusterCredential(t, dir, "tskey-client-x")
			}
			cfg := &Config{DataDir: dir, AuthKey: tt.env}
			n, err := NewNetwork("net", NetworkOptions{AuthKey: tt.option, Tags: tags}, cfg)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if n.AuthKey != tt.wantKey {
				t.Errorf("AuthKey = %q, want %q", n.AuthKey, tt.wantKey)
			}
		})
	}
}

func TestNewNetworkClusterCredentialRequiresTags(t *testing.T) {
	dir := t.TempDir()
	writeClusterCredential(t, dir, "tskey-client-x")
	_, err := NewNetwork("net", NetworkOptions{}, &Config{DataDir: dir})
	if err == nil || !strings.Contains(err.Error(), "tslink.tags") {
		t.Fatalf("err = %v, want tslink.tags required", err)
	}
}

func TestCredential(t *testing.T) {
	t.Run("explicit key", func(t *testing.T) {
		n := &Network{AuthKey: "tskey-auth-x"}
		if got, err := n.Credential(t.TempDir()); err != nil || got != "tskey-auth-x" {
			t.Errorf("Credential() = %q, %v", got, err)
		}
	})

	t.Run("cluster credential is read on every call", func(t *testing.T) {
		dir := t.TempDir()
		n := &Network{}
		writeClusterCredential(t, dir, "tskey-client-old\n")
		if got, err := n.Credential(
			dir,
		); err != nil ||
			got != "tskey-client-old?ephemeral=true&preauthorized=true" {
			t.Errorf("Credential() = %q, %v", got, err)
		}
		writeClusterCredential(t, dir, "tskey-client-new")
		if got, err := n.Credential(
			dir,
		); err != nil ||
			got != "tskey-client-new?ephemeral=true&preauthorized=true" {
			t.Errorf("after rotation, Credential() = %q, %v", got, err)
		}
	})

	for name, secret := range map[string]string{
		"auth key instead of OAuth secret": "tskey-auth-x",
		"parameters appended":              "tskey-client-x?ephemeral=false",
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			dir := t.TempDir()
			writeClusterCredential(t, dir, secret)
			if _, err := (&Network{}).Credential(dir); err == nil {
				t.Error("expected error")
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		if _, err := (&Network{}).Credential(t.TempDir()); err == nil {
			t.Error("expected error")
		}
	})
}

func TestEphemeral(t *testing.T) {
	const cluster = "" // the cluster credential
	keys := []string{
		"tskey-client-x", "tskey-client-x?ephemeral=false", "tskey-client-x?ephemeral=true",
		"tskey-auth-x", "hskey-auth-x", cluster,
	}
	// Ephemeral() per key, in the order of keys; "err" if NewNetwork fails
	tests := map[string][]string{
		"":      {"true", "false", "true", "false", "false", "true"},
		"true":  {"true", "err", "true", "true", "true", "true"},
		"false": {"false", "false", "err", "false", "false", "err"},
	}
	for option, wants := range tests {
		for i, key := range keys {
			t.Run("option="+option+"/key="+key, func(t *testing.T) {
				dir := t.TempDir()
				if key == cluster {
					writeClusterCredential(t, dir, "tskey-client-x")
				}
				opts := NetworkOptions{AuthKey: key, Tags: []string{"tag:web"}, Ephemeral: option}
				n, err := NewNetwork("net", opts, &Config{DataDir: dir})
				if wants[i] == "err" {
					if err == nil || !strings.Contains(err.Error(), "tslink.ephemeral") {
						t.Fatalf("err = %v, want an error naming tslink.ephemeral", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if got := strconv.FormatBool(n.Ephemeral()); got != wants[i] {
					t.Errorf("Ephemeral() = %s, want %s", got, wants[i])
				}
			})
		}
	}
}

func TestNewNetworkRejectsParametersOnNonOAuthKeys(t *testing.T) {
	// tailscale up parses parameters of OAuth client secrets only, and passes
	// any other key to control unchanged, where the suffix makes it invalid
	for _, key := range []string{
		"tskey-auth-k3y?ephemeral=true", "hskey-auth-k3y?preauthorized=true", "tskey-auth-k3y?",
	} {
		for _, source := range []string{"tslink.authkey", "TS_AUTHKEY"} {
			t.Run(source+"/"+key, func(t *testing.T) {
				opts := NetworkOptions{}
				cfg := &Config{DataDir: t.TempDir()}
				if source == "TS_AUTHKEY" {
					cfg.AuthKey = key
				} else {
					opts.AuthKey = key
				}
				_, err := NewNetwork("net", opts, cfg)
				if err == nil || !strings.Contains(err.Error(), source) {
					t.Fatalf("err = %v, want an error naming %s", err, source)
				}
				if strings.Contains(err.Error(), "k3y") {
					t.Errorf("err = %v reveals the key", err)
				}
			})
		}
	}
}

func TestKeyEphemeralParam(t *testing.T) {
	tests := []struct {
		key       string
		want, has bool
	}{
		{key: "tskey-client-x"},
		{key: "tskey-client-x?ephemeral=true", want: true, has: true},
		{key: "tskey-client-x?preauthorized=true&ephemeral=false", has: true},
		{key: "tskey-client-x?preauthorized=true"},
		// Only OAuth client secrets take parameters
		{key: "tskey-auth-x?ephemeral=true"},
		{key: "hskey-auth-x?ephemeral=false"},
	}
	for _, tt := range tests {
		if got, has := keyEphemeralParam(tt.key); got != tt.want || has != tt.has {
			t.Errorf(
				"keyEphemeralParam(%q) = %t, %t, want %t, %t",
				tt.key,
				got,
				has,
				tt.want,
				tt.has,
			)
		}
	}
}

func TestNewNetworkRejectsInvalidEphemeral(t *testing.T) {
	for _, v := range []string{"yes", "1", "TRUE", " true"} {
		_, err := NewNetwork(
			"net",
			NetworkOptions{AuthKey: "tskey-auth-x", Ephemeral: v},
			&Config{DataDir: t.TempDir()},
		)
		if err == nil || !strings.Contains(err.Error(), "tslink.ephemeral") {
			t.Errorf("Ephemeral %q: err = %v, want an error naming tslink.ephemeral", v, err)
		}
	}
}

func TestCredentialAppliesEphemeralOption(t *testing.T) {
	tests := []struct {
		key, option, want string
	}{
		{key: "tskey-client-x", want: "tskey-client-x"},
		{key: "tskey-client-x", option: "true", want: "tskey-client-x?ephemeral=true"},
		{key: "tskey-client-x", option: "false", want: "tskey-client-x?ephemeral=false"},
		{
			key:    "tskey-client-x?preauthorized=true",
			option: "false",
			want:   "tskey-client-x?preauthorized=true&ephemeral=false",
		},
		{
			key:    "tskey-client-x?ephemeral=false",
			option: "false",
			want:   "tskey-client-x?ephemeral=false",
		},
		// Auth keys take no parameters: their ephemerality is set at creation
		{key: "tskey-auth-x", option: "true", want: "tskey-auth-x"},
		{key: "hskey-auth-x", option: "true", want: "hskey-auth-x"},
	}
	for _, tt := range tests {
		dir := t.TempDir()
		n, err := NewNetwork(
			"net",
			NetworkOptions{AuthKey: tt.key, Ephemeral: tt.option},
			&Config{DataDir: dir},
		)
		if err != nil {
			t.Fatalf("%s with %q: %v", tt.key, tt.option, err)
		}
		if got, err := n.Credential(dir); err != nil || got != tt.want {
			t.Errorf(
				"%s with %q: Credential() = %q, %v, want %q",
				tt.key,
				tt.option,
				got,
				err,
				tt.want,
			)
		}
	}
}

// Guards: G4
func TestCheckTagScope(t *testing.T) {
	tests := []struct {
		stack   string
		tags    []string
		strict  bool // exact match; false is the prefix opt-out
		wantErr bool
	}{
		// Exact match (the default): a stack uses only tag:<stack>.
		{stack: "billing", tags: []string{"tag:billing"}, strict: true},
		{stack: "billing", tags: []string{"tag:billing-api"}, strict: true, wantErr: true},
		{
			stack:   "billing",
			tags:    []string{"tag:billing-api", "tag:billing"},
			strict:  true,
			wantErr: true,
		},
		{stack: "billing", tags: []string{"tag:shop"}, strict: true, wantErr: true},
		{stack: "billing", tags: []string{"tag:billingx"}, strict: true, wantErr: true},
		// The L4 overlap: stack "a" must not claim stack "a-b"'s base tag.
		{stack: "a", tags: []string{"tag:a-b"}, strict: true, wantErr: true},
		{stack: "", tags: []string{"tag:billing"}, strict: true, wantErr: true},

		// Prefix opt-out (TSLINK_TAG_SCOPE=prefix): tag:<stack> or tag:<stack>-*.
		{stack: "billing", tags: []string{"tag:billing"}},
		{stack: "billing", tags: []string{"tag:billing-api", "tag:billing"}},
		{stack: "billing", tags: []string{"tag:shop"}, wantErr: true},
		{stack: "billing", tags: []string{"tag:billingx"}, wantErr: true},
		{stack: "bill", tags: []string{"tag:billing"}, wantErr: true},
		{stack: "", tags: []string{"tag:billing"}, wantErr: true},
		// The overlap is reachable only in this opt-out mode.
		{stack: "a", tags: []string{"tag:a-b"}},
	}
	for _, tt := range tests {
		err := CheckTagScope(tt.stack, tt.tags, tt.strict)
		if (err != nil) != tt.wantErr {
			t.Errorf("CheckTagScope(%q, %v, strict=%v) = %v, wantErr %v",
				tt.stack, tt.tags, tt.strict, err, tt.wantErr)
		}
	}
}

// NewNetwork carries the plugin's tag-scope mode onto the network, so the
// scope check sees the operator's setting.
//
// Guards: G4
func TestNewNetworkCarriesTagScope(t *testing.T) {
	for _, strict := range []bool{true, false} {
		cfg := &Config{DataDir: t.TempDir(), AuthKey: "tskey-auth-x", StrictTagScope: strict}
		n, err := NewNetwork("net", NetworkOptions{AuthKey: "tskey-auth-x"}, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if n.StrictTagScope != strict {
			t.Errorf("StrictTagScope = %v, want %v", n.StrictTagScope, strict)
		}
	}
}
