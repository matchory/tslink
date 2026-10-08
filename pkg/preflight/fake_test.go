package preflight

import (
	"context"
	"errors"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	dockernetwork "github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/plugin"
	dockerclient "github.com/moby/moby/client"
)

// fakeDocker answers the preflight's Docker queries from its fields.
type fakeDocker struct {
	plugins    []plugin.Plugin
	containers []container.InspectResponse
	networks   map[string]string // network ID -> driver
	pluginErr  error
	gone       map[string]bool // containers and networks removed after the listing
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		plugins: []plugin.Plugin{{
			Name: "tslink:latest", Enabled: true,
			Config: plugin.Config{Entrypoint: []string{"/tslink"}},
		}},
		networks: map[string]string{"tn": "tslink:latest", "bridge": "bridge"},
		gone:     map[string]bool{},
	}
}

func (f *fakeDocker) PluginList(
	context.Context, dockerclient.PluginListOptions,
) (dockerclient.PluginListResult, error) {
	if f.pluginErr != nil {
		return dockerclient.PluginListResult{}, f.pluginErr
	}
	return dockerclient.PluginListResult{Items: f.plugins}, nil
}

func (f *fakeDocker) ContainerList(
	context.Context, dockerclient.ContainerListOptions,
) (dockerclient.ContainerListResult, error) {
	var res dockerclient.ContainerListResult
	for _, c := range f.containers {
		res.Items = append(res.Items, container.Summary{
			ID: c.ID,
			NetworkSettings: &container.NetworkSettingsSummary{
				Networks: c.NetworkSettings.Networks,
			},
		})
	}
	return res, nil
}

func (f *fakeDocker) ContainerInspect(
	_ context.Context, id string, _ dockerclient.ContainerInspectOptions,
) (dockerclient.ContainerInspectResult, error) {
	for _, c := range f.containers {
		if c.ID == id && !f.gone[id] {
			return dockerclient.ContainerInspectResult{Container: c}, nil
		}
	}
	return dockerclient.ContainerInspectResult{}, cerrdefs.ErrNotFound.WithMessage(
		"no such container",
	)
}

func (f *fakeDocker) NetworkInspect(
	_ context.Context, id string, _ dockerclient.NetworkInspectOptions,
) (dockerclient.NetworkInspectResult, error) {
	driver, ok := f.networks[id]
	if !ok || f.gone[id] {
		return dockerclient.NetworkInspectResult{}, cerrdefs.ErrNotFound.WithMessage(
			"no such network",
		)
	}
	return dockerclient.NetworkInspectResult{
		Network: dockernetwork.Inspect{Network: dockernetwork.Network{ID: id, Driver: driver}},
	}, nil
}

// add adds a running container on the networks with the given IDs.
func (f *fakeDocker) add(c container.InspectResponse, networks ...string) {
	c.NetworkSettings = &container.NetworkSettings{
		Networks: map[string]*dockernetwork.EndpointSettings{},
	}
	for _, id := range networks {
		c.NetworkSettings.Networks[id] = &dockernetwork.EndpointSettings{NetworkID: id}
	}
	f.containers = append(f.containers, c)
}

var errDockerDown = errors.New("cannot connect to the Docker daemon")
