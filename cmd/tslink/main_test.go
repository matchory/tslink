package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
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

func TestVersionTextNamesTailscale(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{Path: "github.com/matchory/tslink", Version: "v0.3.5"},
		Deps: []*debug.Module{
			{Path: "tailscale.com/client/tailscale/v2", Version: "v2.10.1"},
			{Path: "tailscale.com", Version: "v1.104.1"},
		},
	}
	want := "tslink v0.3.5\ntailscale v1.104.1\n"
	if got := versionText(info); got != want {
		t.Errorf("versionText = %q, want %q", got, want)
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
