package preflight

import (
	"context"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
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
