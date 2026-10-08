package preflight

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	dockerclient "github.com/moby/moby/client"
)

// protectedPaths are host paths no container may bind-mount, nor a directory
// containing them: Docker's API, network namespaces, Docker's and tslink's state.
var protectedPaths = []string{
	"/var/run/docker.sock", "/run/docker.sock", "/run/netns", "/var/run/netns",
	"/var/lib/docker", "/var/lib/docker-plugins/tailscale",
}

// protectedTrees are host directories no container may bind-mount anything of.
var protectedTrees = []string{
	"/run/docker", "/var/run/docker", "/run/netns", "/var/run/netns",
	"/var/lib/docker", "/var/lib/docker-plugins",
}

// checkContainers checks E1, E2 and E4 on the running containers attached to
// a tslink network.
func checkContainers(ctx context.Context, d DockerAPI) []Result {
	containers, err := tslinkContainers(ctx, d)
	if err != nil {
		var results []Result
		for _, p := range []string{"E1", "E2", "E4"} {
			results = append(results, Result{Property: p, Status: Error, Detail: err.Error()})
		}
		return results
	}
	return []Result{
		checkPrivileges(containers),
		checkMounts(containers),
		checkStackLabels(containers),
	}
}

// tslinkContainers returns the running containers attached to a network of a
// tslink plugin. Containers and networks removed meanwhile are left out.
func tslinkContainers(ctx context.Context, d DockerAPI) ([]container.InspectResponse, error) {
	plugins, err := tslinkPlugins(ctx, d)
	if err != nil {
		return nil, err
	}
	own := make(map[string]bool)
	for _, p := range plugins {
		own[p.Name] = true
	}
	list, err := d.ContainerList(ctx, dockerclient.ContainerListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}
	drivers := make(map[string]string)
	var out []container.InspectResponse
	for _, c := range list.Items {
		attached, err := onTslink(ctx, d, c, own, drivers)
		if err != nil {
			return nil, err
		}
		if !attached {
			continue
		}
		res, err := d.ContainerInspect(ctx, c.ID, dockerclient.ContainerInspectOptions{})
		if cerrdefs.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("failed to inspect container %s: %w", c.ID, err)
		}
		out = append(out, res.Container)
	}
	return out, nil
}

// onTslink reports whether c is attached to a network whose driver is in own.
// drivers caches the driver of each network ID.
func onTslink(
	ctx context.Context,
	d DockerAPI,
	c container.Summary,
	own map[string]bool,
	drivers map[string]string,
) (bool, error) {
	if c.NetworkSettings == nil {
		return false, nil
	}
	attached := false
	for _, ep := range c.NetworkSettings.Networks {
		if ep == nil {
			continue
		}
		driver, ok := drivers[ep.NetworkID]
		if !ok {
			res, err := d.NetworkInspect(ctx, ep.NetworkID, dockerclient.NetworkInspectOptions{})
			switch {
			case cerrdefs.IsNotFound(err):
			case err != nil:
				return false, fmt.Errorf("failed to inspect network %s: %w", ep.NetworkID, err)
			default:
				driver = res.Network.Driver
			}
			drivers[ep.NetworkID] = driver
		}
		attached = attached || own[driver]
	}
	return attached, nil
}

func containerName(c container.InspectResponse) string {
	return strings.TrimPrefix(c.Name, "/")
}

// checkPrivileges checks E1: no container is privileged or has NET_ADMIN or
// SYS_ADMIN.
func checkPrivileges(containers []container.InspectResponse) Result {
	var violations []string
	for _, c := range containers {
		hc := c.HostConfig
		if hc == nil {
			continue
		}
		if hc.Privileged {
			violations = append(violations, containerName(c)+": privileged")
		}
		for _, capability := range hc.CapAdd {
			switch strings.TrimPrefix(strings.ToUpper(capability), "CAP_") {
			case "NET_ADMIN", "SYS_ADMIN", "ALL":
				violations = append(violations, containerName(c)+": cap_add "+capability)
			}
		}
	}
	return outcome("E1", violations,
		"no container on a tslink network is privileged or has NET_ADMIN or SYS_ADMIN")
}

// within reports whether path is dir or lies in it.
func within(path, dir string) bool {
	path, dir = filepath.Clean(path), filepath.Clean(dir)
	return path == dir || dir == "/" || strings.HasPrefix(path, dir+"/")
}

// exposes reports whether bind-mounting source exposes protected host state.
func exposes(source string) bool {
	for _, p := range protectedPaths {
		if within(p, source) {
			return true
		}
	}
	for _, tree := range protectedTrees {
		if within(source, tree) {
			return true
		}
	}
	return false
}

// checkMounts checks E2: no container bind-mounts protected host state or
// shares the host's PID namespace.
func checkMounts(containers []container.InspectResponse) Result {
	var violations []string
	for _, c := range containers {
		for _, m := range c.Mounts {
			if string(m.Type) == "bind" && exposes(m.Source) {
				violations = append(
					violations,
					fmt.Sprintf("%s: mounts %s", containerName(c), m.Source),
				)
			}
		}
		if c.HostConfig != nil && string(c.HostConfig.PidMode) == "host" {
			violations = append(violations, containerName(c)+": shares the host's PID namespace")
		}
	}
	return outcome("E2", violations,
		"no container on a tslink network mounts protected host paths or shares the host's PIDs")
}

// checkStackLabels checks what it can of E4: Swarm tasks without a stack
// label were not deployed with docker stack deploy, and their stack scope
// rests on labels the service's author chose.
func checkStackLabels(containers []container.InspectResponse) Result {
	var loose []string
	for _, c := range containers {
		if c.Config == nil {
			continue
		}
		labels := c.Config.Labels
		if labels["com.docker.swarm.service.id"] != "" &&
			labels["com.docker.stack.namespace"] == "" {
			loose = append(loose, containerName(c))
		}
	}
	if len(loose) > 0 {
		return Result{Property: "E4", Status: Unknown, Detail: "Swarm tasks outside a stack: " +
			strings.Join(loose, ", ")}
	}
	return Result{
		Property: "E4",
		Status:   OK,
		Detail:   "every Swarm task on a tslink network belongs to a stack",
	}
}
