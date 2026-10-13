package core

import (
	"bytes"
	"log/slog"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/matchory/tslink/pkg/logger"
	"github.com/matchory/tslink/pkg/netutil"
)

func TestParseNetworkOptionsTags(t *testing.T) {
	opts := mustParseNetworkOptions(t, map[string]any{
		GenericOptionsKey: map[string]any{
			AuthKeyOption: "tskey-client-x",
			TagsOption:    " tag:svc-billing, tag:prod ,",
		},
	})
	if opts.AuthKey != "tskey-client-x" {
		t.Errorf("AuthKey = %q", opts.AuthKey)
	}
	if want := []string{"tag:svc-billing", "tag:prod"}; !slices.Equal(opts.Tags, want) {
		t.Errorf("Tags = %q, want %q", opts.Tags, want)
	}
}

func TestParseNetworkOptionsEphemeral(t *testing.T) {
	opts := mustParseNetworkOptions(
		t,
		map[string]any{GenericOptionsKey: map[string]any{EphemeralOption: "true"}},
	)
	if opts.Ephemeral != "true" {
		t.Errorf("Ephemeral = %q, want true", opts.Ephemeral)
	}
}

func TestParseTagsEmpty(t *testing.T) {
	if tags := ParseTags(""); tags != nil {
		t.Errorf("ParseTags(\"\") = %q, want nil", tags)
	}
}

// An MTU out of range or not a number fails the network.
func TestParseNetworkOptionsMTU(t *testing.T) {
	tests := map[string]int{"1450": 1450, "576": 576, "": 0, "abc": 0, "100": 0, "70000": 0}
	for in, want := range tests {
		opts, err := ParseNetworkOptions(
			map[string]any{GenericOptionsKey: map[string]any{MTUOption: in}},
		)
		if opts.MTU != want || (err != nil) != (want == 0) {
			t.Errorf("MTU %q parsed as %d, error %v; want %d", in, opts.MTU, err, want)
		}
	}
}

// Settings of the removed runtime download do not keep the plugin from
// starting after an upgrade, and are logged as ignored: it runs the bundled
// binaries.
func TestLoadConfigIgnoresDownloadSettings(t *testing.T) {
	var log bytes.Buffer
	logger.SetOutput(&log)
	t.Cleanup(func() { logger.SetOutput(os.Stdout) })
	t.Setenv("TS_VERSION", "1.76.6")
	t.Setenv("TS_PATH", "/opt/tailscale/bin")
	LoadConfig()
	for _, want := range []string{"Ignoring TS_VERSION=1.76.6", "Ignoring TS_PATH=/opt/tailscale/bin"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log does not contain %q:\n%s", want, log.String())
		}
	}
}

func TestLoadConfigDataDir(t *testing.T) {
	t.Setenv("TS_DATA_DIR", "")
	if cfg := LoadConfig(); cfg.DataDir != "/data" {
		t.Errorf("without TS_DATA_DIR: config %+v, want DataDir /data", cfg)
	}
	t.Setenv("TS_DATA_DIR", "/var/lib/tslink")
	if cfg := LoadConfig(); cfg.DataDir != "/var/lib/tslink" {
		t.Errorf("config %+v, want DataDir from TS_DATA_DIR", cfg)
	}
}

func TestLoadConfigIsolateHostTailnet(t *testing.T) {
	// Only an explicit false turns the isolation off: a typo must not open
	// the host's tailnet to containers
	for value, want := range map[string]bool{
		"": true, "true": true, "false": false, "FALSE": false, "0": true, "no": true,
	} {
		t.Setenv(IsolateHostTailnetSetting, value)
		cfg := LoadConfig()
		if cfg.IsolateHostTailnet != want {
			t.Errorf("%s=%q: IsolateHostTailnet = %v, want %v",
				IsolateHostTailnetSetting, value, cfg.IsolateHostTailnet, want)
		}
	}
}

// An invalid range, or one with tailscaled's own port, keeps the default.
func TestLoadConfigWireGuardPorts(t *testing.T) {
	for value, want := range map[string]netutil.PortRange{
		"":            netutil.DefaultPortRange,
		"50000-50999": {First: 50000, Last: 50999},
		"nonsense":    netutil.DefaultPortRange,
		"10-20":       netutil.DefaultPortRange,
		"41000-42000": netutil.DefaultPortRange, // tailscaled's own port
	} {
		t.Setenv(WireGuardPortsSetting, value)
		cfg := LoadConfig()
		if cfg.WireGuardPorts != want {
			t.Errorf("%s=%q: WireGuardPorts = %v, want %v",
				WireGuardPortsSetting, value, cfg.WireGuardPorts, want)
		}
	}
}

func TestLoadConfigHostEndpoints(t *testing.T) {
	two := []netip.Addr{netip.MustParseAddr("10.10.0.2"), netip.MustParseAddr("192.168.1.5")}
	for value, want := range map[string]netutil.HostEndpointsConfig{
		"":                                    {},
		"auto":                                {},
		"AUTO":                                {},
		"off":                                 {Off: true},
		"10.10.0.2, 192.168.1.5":              {Addrs: two},
		"10.10.0.2,fd00::1":                   {}, // IPv6: keeps auto
		"10.0.0.1,10.0.0.2,10.0.0.3,10.0.0.4": {}, // more than 3
		"nonsense":                            {},
		"10.10.0.2,":                          {}, // an empty entry
	} {
		t.Setenv(HostEndpointsSetting, value)
		cfg := LoadConfig()
		if cfg.HostEndpoints.Off != want.Off || !slices.Equal(cfg.HostEndpoints.Addrs, want.Addrs) {
			t.Errorf("%s=%q: HostEndpoints = %+v, want %+v",
				HostEndpointsSetting, value, cfg.HostEndpoints, want)
		}
	}
}

// Guards: G4
func TestTagScopeStrict(t *testing.T) {
	for v, want := range map[string]bool{
		"": true, "exact": true, "EXACT": true, "prefix": false, "PREFIX": false, "nonsense": true,
	} {
		if got := TagScopeStrict(v); got != want {
			t.Errorf("TagScopeStrict(%q) = %v, want %v", v, got, want)
		}
	}
}

// The log level defaults to info; an invalid value keeps it.
func TestLoadConfigLogLevel(t *testing.T) {
	for value, want := range map[string]slog.Level{
		"":        slog.LevelInfo,
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"error":   slog.LevelError,
		"verbose": slog.LevelInfo,
	} {
		t.Setenv(LogLevelSetting, value)
		if got := LoadConfig().LogLevel; got != want {
			t.Errorf("%s=%q: LogLevel = %v, want %v", LogLevelSetting, value, got, want)
		}
		if !logger.Get().Enabled(t.Context(), want) || want > slog.LevelDebug &&
			logger.Get().Enabled(t.Context(), want-1) {
			t.Errorf("%s=%q: the logger does not log from %v on", LogLevelSetting, value, want)
		}
	}
}
