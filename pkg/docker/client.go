package docker

import (
	"context"

	dockerclient "github.com/moby/moby/client"
)

// dockerAPI is the part of the Docker client the driver and the event watcher
// use. The moby client satisfies it; tests replace it with a fake.
type dockerAPI interface {
	PluginList(
		ctx context.Context,
		options dockerclient.PluginListOptions,
	) (dockerclient.PluginListResult, error)
	ContainerList(
		ctx context.Context,
		options dockerclient.ContainerListOptions,
	) (dockerclient.ContainerListResult, error)
	ContainerInspect(
		ctx context.Context,
		containerID string,
		options dockerclient.ContainerInspectOptions,
	) (dockerclient.ContainerInspectResult, error)
	NetworkInspect(
		ctx context.Context,
		networkID string,
		options dockerclient.NetworkInspectOptions,
	) (dockerclient.NetworkInspectResult, error)
	NetworkList(
		ctx context.Context,
		options dockerclient.NetworkListOptions,
	) (dockerclient.NetworkListResult, error)
	Events(ctx context.Context, options dockerclient.EventsListOptions) dockerclient.EventsResult
	Close() error
}

var _ dockerAPI = (*dockerclient.Client)(nil)
