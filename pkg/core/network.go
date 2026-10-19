package core

import (
	"errors"
	"fmt"
	"io/fs"
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

	LoginServer string // the plugin's control server URL; empty for Tailscale's

	ephemeral *bool // tslink.ephemeral; nil if unset

	// refused says why no endpoint may join the network; nil if one may
	refused error

	StrictTagScope bool // confine a cluster-credential stack to tag:<stack> exactly
}

// errLoginServerOption refuses a network with tslink.loginserver.
var errLoginServerOption = fmt.Errorf(
	"network option %s is no longer supported: the control server is the plugin setting %s; "+
		"recreate the network without it",
	LoginServerOption, LoginServerSetting,
)

// NewNetwork builds a network from its options and resolves its credential:
// its tslink.authkey option, else the cluster credential, else the plugin's
// TS_AUTHKEY. Its nodes use the plugin's control server. A network with
// tslink.loginserver is built, so one Docker kept from before is known, and
// refused (see Refused).
func NewNetwork(id string, opts NetworkOptions, cfg *Config) (*Network, error) {
	n := &Network{
		ID:             id,
		AuthKey:        opts.AuthKey,
		Tags:           opts.Tags,
		MTU:            opts.MTU,
		LoginServer:    cfg.LoginServer,
		StrictTagScope: cfg.StrictTagScope,
	}
	if opts.loginServerOption {
		n.refused = errLoginServerOption
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
	source := AuthKeyOption
	if opts.AuthKey == "" {
		source = "TS_AUTHKEY"
	}
	if err := n.checkKeyParams(source); err != nil {
		return nil, err
	}
	if err := n.checkEphemeral(); err != nil {
		return nil, err
	}
	return n, nil
}

// Refused returns why the network may not be created or joined, nil if it
// may: a network with tslink.loginserver names a control server of its own,
// to which its nodes would present their keys.
func (n *Network) Refused() error {
	return n.refused
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
		// NewNetwork refused a secret whose attributes cannot be parsed
		ephemeral, _, err := tailscale.KeyEphemeral(n.AuthKey)
		return ephemeral && err == nil
	}
}

// keyEphemeralParam returns the ephemeral attribute appended to an OAuth
// client secret, as in "tskey-client-...?ephemeral=false", and whether there
// is one. Other keys take no attributes, so they never have one. NewNetwork
// refuses a secret whose attributes cannot be parsed.
func keyEphemeralParam(key string) (bool, bool) {
	ephemeral, set, err := tailscale.KeyEphemeral(key)
	if !set || err != nil {
		return false, false
	}
	return ephemeral, true
}

// Credential returns the key a node registers with. The cluster credential is
// read on every call, so replacing the file takes effect at the next
// registration. An OAuth client secret without an ephemeral parameter gets
// one from the tslink.ephemeral option, so its nodes are what the option says.
func (n *Network) Credential(dataDir string) (string, error) {
	if !n.UsesClusterCredential() {
		key := n.AuthKey
		if _, ok := keyEphemeralParam(key); n.ephemeral != nil && !ok &&
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

// checkKeyParams refuses parameters appended to a key that is not an OAuth
// client secret: tailscale up parses them for OAuth client secrets only and
// passes any other key to control unchanged, where it is invalid. The error
// names where the key came from, never the key.
func (n *Network) checkKeyParams(source string) error {
	if strings.HasPrefix(n.AuthKey, "tskey-client-") {
		// The error quotes nothing of the secret
		if _, _, err := tailscale.KeyEphemeral(n.AuthKey); err != nil {
			return fmt.Errorf("%s: %w", source, err)
		}
		return nil
	}
	if n.UsesClusterCredential() || !strings.Contains(n.AuthKey, "?") {
		return nil
	}
	return fmt.Errorf(
		"%s has parameters appended (?...), which only OAuth client secrets "+
			"(tskey-client-...) take: remove them",
		source,
	)
}

// checkEphemeral refuses a tslink.ephemeral option the credential contradicts:
// the cluster credential registers ephemeral nodes only, and an OAuth client
// secret may say otherwise in its own ephemeral parameter.
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
	if v, ok := keyEphemeralParam(n.AuthKey); ok && v != *n.ephemeral {
		return fmt.Errorf("%s=%t contradicts ephemeral=%t in tslink.authkey: remove one of them",
			EphemeralOption, *n.ephemeral, v)
	}
	return nil
}

// CheckTagScope reports whether a stack may register nodes with tags using the
// cluster credential: the cluster client owns every stack's tags, so each
// stack is confined to tag:<stack>, or with strict false (TSLINK_TAG_SCOPE=
// prefix) also tag:<stack>-*.
func CheckTagScope(stack string, tags []string, strict bool) error {
	if stack == "" {
		return errors.New(
			"the cluster credential is only available to networks of a stack: use tslink.authkey",
		)
	}
	exact := "tag:" + stack
	for _, tag := range tags {
		// Exact is the default. The prefix opt-out also allows tag:<stack>-*,
		// which lets stack "a" claim "tag:a-b", stack "a-b"'s base tag.
		if tag == exact {
			continue
		}
		if !strict && strings.HasPrefix(tag, exact+"-") {
			continue
		}
		scope := exact
		if !strict {
			scope = exact + " or " + exact + "-*"
		}
		return fmt.Errorf(
			"tag %q is outside stack %q's scope (%s): use tslink.authkey for other tags",
			tag, stack, scope,
		)
	}
	return nil
}
