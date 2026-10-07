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
		// tailscale up passes an auth key on unchanged: a parameter means nothing
		"tskey-auth-x?ephemeral=true",
	}
	// Ephemeral() per key, in the order of keys; "err" if NewNetwork fails
	tests := map[string][]string{
		"":      {"true", "false", "true", "false", "false", "true", "false"},
		"true":  {"true", "err", "true", "true", "true", "true", "true"},
		"false": {"false", "false", "err", "false", "false", "err", "false"},
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

func TestCheckTagScope(t *testing.T) {
	tests := []struct {
		stack   string
		tags    []string
		wantErr bool
	}{
		{stack: "billing", tags: []string{"tag:billing"}},
		{stack: "billing", tags: []string{"tag:billing-api", "tag:billing"}},
		{stack: "billing", tags: []string{"tag:shop"}, wantErr: true},
		{stack: "billing", tags: []string{"tag:billing", "tag:shop"}, wantErr: true},
		{stack: "billing", tags: []string{"tag:billingx"}, wantErr: true},
		{stack: "bill", tags: []string{"tag:billing"}, wantErr: true},
		{stack: "", tags: []string{"tag:billing"}, wantErr: true},
	}
	for _, tt := range tests {
		err := CheckTagScope(tt.stack, tt.tags)
		if (err != nil) != tt.wantErr {
			t.Errorf("CheckTagScope(%q, %v) = %v, wantErr %v", tt.stack, tt.tags, err, tt.wantErr)
		}
	}
}
