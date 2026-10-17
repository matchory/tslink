package preflight

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	dockerclient "github.com/moby/moby/client"

	"github.com/matchory/tslink/pkg/core"
)

// protectedPaths are host paths no container may bind-mount, nor a directory
// containing them: Docker's API, network namespaces, Docker's and tslink's
// state. Docker's data root, if it is not /var/lib/docker, is one of them too.
var protectedPaths = []string{
	"/var/run/docker.sock", "/run/docker.sock", "/run/netns", "/var/run/netns",
	"/var/lib/docker", "/var/lib/docker-plugins/tailscale",
}

// protectedTrees are host directories no container may bind-mount anything
// of. Docker's data root, if it is not /var/lib/docker, is one of them too.
var protectedTrees = []string{
	"/run/docker", "/var/run/docker", "/run/netns", "/var/run/netns",
	"/var/lib/docker", "/var/lib/docker-plugins",
}

// protected holds the host paths E2 protects on one host.
type protected struct {
	paths, trees []string
}

// add returns p with path protected too, as both a path and a tree: nothing
// bind-mounted under it, or over it, is allowed. A blank path, or one already
// protected, leaves p unchanged.
func (p protected) add(path string) protected {
	if path == "" {
		return p
	}
	path = filepath.Clean(path)
	if slices.Contains(p.paths, path) {
		return p
	}
	p.paths = append(slices.Clone(p.paths), path)
	p.trees = append(slices.Clone(p.trees), path)
	return p
}

// protectedOn returns the protected host paths of a Docker daemon whose data
// root is rootDir.
func protectedOn(rootDir string) protected {
	return protected{paths: protectedPaths, trees: protectedTrees}.add(rootDir)
}

// stackLabel is the label docker stack deploy sets to the stack's name.
const stackLabel = "com.docker.stack.namespace"

// checkContainers checks E4 on the containers attached, directly or through
// another container's network namespace, to a network of a tslink plugin,
// and E1 and E2 on them and the other containers of their stacks.
func checkContainers(ctx context.Context, d DockerAPI) []Result {
	containers, scope, err := tslinkContainers(ctx, d)
	if err != nil {
		var results []Result
		for _, p := range []string{"E1", "E2", "E4"} {
			results = append(results, Result{Property: p, Status: Error, Detail: err.Error()})
		}
		return results
	}
	if containers == nil {
		return []Result{noPlugin("E1"), noPlugin("E2"), noPlugin("E4")}
	}
	mounts, err := checkMounts(ctx, d, scope)
	if err != nil {
		mounts = Result{Property: "E2", Status: Error, Detail: err.Error()}
	}
	return []Result{checkPrivileges(scope), mounts, checkStackLabels(containers)}
}

// tslinkContainers returns the containers attached, directly or through
// another container's network namespace, to a network of a tslink plugin,
// and, in scope, those and the other containers of their stacks, in any
// state (including stopped and created-but-never-started): nil if no tslink
// plugin is enabled, and empty but not nil if none is attached. Containers
// and networks removed meanwhile are left out.
func tslinkContainers(
	ctx context.Context, d DockerAPI,
) ([]container.InspectResponse, []container.InspectResponse, error) {
	plugins, err := tslinkPlugins(ctx, d)
	if err != nil || len(plugins) == 0 {
		return nil, nil, err
	}
	own := make(map[string]bool)
	for _, p := range plugins {
		own[p.Name] = true
	}
	list, err := d.ContainerList(ctx, dockerclient.ContainerListOptions{All: true})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list containers: %w", err)
	}
	attached, err := attachedIDs(ctx, d, list.Items, own)
	if err != nil {
		return nil, nil, err
	}
	inScope := stackScope(list.Items, attached)
	tslink, scope := []container.InspectResponse{}, []container.InspectResponse{}
	for _, c := range list.Items {
		if !inScope[c.ID] {
			continue
		}
		res, err := d.ContainerInspect(ctx, c.ID, dockerclient.ContainerInspectOptions{})
		if cerrdefs.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("failed to inspect container %s: %w", c.ID, err)
		}
		scope = append(scope, res.Container)
		if attached[c.ID] {
			tslink = append(tslink, res.Container)
		}
	}
	return tslink, scope, nil
}

// stackScope returns, for every container in items, whether E1 and E2 apply
// to it: whether it is attached, or of the same stack as a container that is.
// The author of a stack chose all its services, not only those on tslink
// networks.
func stackScope(items []container.Summary, attached map[string]bool) map[string]bool {
	stacks := make(map[string]bool)
	for _, c := range items {
		if ns := c.Labels[stackLabel]; attached[c.ID] && ns != "" {
			stacks[ns] = true
		}
	}
	scope := make(map[string]bool, len(items))
	for _, c := range items {
		scope[c.ID] = attached[c.ID] || stacks[c.Labels[stackLabel]]
	}
	return scope
}

// attachedIDs returns, for every container in items, whether it is attached
// to a network whose driver is in own: directly, or by sharing the network
// namespace of a container that is (as --network container:<ref> does).
func attachedIDs(
	ctx context.Context, d DockerAPI, items []container.Summary, own map[string]bool,
) (map[string]bool, error) {
	drivers := make(map[string]string)
	attached := make(map[string]bool, len(items))
	for _, c := range items {
		onNet, err := onTslinkNetwork(ctx, d, c, own, drivers)
		if err != nil {
			return nil, err
		}
		attached[c.ID] = onNet
	}
	byName := make(map[string]string, len(items)) // container name (no slash) -> ID
	for _, c := range items {
		for _, name := range c.Names {
			byName[core.ContainerName(name)] = c.ID
		}
	}
	// Propagate through network_mode: container:<ref>, to a fixed point: a
	// chain of such references, however unusual, resolves fully.
	for range items {
		changed := false
		for _, c := range items {
			if attached[c.ID] {
				continue
			}
			mode := container.NetworkMode(c.HostConfig.NetworkMode)
			if !mode.IsContainer() {
				continue
			}
			targetID := mode.ConnectedContainer()
			if id, ok := byName[targetID]; ok {
				targetID = id
			}
			if attached[targetID] {
				attached[c.ID] = true
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	return attached, nil
}

// onTslinkNetwork reports whether c is directly attached to a network whose
// driver is in own. drivers caches the driver of each network ID. A
// container created but never started reports no network ID of its own, so
// the network's name (the map key) identifies it instead.
func onTslinkNetwork(
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
	for name, ep := range c.NetworkSettings.Networks {
		if ep == nil {
			continue
		}
		id := ep.NetworkID
		if id == "" {
			id = name
		}
		driver, ok := drivers[id]
		if !ok {
			res, err := d.NetworkInspect(ctx, id, dockerclient.NetworkInspectOptions{})
			switch {
			case cerrdefs.IsNotFound(err):
			case err != nil:
				return false, fmt.Errorf("failed to inspect network %s: %w", id, err)
			default:
				driver = res.Network.Driver
			}
			drivers[id] = driver
		}
		attached = attached || own[driver]
	}
	return attached, nil
}

// forwardingSysctl reports whether key is a namespaced sysctl that enables IP
// forwarding when set to anything other than "0": net.ipv4.ip_forward, or
// net.ipv4.conf.<any>.forwarding or net.ipv6.conf.<any>.forwarding for any
// interface or "all"/"default".
func forwardingSysctl(key string) bool {
	if key == "net.ipv4.ip_forward" {
		return true
	}
	return (strings.HasPrefix(key, "net.ipv4.conf.") || strings.HasPrefix(key, "net.ipv6.conf.")) &&
		strings.HasSuffix(key, ".forwarding")
}

// unconfinedSecurityOpt reports whether opt turns off seccomp or AppArmor
// confinement: seccomp=unconfined, apparmor=unconfined, or the same with a
// colon, which Docker accepts too.
func unconfinedSecurityOpt(opt string) bool {
	opt = strings.Replace(opt, ":", "=", 1)
	return opt == "seccomp=unconfined" || opt == "apparmor=unconfined"
}

// checkPrivileges checks E1: no container is privileged, has NET_ADMIN or
// SYS_ADMIN, sets a namespaced sysctl that enables forwarding, or disables
// seccomp or AppArmor confinement.
func checkPrivileges(containers []container.InspectResponse) Result {
	var violations []string
	for _, c := range containers {
		hc := c.HostConfig
		if hc == nil {
			continue
		}
		if hc.Privileged {
			violations = append(violations, core.ContainerName(c.Name)+": privileged")
		}
		for _, capability := range hc.CapAdd {
			switch strings.TrimPrefix(strings.ToUpper(capability), "CAP_") {
			case "NET_ADMIN", "SYS_ADMIN", "ALL":
				violations = append(violations, core.ContainerName(c.Name)+": cap_add "+capability)
			}
		}
		for key, value := range hc.Sysctls {
			if forwardingSysctl(key) && value != "0" {
				violations = append(
					violations,
					fmt.Sprintf("%s: sysctl %s=%s", core.ContainerName(c.Name), key, value),
				)
			}
		}
		for _, opt := range hc.SecurityOpt {
			if unconfinedSecurityOpt(opt) {
				violations = append(violations, core.ContainerName(c.Name)+": "+opt)
			}
		}
	}
	return outcome("E1", violations, "no container on a tslink network on this host, nor "+
		"another container of its stack, is privileged, has NET_ADMIN or SYS_ADMIN, enables "+
		"forwarding by sysctl, or disables seccomp or AppArmor")
}

// within reports whether path is dir or lies in it.
func within(path, dir string) bool {
	path, dir = filepath.Clean(path), filepath.Clean(dir)
	return path == dir || dir == "/" || strings.HasPrefix(path, dir+"/")
}

// exposes reports whether bind-mounting source exposes protected host state.
func (p protected) exposes(source string) bool {
	for _, path := range p.paths {
		if within(path, source) {
			return true
		}
	}
	for _, tree := range p.trees {
		if within(source, tree) {
			return true
		}
	}
	return false
}

// sharedDirsOf returns the host paths E2 protects as the shared certificate
// directory: each enabled tslink plugin's own shared.source setting (docker
// plugin set tslink shared.source=...). --shared-dir is not one of them: it
// names the directory as mounted where the preflight runs, for E7.
func sharedDirsOf(ctx context.Context, d DockerAPI) ([]string, error) {
	plugins, err := tslinkPlugins(ctx, d)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, p := range plugins {
		for _, m := range p.Settings.Mounts {
			if m.Name == "shared" && m.Source != nil && *m.Source != "" {
				dirs = append(dirs, *m.Source)
			}
		}
	}
	return dirs, nil
}

// checkMounts checks E2: no container bind-mounts protected host state,
// directly or through a bind-backed local volume, or shares the host's PID
// namespace.
func checkMounts(
	ctx context.Context,
	d DockerAPI,
	containers []container.InspectResponse,
) (Result, error) {
	info, err := d.Info(ctx, dockerclient.InfoOptions{})
	if err != nil {
		return Result{}, fmt.Errorf("failed to read Docker's data root: %w", err)
	}
	prot := protectedOn(info.Info.DockerRootDir)
	sharedDirs, err := sharedDirsOf(ctx, d)
	if err != nil {
		return Result{}, err
	}
	for _, dir := range sharedDirs {
		prot = prot.add(dir)
	}
	var violations []string
	for _, c := range containers {
		for _, m := range c.Mounts {
			switch {
			case string(m.Type) == "bind" && prot.exposes(m.Source):
				violations = append(
					violations,
					fmt.Sprintf("%s: mounts %s", core.ContainerName(c.Name), m.Source),
				)
			case string(m.Type) == "volume" && m.Driver == "local":
				device, err := bindVolumeDevice(ctx, d, m.Name)
				if err != nil {
					return Result{}, err
				}
				if device != "" && prot.exposes(device) {
					violations = append(violations, fmt.Sprintf(
						"%s: volume %s mounts %s", core.ContainerName(c.Name), m.Name, device,
					))
				}
			}
		}
		if c.HostConfig != nil && string(c.HostConfig.PidMode) == "host" {
			violations = append(
				violations,
				core.ContainerName(c.Name)+": shares the host's PID namespace",
			)
		}
	}
	return outcome(
		"E2",
		violations,
		"no container on a tslink network on this host, nor another container of its "+
			"stack, mounts protected host paths or shares the host's PIDs",
	), nil
}

// bindVolumeDevice returns the host path a local-driver volume bind-mounts
// (`docker volume create -o type=none -o o=bind -o device=...`), or "" if it
// is not bind-backed. A volume removed meanwhile is treated the same as one
// that is not bind-backed.
func bindVolumeDevice(ctx context.Context, d DockerAPI, name string) (string, error) {
	res, err := d.VolumeInspect(ctx, name, dockerclient.VolumeInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to inspect volume %s: %w", name, err)
	}
	if res.Volume.Driver != "local" || !strings.Contains(res.Volume.Options["o"], "bind") {
		return "", nil
	}
	return res.Volume.Options["device"], nil
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
			labels[stackLabel] == "" {
			loose = append(loose, core.ContainerName(c.Name))
		}
	}
	if len(loose) > 0 {
		return Result{Property: "E4", Status: Unknown, Detail: "Swarm tasks outside a stack: " +
			strings.Join(loose, ", ")}
	}
	return Result{
		Property: "E4",
		Status:   OK,
		Detail:   "every Swarm task on a tslink network on this host carries a stack label",
	}
}
