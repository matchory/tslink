package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	dockerclient "github.com/moby/moby/client"

	"github.com/matchory/tslink/pkg/preflight"
)

// downDocker is a Docker API that cannot be reached.
type downDocker struct{ preflight.DockerAPI }

func (downDocker) PluginList(
	context.Context, dockerclient.PluginListOptions,
) (dockerclient.PluginListResult, error) {
	return dockerclient.PluginListResult{}, errors.New("cannot connect to the Docker daemon")
}

func TestPreflightFailsWithoutDocker(t *testing.T) {
	var out bytes.Buffer
	env := preflight.Env{
		Docker:  downDocker{},
		Run:     func(context.Context, string, ...string) ([]byte, error) { return nil, nil },
		DataDir: t.TempDir(),
	}
	if code := runPreflight(context.Background(), env, &out); code != 1 {
		t.Errorf("runPreflight = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "E1  error") {
		t.Errorf("output %q does not report E1 as an error", out.String())
	}
}

func TestVersionText(t *testing.T) {
	tailscale := []*debug.Module{
		{Path: "tailscale.com/client/tailscale/v2", Version: "v2.10.1"},
		{Path: "tailscale.com", Version: "v1.104.1"},
	}
	mod := func(v string) debug.Module {
		return debug.Module{Path: "github.com/matchory/tslink", Version: v}
	}
	tests := []struct {
		name    string
		stamped string
		info    *debug.BuildInfo
		want    string
	}{
		{
			name: "VCS stamp",
			info: &debug.BuildInfo{Main: mod("v0.3.5"), Deps: tailscale},
			want: "tslink v0.3.5\ntailscale v1.104.1\n",
		},
		{
			name:    "build stamp wins",
			stamped: "v0.3.6-2-gabcdef0",
			info:    &debug.BuildInfo{Main: mod("(devel)"), Deps: tailscale},
			want:    "tslink v0.3.6-2-gabcdef0\ntailscale v1.104.1\n",
		},
		{
			name:    "build stamp without build info",
			stamped: "v0.3.6",
			want:    "tslink v0.3.6\n",
		},
		{name: "neither", want: "tslink (devel)\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := versionText(tt.stamped, tt.info); got != tt.want {
				t.Errorf("versionText = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestBuildStampsVersion builds the binary as the Dockerfile does, with the
// version in main.version, and runs tslink version: a renamed variable would
// leave every release reporting the VCS stamp.
func TestBuildStampsVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := filepath.Join(t.TempDir(), "tslink")
	build := exec.CommandContext(t.Context(), "go", "build",
		"-ldflags", "-X main.version=vtest", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	out, err := exec.CommandContext(t.Context(), bin, "version").Output()
	if err != nil {
		t.Fatalf("tslink version: %v", err)
	}
	if first, _, _ := strings.Cut(string(out), "\n"); first != "tslink vtest" {
		t.Errorf("tslink version printed %q first, want %q", first, "tslink vtest")
	}
}

// TestUnknownCommandDoesNotStartThePlugin runs the binary with arguments it
// does not know: it must exit 2 without starting a plugin instance.
func TestUnknownCommandDoesNotStartThePlugin(t *testing.T) {
	if arg, ok := os.LookupEnv("TSLINK_TEST_ARG"); ok {
		os.Args = []string{"tslink", arg}
		main()
		return
	}
	for _, arg := range []string{"--bogus", "start"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestUnknownCommandDoesNotStartThePlugin$")
		cmd.Env = append(os.Environ(), "TSLINK_TEST_ARG="+arg)
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 {
			t.Errorf("tslink %s: %v, want exit status 2; output:\n%s", arg, err, out)
		}
		if strings.Contains(string(out), "Starting") {
			t.Errorf("tslink %s started the plugin:\n%s", arg, out)
		}
	}
}
