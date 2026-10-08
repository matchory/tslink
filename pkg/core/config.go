package core

import (
	"os"
	"strconv"
	"strings"

	"github.com/matchory/tslink/pkg/logger"
	"github.com/matchory/tslink/pkg/tailscale"
)

// Config holds the plugin configuration.
type Config struct {
	AuthKey   string
	DataDir   string
	SharedDir string // Directory shared between hosts, for certificates; empty for none

	// IsolateHostTailnet keeps every container on the host from reaching the
	// tailnet through the host's own tailscaled (see netutil.SetupHostIsolation)
	IsolateHostTailnet bool

	// StrictTagScope confines a cluster-credential stack to tag:<stack>
	// exactly. TSLINK_TAG_SCOPE=prefix turns it off.
	StrictTagScope bool
}

// IsolateHostTailnetSetting is the plugin setting that turns the host's
// tailnet isolation off.
const IsolateHostTailnetSetting = "TSLINK_ISOLATE_HOST_TAILNET"

// TagScopeSetting selects how strictly a stack using the cluster credential is
// confined to its tags: "exact" (the default) allows only tag:<stack>,
// "prefix" also allows tag:<stack>-*, the old behaviour, which lets a stack
// claim a longer-named stack's base tag.
const TagScopeSetting = "TSLINK_TAG_SCOPE"

// tagScopeStrict parses TagScopeSetting. Only "prefix" relaxes the scope; any
// other value, a typo included, keeps the exact match.
func tagScopeStrict(v string) bool {
	switch {
	case strings.EqualFold(v, "prefix"):
		logger.Warnf(
			"%s=prefix: a stack may claim a longer-named stack's base tag",
			TagScopeSetting,
		)
		return false
	case v != "" && !strings.EqualFold(v, "exact"):
		logger.Warnf("%s=%q is neither exact nor prefix: keeping the exact tag scope",
			TagScopeSetting, v)
	}
	return true
}

// isolateHostTailnet parses IsolateHostTailnetSetting. Only "false" turns the
// isolation off: any other value, a typo included, keeps the host's tailnet
// closed to containers.
func isolateHostTailnet(v string) bool {
	switch {
	case strings.EqualFold(v, "false"):
		logger.Warnf("%s=false: containers on this host can reach the tailnet as the host",
			IsolateHostTailnetSetting)
		return false
	case v != "" && !strings.EqualFold(v, "true"):
		logger.Warnf("%s=%q is neither true nor false: keeping the host's tailnet isolated",
			IsolateHostTailnetSetting, v)
	}
	return true
}

// NetworkOptions holds options for network creation.
type NetworkOptions struct {
	AuthKey string
	Tags    []string
	MTU     int // 0 if unset or invalid

	LoginServer string // control server URL; empty for Tailscale's
	Ephemeral   string // tslink.ephemeral as given; NewNetwork validates it
}

// EphemeralOption says whether a network's nodes are ephemeral, for keys
// that do not say so themselves.
const EphemeralOption = "tslink.ephemeral"

// MTUOption is Docker's standard network option for the interface MTU.
const MTUOption = "com.docker.network.driver.mtu"

// ParseMTU returns the MTU an option value sets, or 0 if it is not a valid one.
func ParseMTU(s string) int {
	mtu, err := strconv.Atoi(s)
	if err != nil || mtu < 576 || mtu > 65535 {
		return 0
	}
	return mtu
}

// EndpointOptions holds options for endpoint creation.
type EndpointOptions struct {
	Hostname string
}

// DataDir returns the plugin's data directory: TS_DATA_DIR, or /data.
func DataDir() string {
	if dir := os.Getenv("TS_DATA_DIR"); dir != "" {
		return dir
	}
	return "/data"
}

// LoadConfig loads configuration from environment variables.
func LoadConfig() (*Config, error) {
	cfg := &Config{
		AuthKey:   os.Getenv("TS_AUTHKEY"),
		DataDir:   DataDir(),
		SharedDir: os.Getenv("TS_SHARED_DIR"),

		IsolateHostTailnet: isolateHostTailnet(os.Getenv(IsolateHostTailnetSetting)),
		StrictTagScope:     tagScopeStrict(os.Getenv(TagScopeSetting)),
	}

	// tslink no longer downloads Tailscale or runs other binaries: an
	// installation upgraded with these settings keeps working, on the bundled
	// version.
	if v := os.Getenv("TS_VERSION"); v != "" && !strings.EqualFold(v, tailscale.BundledVersion) {
		logger.Warnf(
			"Ignoring TS_VERSION=%s: tslink runs the Tailscale bundled in the plugin image",
			v,
		)
	}
	if p := os.Getenv("TS_PATH"); p != "" {
		logger.Warnf(
			"Ignoring TS_PATH=%s: tslink runs the Tailscale bundled in the plugin image",
			p,
		)
	}

	return cfg, nil
}

// GenericOptionsKey is the key Docker uses to pass driver options.
const GenericOptionsKey = "com.docker.network.generic"

// ParseNetworkOptions parses network creation options.
// Docker passes --opt values nested under "com.docker.network.generic".
func ParseNetworkOptions(opts map[string]any) NetworkOptions {
	options := NetworkOptions{}

	genericOpts := opts
	if nested, ok := opts[GenericOptionsKey]; ok {
		if nestedMap, ok := nested.(map[string]any); ok {
			genericOpts = nestedMap
		}
	}

	if v, ok := genericOpts["tslink.authkey"]; ok {
		if s, ok := v.(string); ok {
			options.AuthKey = s
		}
	}

	if v, ok := genericOpts[MTUOption]; ok {
		if s, ok := v.(string); ok {
			options.MTU = ParseMTU(s)
		}
	}

	if v, ok := genericOpts[LoginServerOption]; ok {
		if s, ok := v.(string); ok {
			options.LoginServer = s
		}
	}

	if v, ok := genericOpts[EphemeralOption]; ok {
		if s, ok := v.(string); ok {
			options.Ephemeral = s
		}
	}

	if v, ok := genericOpts["tslink.tags"]; ok {
		if s, ok := v.(string); ok {
			options.Tags = ParseTags(s)
		}
	}

	return options
}

// ParseTags splits a comma-separated tag list (e.g. "tag:web, tag:prod").
func ParseTags(s string) []string {
	var tags []string
	for t := range strings.SplitSeq(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			tags = append(tags, t)
		}
	}
	return tags
}

// ParseEndpointOptions parses endpoint creation options.
func ParseEndpointOptions(opts map[string]any) EndpointOptions {
	options := EndpointOptions{}

	if v, ok := opts["tslink.hostname"]; ok {
		if s, ok := v.(string); ok {
			options.Hostname = s
		}
	}

	return options
}
