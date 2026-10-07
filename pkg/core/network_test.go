package core

import (
	"os"
	"path/filepath"
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
	tests := map[string]bool{
		"tskey-client-x":                 true,
		"tskey-client-x?ephemeral=false": false,
		"tskey-auth-x":                   false,
	}
	for key, want := range tests {
		if got := (&Network{AuthKey: key}).Ephemeral(); got != want {
			t.Errorf("Ephemeral(%q) = %v, want %v", key, got, want)
		}
	}
	if !(&Network{}).Ephemeral() {
		t.Error("cluster credential network is not ephemeral")
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
