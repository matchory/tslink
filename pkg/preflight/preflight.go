// Package preflight checks, on the host it runs on, the environment
// properties that SECURITY.md's guarantees rely on.
package preflight

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/moby/moby/api/types/plugin"
	dockerclient "github.com/moby/moby/client"
)

// Status is the outcome of checking one property.
type Status string

// The outcomes of a check. Only Violated and Error fail the preflight.
const (
	OK       Status = "ok"
	Violated Status = "violated"
	Unknown  Status = "unknown" // the check cannot tell from this host
	Info     Status = "info"    // an operator's choice, reported
	Error    Status = "error"   // the check could not run
)

// Result is the outcome of checking one environment property.
type Result struct {
	Property string // E1, E2, ...
	Status   Status
	Detail   string
}

// DockerAPI is the part of the Docker client the preflight uses.
type DockerAPI interface {
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
	VolumeInspect(
		ctx context.Context,
		volumeID string,
		options dockerclient.VolumeInspectOptions,
	) (dockerclient.VolumeInspectResult, error)
}

var _ DockerAPI = (*dockerclient.Client)(nil)

// outcome is the result of a check that lists what violates property: OK with
// okDetail if nothing does.
func outcome(property string, violations []string, okDetail string) Result {
	if len(violations) > 0 {
		return Result{Property: property, Status: Violated, Detail: strings.Join(violations, "; ")}
	}
	return Result{Property: property, Status: OK, Detail: okDetail}
}

// tslinkPlugins returns the enabled tslink plugins: those whose entrypoint is
// /tslink, as the driver recognizes itself.
func tslinkPlugins(ctx context.Context, d DockerAPI) ([]plugin.Plugin, error) {
	plugins, err := d.PluginList(ctx, dockerclient.PluginListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list plugins: %w", err)
	}
	var own []plugin.Plugin
	for _, p := range plugins.Items {
		if p.Enabled && slices.Equal(p.Config.Entrypoint, []string{"/tslink"}) {
			own = append(own, p)
		}
	}
	return own, nil
}
