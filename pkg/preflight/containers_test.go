package preflight

import (
	"context"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/volume"
)

func result(t *testing.T, results []Result, property string) Result {
	t.Helper()
	for _, r := range results {
		if r.Property == property {
			return r
		}
	}
	t.Fatalf("no result for %s in %+v", property, results)
	return Result{}
}

func TestCapabilities(t *testing.T) {
	for _, tt := range []struct {
		name   string
		hc     container.HostConfig
		status Status
	}{
		{"default", container.HostConfig{}, OK},
		{"privileged", container.HostConfig{Privileged: true}, Violated},
		{"NET_ADMIN", container.HostConfig{CapAdd: []string{"NET_ADMIN"}}, Violated},
		{"cap_net_admin", container.HostConfig{CapAdd: []string{"cap_net_admin"}}, Violated},
		{"CAP_SYS_ADMIN", container.HostConfig{CapAdd: []string{"CAP_SYS_ADMIN"}}, Violated},
		{"ALL", container.HostConfig{CapAdd: []string{"ALL"}}, Violated},
		{"NET_RAW", container.HostConfig{CapAdd: []string{"NET_RAW"}}, OK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeDocker()
			f.add(container.InspectResponse{ID: "c1", Name: "/web", HostConfig: &tt.hc}, "tn")
			hc := container.HostConfig{Privileged: true}
			f.add(container.InspectResponse{ID: "c2", Name: "/other", HostConfig: &hc}, "bridge")
			r := result(t, checkContainers(context.Background(), f), "E1")
			if r.Status != tt.status {
				t.Errorf("E1 = %+v, want %s", r, tt.status)
			}
			if r.Status == Violated && !strings.Contains(r.Detail, "web") {
				t.Errorf("E1 detail %q does not name the container", r.Detail)
			}
		})
	}
}

func TestMounts(t *testing.T) {
	for _, tt := range []struct {
		source, typ string
		status      Status
	}{
		{"/srv/app", "bind", OK},
		{"/var/lib/docker/volumes/x/_data", "volume", OK},
		{"/var/run/docker.sock", "bind", Violated},
		{"/run", "bind", Violated},
		{"/", "bind", Violated},
		{"/var/", "bind", Violated},
		{"/var/lib/../lib", "bind", Violated},
		{"/var/lib/docker-plugins/tailscale/by-hostname/x", "bind", Violated},
		{"/run/netns", "bind", Violated},
		{"/var/lib/docker/swarm", "bind", Violated},
		{"/run/user/1000", "bind", OK},
	} {
		t.Run(tt.source, func(t *testing.T) {
			f := newFakeDocker()
			f.add(container.InspectResponse{
				ID:         "c1",
				Name:       "/web",
				HostConfig: &container.HostConfig{},
				Mounts: []container.MountPoint{
					{Type: mount.Type(tt.typ), Source: tt.source, Destination: "/x"},
				},
			}, "tn")
			if r := result(
				t,
				checkContainers(context.Background(), f),
				"E2",
			); r.Status != tt.status {
				t.Errorf("E2 = %+v, want %s", r, tt.status)
			}
		})
	}
}

func TestHostPIDNamespace(t *testing.T) {
	f := newFakeDocker()
	f.add(
		container.InspectResponse{
			ID:         "c1",
			Name:       "/web",
			HostConfig: &container.HostConfig{PidMode: "host"},
		},
		"tn",
	)
	if r := result(t, checkContainers(context.Background(), f), "E2"); r.Status != Violated {
		t.Errorf("E2 = %+v, want violated", r)
	}
}

func TestStackLabels(t *testing.T) {
	f := newFakeDocker()
	f.add(container.InspectResponse{
		ID: "c1", Name: "/app_web.1.x", HostConfig: &container.HostConfig{},
		Config: &container.Config{Labels: map[string]string{
			"com.docker.swarm.service.id": "s1", "com.docker.stack.namespace": "app",
		}},
	}, "tn")
	if r := result(t, checkContainers(context.Background(), f), "E4"); r.Status != OK {
		t.Errorf("E4 = %+v, want ok", r)
	}
	f.add(container.InspectResponse{
		ID: "c2", Name: "/loose.1.y", HostConfig: &container.HostConfig{},
		Config: &container.Config{Labels: map[string]string{"com.docker.swarm.service.id": "s2"}},
	}, "tn")
	r := result(t, checkContainers(context.Background(), f), "E4")
	if r.Status != Unknown || !strings.Contains(r.Detail, "loose.1.y") {
		t.Errorf("E4 = %+v, want unknown naming loose.1.y", r)
	}
}

func TestContainersGoneDuringTheCheck(t *testing.T) {
	f := newFakeDocker()
	f.add(
		container.InspectResponse{
			ID:         "c1",
			Name:       "/gone",
			HostConfig: &container.HostConfig{Privileged: true},
		},
		"tn",
	)
	f.add(
		container.InspectResponse{
			ID:         "c2",
			Name:       "/web",
			HostConfig: &container.HostConfig{Privileged: true},
		},
		"tn",
	)
	f.add(
		container.InspectResponse{ID: "c3", Name: "/net-gone", HostConfig: &container.HostConfig{}},
		"gone-net",
	)
	f.networks["gone-net"] = "tslink:latest"
	f.gone["c1"], f.gone["gone-net"] = true, true
	r := result(t, checkContainers(context.Background(), f), "E1")
	if r.Status != Violated || strings.Contains(r.Detail, "gone") ||
		!strings.Contains(r.Detail, "web") {
		t.Errorf("E1 = %+v, want violated by web only", r)
	}
}

func TestDockerUnreachable(t *testing.T) {
	f := newFakeDocker()
	f.pluginErr = errDockerDown
	results := checkContainers(context.Background(), f)
	for _, p := range []string{"E1", "E2", "E4"} {
		if r := result(t, results, p); r.Status != Error {
			t.Errorf("%s = %+v, want error", p, r)
		}
	}
}

// TestNetworkModeContainer covers a container started with
// --network container:<ref> (compose's network_mode: service:x): it has no
// network of its own, so it must be recognized through the container whose
// netns it shares.
func TestNetworkModeContainer(t *testing.T) {
	for _, ref := range []string{"web", "c1"} {
		t.Run(ref, func(t *testing.T) {
			f := newFakeDocker()
			f.add(
				container.InspectResponse{
					ID:         "c1",
					Name:       "/web",
					HostConfig: &container.HostConfig{},
				},
				"tn",
			)
			f.add(container.InspectResponse{
				ID: "c2", Name: "/sidecar",
				HostConfig: &container.HostConfig{
					NetworkMode: container.NetworkMode("container:" + ref),
					Privileged:  true,
				},
			})
			r := result(t, checkContainers(context.Background(), f), "E1")
			if r.Status != Violated || !strings.Contains(r.Detail, "sidecar") {
				t.Errorf("E1 = %+v, want violated by sidecar, which shares web's netns", r)
			}
		})
	}
}

// TestBindBackedVolume covers a named local-driver volume that itself
// bind-mounts a host path (`docker volume create -o type=none -o o=bind -o
// device=...`): its container.MountPoint reports Type "volume", so E2 must
// look at the volume's own options to see the host path it exposes.
func TestBindBackedVolume(t *testing.T) {
	for _, tt := range []struct {
		name   string
		vol    volume.Volume
		absent bool
		status Status
	}{
		{name: "plain", vol: volume.Volume{Driver: "local"}, status: OK},
		{
			name: "protected", status: Violated,
			vol: volume.Volume{
				Driver:  "local",
				Options: map[string]string{"o": "bind", "device": "/var/run/docker.sock"},
			},
		},
		{
			name: "other", status: OK,
			vol: volume.Volume{
				Driver:  "local",
				Options: map[string]string{"o": "bind", "device": "/srv/app"},
			},
		},
		{
			name: "nfs", status: OK,
			vol: volume.Volume{Driver: "nfs", Options: map[string]string{"device": "/var/lib/docker"}},
		},
		{name: "gone", absent: true, status: OK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeDocker()
			if !tt.absent {
				f.volumes[tt.name] = tt.vol
			}
			f.add(container.InspectResponse{
				ID: "c1", Name: "/web", HostConfig: &container.HostConfig{},
				Mounts: []container.MountPoint{
					{Type: mount.TypeVolume, Name: tt.name, Driver: "local", Destination: "/x"},
				},
			}, "tn")
			if r := result(
				t,
				checkContainers(context.Background(), f),
				"E2",
			); r.Status != tt.status {
				t.Errorf("E2 = %+v, want %s", r, tt.status)
			}
		})
	}
}

// TestStoppedContainers covers a stopped (not just running) container
// attached to a tslink network: Docker's default container list omits it.
func TestStoppedContainers(t *testing.T) {
	f := newFakeDocker()
	f.add(
		container.InspectResponse{
			ID:         "c1",
			Name:       "/stopped",
			HostConfig: &container.HostConfig{Privileged: true},
		},
		"tn",
	)
	f.stopped["c1"] = true
	r := result(t, checkContainers(context.Background(), f), "E1")
	if r.Status != Violated || !strings.Contains(r.Detail, "stopped") {
		t.Errorf("E1 = %+v, want violated by a stopped container", r)
	}
}

// TestCreatedButNeverStarted covers a container created but never started:
// Docker reports no NetworkID for its networks, only their name (the map
// key the summary carries them under).
func TestCreatedButNeverStarted(t *testing.T) {
	f := newFakeDocker()
	f.add(
		container.InspectResponse{
			ID: "c1", Name: "/never-started", HostConfig: &container.HostConfig{Privileged: true},
		},
		"tn",
	)
	f.stopped["c1"] = true
	f.containers[0].NetworkSettings.Networks["tn"].NetworkID = ""
	r := result(t, checkContainers(context.Background(), f), "E1")
	if r.Status != Violated || !strings.Contains(r.Detail, "never-started") {
		t.Errorf("E1 = %+v, want violated by never-started", r)
	}
}

// TestNoPluginEnabled covers a host without an enabled tslink plugin: no
// container is on a tslink network, but that says nothing about the host.
func TestNoPluginEnabled(t *testing.T) {
	f := newFakeDocker()
	f.plugins[0].Enabled = false
	hc := container.HostConfig{Privileged: true}
	f.add(container.InspectResponse{ID: "c1", Name: "/web", HostConfig: &hc}, "tn")
	results := checkContainers(context.Background(), f)
	for _, p := range []string{"E1", "E2", "E4"} {
		r := result(t, results, p)
		if r.Status != Unknown || !strings.Contains(r.Detail, "no tslink plugin is enabled") {
			t.Errorf("%s = %+v, want unknown: no tslink plugin is enabled", p, r)
		}
	}
}

// TestCustomDataRoot covers a Docker daemon with a data-root other than
// /var/lib/docker: its own data root is as protected.
func TestCustomDataRoot(t *testing.T) {
	for _, tt := range []struct {
		source string
		status Status
	}{
		{"/srv/docker", Violated},
		{"/srv/docker/swarm", Violated},
		{"/srv", Violated},
		{"/srv/app", OK},
		{"/var/lib/docker", Violated},
	} {
		t.Run(tt.source, func(t *testing.T) {
			f := newFakeDocker()
			f.rootDir = "/srv/docker"
			f.add(container.InspectResponse{
				ID: "c1", Name: "/web", HostConfig: &container.HostConfig{},
				Mounts: []container.MountPoint{
					{Type: mount.TypeBind, Source: tt.source, Destination: "/x"},
				},
			}, "tn")
			r := result(t, checkContainers(context.Background(), f), "E2")
			if r.Status != tt.status {
				t.Errorf("E2 = %+v, want %s", r, tt.status)
			}
		})
	}
}

func TestDockerInfoFails(t *testing.T) {
	f := newFakeDocker()
	f.infoErr = errDockerDown
	f.add(
		container.InspectResponse{ID: "c1", Name: "/web", HostConfig: &container.HostConfig{}},
		"tn",
	)
	if r := result(t, checkContainers(context.Background(), f), "E2"); r.Status != Error {
		t.Errorf("E2 = %+v, want error", r)
	}
}
