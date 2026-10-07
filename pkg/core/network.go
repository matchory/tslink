package core

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/aaomidi/tslink/pkg/tailscale"
)

// ClusterCredentialFile holds the cluster's OAuth client secret, in the
// plugin's data directory. Networks without tslink.authkey register their
// nodes with it, so stacks need no secret of their own.
const ClusterCredentialFile = "oauth-client.secret"

// Network represents a Docker network using Tailscale.
type Network struct {
	ID      string
	AuthKey string   // Empty if nodes register with the cluster credential
	Tags    []string // If set, overrides the tslink.tags container label
	MTU     int      // veth MTU; 0 for the default

	LoginServer string // control server URL; empty for Tailscale's
}

// NewNetwork builds a network from its options and resolves its credential:
// its tslink.authkey option, else the cluster credential, else the plugin's
// TS_AUTHKEY.
func NewNetwork(id string, opts NetworkOptions, cfg *Config) (*Network, error) {
	if err := ValidateLoginServer(opts.LoginServer); err != nil {
		return nil, err
	}
	n := &Network{ID: id, AuthKey: opts.AuthKey, Tags: opts.Tags, MTU: opts.MTU, LoginServer: opts.LoginServer}
	if n.AuthKey != "" {
		return n, nil
	}

	_, err := os.Stat(filepath.Join(cfg.DataDir, ClusterCredentialFile))
	switch {
	case err == nil:
		// Tags are checked against the stack when a task starts, since Docker
		// does not pass the network's labels here
		if len(n.Tags) == 0 {
			return nil, errors.New("tslink.tags is required with the cluster credential")
		}
		return n, nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("failed to check the cluster credential: %w", err)
	}

	n.AuthKey = cfg.AuthKey
	if n.AuthKey == "" {
		return nil, fmt.Errorf("no Tailscale credential: provide %s, set TS_AUTHKEY, or use --opt tslink.authkey=xxx",
			ClusterCredentialFile)
	}
	return n, nil
}

// UsesClusterCredential reports whether the network's nodes register with the
// cluster credential.
func (n *Network) UsesClusterCredential() bool {
	return n.AuthKey == ""
}

// Ephemeral reports whether the network's nodes are ephemeral. The cluster
// credential registers ephemeral nodes only.
func (n *Network) Ephemeral() bool {
	return n.UsesClusterCredential() || tailscale.IsEphemeralKey(n.AuthKey)
}

// Credential returns the key a node registers with. The cluster credential is
// read on every call, so replacing the file takes effect at the next
// registration.
func (n *Network) Credential(dataDir string) (string, error) {
	if !n.UsesClusterCredential() {
		return n.AuthKey, nil
	}
	path := filepath.Join(dataDir, ClusterCredentialFile)
	b, err := os.ReadFile(path) // #nosec G304 -- fixed name in the plugin's data directory
	if err != nil {
		return "", fmt.Errorf("failed to read the cluster credential: %w", err)
	}
	secret := strings.TrimSpace(string(b))
	if !strings.HasPrefix(secret, "tskey-client-") || strings.Contains(secret, "?") {
		return "", fmt.Errorf("%s must hold an OAuth client secret (tskey-client-...) with nothing appended", path)
	}
	return secret + "?ephemeral=true&preauthorized=true", nil
}

// CheckTagScope reports whether a stack may register nodes with tags using the
// cluster credential: the cluster client owns every stack's tags, so each
// stack is confined to tag:<stack> and tag:<stack>-*.
func CheckTagScope(stack string, tags []string) error {
	if stack == "" {
		return errors.New("the cluster credential is only available to networks of a stack: use tslink.authkey")
	}
	prefix := "tag:" + stack
	for _, tag := range tags {
		if tag != prefix && !strings.HasPrefix(tag, prefix+"-") {
			return fmt.Errorf("tag %q is outside stack %q's scope (%s or %s-*): use tslink.authkey for other tags",
				tag, stack, prefix, prefix)
		}
	}
	return nil
}
