package main

import (
	"bytes"
	"context"
	"errors"
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
