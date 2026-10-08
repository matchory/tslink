package preflight

import (
	"context"
	"errors"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	dockernetwork "github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/plugin"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/api/types/volume"
	dockerclient "github.com/moby/moby/client"
)

// fakeDocker answers the preflight's Docker queries from its fields.
type fakeDocker struct {
	plugins    []plugin.Plugin
	containers []container.InspectResponse
	networks   map[string]string        // network ID -> driver
	volumes    map[string]volume.Volume // volume name -> volume
	stopped    map[string]bool          // container ID -> not currently running
	pluginErr  error
	gone       map[string]bool // containers and networks removed after the listing
	rootDir    string          // Docker's data root
	infoErr    error
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		plugins: []plugin.Plugin{{
			Name: "tslink:latest", Enabled: true,
			Config: plugin.Config{Entrypoint: []string{"/tslink"}},
		}},
		networks: map[string]string{"tn": "tslink:latest", "bridge": "bridge"},
		volumes:  map[string]volume.Volume{},
		stopped:  map[string]bool{},
		gone:     map[string]bool{},
		rootDir:  "/var/lib/docker",
	}
}

func (f *fakeDocker) Info(
	context.Context, dockerclient.InfoOptions,
) (dockerclient.SystemInfoResult, error) {
	if f.infoErr != nil {
		return dockerclient.SystemInfoResult{}, f.infoErr
	}
	return dockerclient.SystemInfoResult{Info: system.Info{DockerRootDir: f.rootDir}}, nil
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
	_ context.Context, opts dockerclient.ContainerListOptions,
) (dockerclient.ContainerListResult, error) {
	var res dockerclient.ContainerListResult
	for _, c := range f.containers {
		if f.stopped[c.ID] && !opts.All {
			continue
		}
		summary := container.Summary{
			ID:    c.ID,
			Names: []string{c.Name},
			NetworkSettings: &container.NetworkSettingsSummary{
				Networks: c.NetworkSettings.Networks,
			},
		}
		if c.HostConfig != nil {
			summary.HostConfig.NetworkMode = string(c.HostConfig.NetworkMode)
		}
		res.Items = append(res.Items, summary)
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

func (f *fakeDocker) VolumeInspect(
	_ context.Context, name string, _ dockerclient.VolumeInspectOptions,
) (dockerclient.VolumeInspectResult, error) {
	v, ok := f.volumes[name]
	if !ok {
		return dockerclient.VolumeInspectResult{}, cerrdefs.ErrNotFound.WithMessage(
			"no such volume",
		)
	}
	return dockerclient.VolumeInspectResult{Volume: v}, nil
}

// add adds a container on the networks with the given IDs.
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
