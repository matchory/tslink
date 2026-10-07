package core

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/matchory/tslink/pkg/tailscale"
)

// ClusterCredentialFile holds the cluster's OAuth client secret, in the
// plugin's data directory. Networks without tslink.authkey register their
// nodes with it, so stacks need no secret of their own.
const ClusterCredentialFile = "oauth-client.secret" //nolint:gosec // a file name, not a credential

// Network represents a Docker network using Tailscale.
type Network struct {
	ID      string
	AuthKey string   // Empty if nodes register with the cluster credential
	Tags    []string // If set, overrides the tslink.tags container label
	MTU     int      // veth MTU; 0 for the default

	LoginServer string // control server URL; empty for Tailscale's

	ephemeral *bool // tslink.ephemeral; nil if unset
}

// NewNetwork builds a network from its options and resolves its credential:
// its tslink.authkey option, else the cluster credential, else the plugin's
// TS_AUTHKEY.
func NewNetwork(id string, opts NetworkOptions, cfg *Config) (*Network, error) {
	if err := ValidateLoginServer(opts.LoginServer); err != nil {
		return nil, err
	}
	n := &Network{
		ID:          id,
		AuthKey:     opts.AuthKey,
		Tags:        opts.Tags,
		MTU:         opts.MTU,
		LoginServer: opts.LoginServer,
	}
	if opts.Ephemeral != "" {
		v, err := strconv.ParseBool(opts.Ephemeral)
		if err != nil || strconv.FormatBool(v) != opts.Ephemeral {
			return nil, fmt.Errorf(
				"%s must be true or false, not %q",
				EphemeralOption,
				opts.Ephemeral,
			)
		}
		n.ephemeral = &v
	}
	if err := n.resolveCredential(cfg); err != nil {
		return nil, err
	}
	if err := n.checkEphemeral(); err != nil {
		return nil, err
	}
	return n, nil
}

// UsesClusterCredential reports whether the network's nodes register with the
// cluster credential.
func (n *Network) UsesClusterCredential() bool {
	return n.AuthKey == ""
}

// Ephemeral reports whether the network's nodes are ephemeral: always with the
// cluster credential, which registers ephemeral nodes only, else as the
// tslink.ephemeral option says, else as far as the key says. Unknown counts as
// not ephemeral, so a persistent node is never logged out by guess.
func (n *Network) Ephemeral() bool {
	switch {
	case n.UsesClusterCredential():
		return true
	case n.ephemeral != nil:
		return *n.ephemeral
	default:
		return tailscale.IsEphemeralKey(n.AuthKey)
	}
}

// keyEphemeralParam returns the ephemeral parameter appended to a key, as in
// "tskey-client-...?ephemeral=false", and whether there is one.
func keyEphemeralParam(key string) (bool, bool) {
	_, query, _ := strings.Cut(key, "?")
	params, err := url.ParseQuery(query)
	if err != nil || !params.Has("ephemeral") {
		return false, false
	}
	return params.Get("ephemeral") == "true", true
}

// Credential returns the key a node registers with. The cluster credential is
// read on every call, so replacing the file takes effect at the next
// registration. An OAuth client secret without an ephemeral parameter gets
// one from the tslink.ephemeral option, so its nodes are what the option says.
func (n *Network) Credential(dataDir string) (string, error) {
	if !n.UsesClusterCredential() {
		key := n.AuthKey
		if _, ok := keyEphemeralParam(
			key,
		); n.ephemeral != nil && !ok &&
			strings.HasPrefix(key, "tskey-client-") {
			sep := "?"
			if strings.Contains(key, "?") {
				sep = "&"
			}
			key += sep + "ephemeral=" + strconv.FormatBool(*n.ephemeral)
		}
		return key, nil
	}
	path := filepath.Join(dataDir, ClusterCredentialFile)
	b, err := os.ReadFile(path) // #nosec G304 -- fixed name in the plugin's data directory
	if err != nil {
		return "", fmt.Errorf("failed to read the cluster credential: %w", err)
	}
	secret := strings.TrimSpace(string(b))
	if !strings.HasPrefix(secret, "tskey-client-") || strings.Contains(secret, "?") {
		return "", fmt.Errorf(
			"%s must hold an OAuth client secret (tskey-client-...) with nothing appended",
			path,
		)
	}
	return secret + "?ephemeral=true&preauthorized=true", nil
}

func (n *Network) resolveCredential(cfg *Config) error {
	if n.AuthKey != "" {
		return nil
	}

	_, err := os.Stat(filepath.Join(cfg.DataDir, ClusterCredentialFile))
	switch {
	case err == nil:
		// Tags are checked against the stack when a task starts, since Docker
		// does not pass the network's labels here
		if len(n.Tags) == 0 {
			return errors.New("tslink.tags is required with the cluster credential")
		}
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("failed to check the cluster credential: %w", err)
	}

	n.AuthKey = cfg.AuthKey
	if n.AuthKey == "" {
		return fmt.Errorf(
			"no Tailscale credential: provide %s, set TS_AUTHKEY, or use --opt tslink.authkey=xxx",
			ClusterCredentialFile,
		)
	}
	return nil
}

// checkEphemeral refuses a tslink.ephemeral option the credential contradicts:
// the cluster credential registers ephemeral nodes only, and an OAuth client
// secret may say otherwise in its own ephemeral parameter. A parameter appended
// to any other key is not read by tailscale up, so it contradicts nothing.
func (n *Network) checkEphemeral() error {
	if n.ephemeral == nil {
		return nil
	}
	if n.UsesClusterCredential() {
		if !*n.ephemeral {
			return fmt.Errorf(
				"%s=false is not possible with the cluster credential, which registers ephemeral "+
					"nodes only: use tslink.authkey for persistent nodes",
				EphemeralOption,
			)
		}
		return nil
	}
	if !strings.HasPrefix(n.AuthKey, "tskey-client-") {
		return nil // only OAuth client secrets take parameters
	}
	if v, ok := keyEphemeralParam(n.AuthKey); ok && v != *n.ephemeral {
		return fmt.Errorf("%s=%t contradicts ephemeral=%t in tslink.authkey: remove one of them",
			EphemeralOption, *n.ephemeral, v)
	}
	return nil
}

// CheckTagScope reports whether a stack may register nodes with tags using the
// cluster credential: the cluster client owns every stack's tags, so each
// stack is confined to tag:<stack> and tag:<stack>-*.
func CheckTagScope(stack string, tags []string) error {
	if stack == "" {
		return errors.New(
			"the cluster credential is only available to networks of a stack: use tslink.authkey",
		)
	}
	prefix := "tag:" + stack
	for _, tag := range tags {
		if tag != prefix && !strings.HasPrefix(tag, prefix+"-") {
			return fmt.Errorf(
				"tag %q is outside stack %q's scope (%s or %s-*): use tslink.authkey for other tags",
				tag,
				stack,
				prefix,
				prefix,
			)
		}
	}
	return nil
}
