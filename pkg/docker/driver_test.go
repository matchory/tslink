package docker

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/go-plugins-helpers/network"
	"github.com/moby/moby/api/types/container"
	dockernetwork "github.com/moby/moby/api/types/network"
	dockerclient "github.com/moby/moby/client"

	"github.com/matchory/tslink/pkg/core"
	"github.com/matchory/tslink/pkg/logger"
)

const sandbox = "/var/run/docker/netns/test"

func TestCreateNetworkOptions(t *testing.T) {
	tests := []struct {
		name      string
		opts      map[string]any
		pluginKey string
		wantErr   bool
		want      core.Network
	}{
		{
			name: "generic options",
			opts: map[string]any{core.GenericOptionsKey: map[string]any{
				"tslink.authkey": "tskey-auth-net", "tslink.tags": "tag:a,tag:b",
				core.MTUOption: "1400", core.LoginServerOption: "https://hs.example.com",
			}},
			want: core.Network{
				AuthKey: "tskey-auth-net", Tags: []string{"tag:a", "tag:b"}, MTU: 1400,
				LoginServer: "https://hs.example.com",
			},
		},
		{
			name: "top-level options",
			opts: map[string]any{"tslink.authkey": "tskey-auth-net"},
			want: core.Network{AuthKey: "tskey-auth-net"},
		},
		{
			name:      "plugin key without tslink.authkey",
			pluginKey: "tskey-auth-plugin",
			want:      core.Network{AuthKey: "tskey-auth-plugin"},
		},
		{name: "no key at all", wantErr: true},
		{
			name:    "invalid login server",
			opts:    map[string]any{"tslink.authkey": "k", core.LoginServerOption: "ftp://x"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			td := newTestDriver(t, newFakeDocker())
			td.config.AuthKey = tt.pluginKey
			id := fakeID(tt.name)
			err := td.CreateNetwork(&network.CreateNetworkRequest{NetworkID: id, Options: tt.opts})
			net, stored := td.network(id)
			if tt.wantErr {
				if err == nil || stored {
					t.Fatalf(
						"CreateNetwork = %v, stored %v; want an error and nothing stored",
						err,
						stored,
					)
				}
				return
			}
			if err != nil || !stored {
				t.Fatalf("CreateNetwork = %v, stored %v", err, stored)
			}
			if net.ID != id || net.AuthKey != tt.want.AuthKey || net.MTU != tt.want.MTU ||
				net.LoginServer != tt.want.LoginServer || !slices.Equal(net.Tags, tt.want.Tags) {
				t.Errorf("network %+v, want %+v", net, tt.want)
			}
			if !td.ownsNetwork(id) {
				t.Error("driver does not own the network it created")
			}
		})
	}
}

func TestDeleteNetwork(t *testing.T) {
	td := newTestDriver(t, newFakeDocker())
	id := fakeID("net")
	td.createNetwork(t, id, map[string]string{"tslink.authkey": "k"})
	if err := td.DeleteNetwork(&network.DeleteNetworkRequest{NetworkID: id}); err != nil {
		t.Fatal(err)
	}
	if td.ownsNetwork(id) {
		t.Error("network still owned after DeleteNetwork")
	}
	// Docker may delete a network the restarted plugin never saw
	if err := td.DeleteNetwork(
		&network.DeleteNetworkRequest{NetworkID: fakeID("unknown")},
	); err != nil {
		t.Errorf("DeleteNetwork of an unknown network: %v", err)
	}
}

// After a plugin restart the driver has no networks. A network without
// running containers is not recovered, so the first endpoint on it must
// rebuild it from what Docker stored at creation (B3).
func TestCreateEndpointRebuildsIdleNetwork(t *testing.T) {
	fake := newFakeDocker()
	netID := fake.addNetwork("idle", pluginName, map[string]string{
		"tslink.authkey": "tskey-auth-idle", "tslink.tags": "tag:idle", core.MTUOption: "1380",
	}, nil)
	td := newTestDriver(t, fake)

	td.join(t, netID, fakeID("ep1"), sandbox)
	net, ok := td.network(netID)
	if !ok {
		t.Fatal("network not adopted")
	}
	if net.AuthKey != "tskey-auth-idle" || net.MTU != 1380 ||
		!slices.Equal(net.Tags, []string{"tag:idle"}) {
		t.Errorf("rebuilt network %+v", net)
	}
	ep, ok := td.endpoint(fakeID("ep1"))
	if !ok || ep.Network != net {
		t.Errorf("endpoint %+v not on the rebuilt network", ep)
	}
	if !td.ownsNetwork(netID) {
		t.Error("events for the rebuilt network would be ignored")
	}

	td.join(t, netID, fakeID("ep2"), sandbox)
	if n := fake.networkInspects(); n != 1 {
		t.Errorf("network inspected %d times, want once", n)
	}
}

func TestCreateEndpointUnknownNetwork(t *testing.T) {
	fake := newFakeDocker()
	noKey := fake.addNetwork("nokey", pluginName, nil, nil)
	td := newTestDriver(t, fake)
	td.config.AuthKey = ""

	for name, netID := range map[string]string{"not in Docker": fakeID("gone"), "no credential": noKey} {
		_, err := td.CreateEndpoint(
			&network.CreateEndpointRequest{NetworkID: netID, EndpointID: fakeID(name)},
		)
		if err == nil {
			t.Errorf("%s: CreateEndpoint succeeded", name)
		}
		if _, ok := td.endpoint(fakeID(name)); ok {
			t.Errorf("%s: endpoint stored", name)
		}
		if _, ok := td.network(netID); ok {
			t.Errorf("%s: network stored", name)
		}
	}
}

// CreateEndpoint keeps the address Docker's IPAM assigned, which Join puts on
// the container: Docker lists it in the container's /etc/hosts.
func TestCreateEndpointKeepsIPAMAddress(t *testing.T) {
	td := newTestDriver(t, newFakeDocker())
	netID := fakeID("net")
	td.createNetwork(t, netID, map[string]string{"tslink.authkey": "k"})

	for _, tc := range []struct {
		name  string
		iface *network.EndpointInterface
		want  string
	}{
		{"assigned", &network.EndpointInterface{Address: "10.42.6.51/24"}, "10.42.6.51"},
		{"none", nil, ""},
		{"empty", &network.EndpointInterface{}, ""},
	} {
		epID := fakeID(tc.name)
		if _, err := td.CreateEndpoint(
			&network.CreateEndpointRequest{NetworkID: netID, EndpointID: epID, Interface: tc.iface},
		); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		ep, _ := td.endpoint(epID)
		if ep.IPAMAddress != tc.want {
			t.Errorf("%s: IPAMAddress %q, want %q", tc.name, ep.IPAMAddress, tc.want)
		}
	}
}

// Join must not wait for Tailscale, which can take a minute to come up: with
// the container's info already known it starts Tailscale in the background.
func TestJoinDoesNotBlockOnTailscale(t *testing.T) {
	td := newTestDriver(t, newFakeDocker())
	netID, epID := fakeID("net"), fakeID("ep")
	td.createNetwork(t, netID, map[string]string{"tslink.authkey": "k"})
	if _, err := td.CreateEndpoint(
		&network.CreateEndpointRequest{NetworkID: netID, EndpointID: epID},
	); err != nil {
		t.Fatal(err)
	}
	info := parseContainerInfo("web", nil)
	td.cache.Store(epID, info)

	done := make(chan error, 1)
	go func() {
		_, err := td.Join(
			&network.JoinRequest{NetworkID: netID, EndpointID: epID, SandboxKey: sandbox},
		)
		done <- err
	}()
	if err := receive(t, done, "Join to return"); err != nil {
		t.Fatal(err)
	}
	// runTailscale blocks until the test ends: Join returned before it finished
	r := td.nextRun(t)
	if r.endpointID != epID || r.info != info {
		t.Errorf("started %+v, want endpoint %s with the cached info", r, epID[:12])
	}
	if !strings.Contains(r.traceback, "endpoint: "+epID[:12]) {
		t.Errorf("traceback %q does not name the endpoint", r.traceback)
	}
	ep, _ := td.endpoint(epID)
	if ep.SandboxKey() != sandbox {
		t.Errorf("sandbox key %q", ep.SandboxKey())
	}
}

// Without the container's info Join leaves starting Tailscale to the event.
func TestJoinWithoutInfoWaitsForEvent(t *testing.T) {
	td := newTestDriver(t, newFakeDocker())
	netID := fakeID("net")
	td.createNetwork(t, netID, map[string]string{"tslink.authkey": "k"})
	td.join(t, netID, fakeID("ep"), sandbox)
	td.noRun(t)
}

func TestJoinErrors(t *testing.T) {
	td := newTestDriver(t, newFakeDocker())
	netID, epID := fakeID("net"), fakeID("ep")
	td.createNetwork(t, netID, map[string]string{"tslink.authkey": "k"})

	if _, err := td.Join(
		&network.JoinRequest{NetworkID: netID, EndpointID: epID, SandboxKey: sandbox},
	); err == nil {
		t.Error("Join of an unknown endpoint succeeded")
	}

	if _, err := td.CreateEndpoint(
		&network.CreateEndpointRequest{NetworkID: netID, EndpointID: epID},
	); err != nil {
		t.Fatal(err)
	}
	td.cache.Store(epID, parseContainerInfo("web", nil))
	td.joinEndpoint = func(*core.Endpoint, string) (*network.JoinResponse, error) {
		return nil, errors.New("veth failed")
	}
	if _, err := td.Join(
		&network.JoinRequest{NetworkID: netID, EndpointID: epID, SandboxKey: sandbox},
	); err == nil {
		t.Error("Join succeeded although setting up the veth failed")
	}
	td.noRun(t)
}

// Leave keeps the endpoint until Docker deletes it; DeleteEndpoint forgets it
// and the container's info.
func TestLeaveAndDeleteEndpoint(t *testing.T) {
	td := newTestDriver(t, newFakeDocker())
	netID, epID := fakeID("net"), fakeID("ep")
	td.createNetwork(t, netID, map[string]string{"tslink.authkey": "k"})
	td.join(t, netID, epID, "/nonexistent/tslink-test/netns")
	td.cache.Store(epID, parseContainerInfo("web", nil))

	if err := td.Leave(&network.LeaveRequest{NetworkID: netID, EndpointID: epID}); err != nil {
		t.Fatal(err)
	}
	if _, ok := td.endpoint(epID); !ok {
		t.Error("Leave removed the endpoint; DeleteEndpoint does")
	}

	if err := td.DeleteEndpoint(
		&network.DeleteEndpointRequest{NetworkID: netID, EndpointID: epID},
	); err != nil {
		t.Fatal(err)
	}
	if _, ok := td.endpoint(epID); ok {
		t.Error("endpoint kept after DeleteEndpoint")
	}
	if _, ok := td.cache.GetByEndpoint(epID); ok {
		t.Error("container info kept after DeleteEndpoint")
	}
	if _, err := td.EndpointInfo(
		&network.InfoRequest{NetworkID: netID, EndpointID: epID},
	); err == nil {
		t.Error("EndpointInfo of a deleted endpoint succeeded")
	}

	// Docker retries these after a plugin restart
	if err := td.Leave(&network.LeaveRequest{NetworkID: netID, EndpointID: epID}); err != nil {
		t.Errorf("Leave of an unknown endpoint: %v", err)
	}
	if err := td.DeleteEndpoint(
		&network.DeleteEndpointRequest{NetworkID: netID, EndpointID: epID},
	); err != nil {
		t.Errorf("DeleteEndpoint of an unknown endpoint: %v", err)
	}
}

// blockingOp returns an endpoint operation that reports each call on entered
// and returns once release is closed.
func blockingOp(entered chan<- string, release <-chan struct{}) func(*core.Endpoint) error {
	return func(e *core.Endpoint) error {
		entered <- e.ID
		<-release
		return nil
	}
}

// otherDriverCallsComplete fails the test unless driver calls on another
// endpoint, which need the driver's lock, complete within a second.
func otherDriverCallsComplete(t *testing.T, td *testDriver, netID string) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		other := fakeID("other")
		if _, err := td.CreateEndpoint(
			&network.CreateEndpointRequest{NetworkID: netID, EndpointID: other},
		); err != nil {
			done <- err
			return
		}
		_, err := td.EndpointInfo(&network.InfoRequest{NetworkID: netID, EndpointID: other})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("driver calls blocked while an endpoint was stopping")
	}
}

// Leaving, which drains and waits for control, holds no driver lock: other
// endpoints keep working meanwhile, and the leaving one stays known until
// Docker deletes it.
func TestLeaveDoesNotHoldDriverLock(t *testing.T) {
	td := newTestDriver(t, newFakeDocker())
	entered, release := make(chan string, 1), make(chan struct{})
	td.leaveEndpoint = blockingOp(entered, release)
	netID, epID := fakeID("net"), fakeID("ep")
	td.createNetwork(t, netID, map[string]string{"tslink.authkey": "k"})
	td.join(t, netID, epID, sandbox)

	left := make(chan error, 1)
	go func() { left <- td.Leave(&network.LeaveRequest{NetworkID: netID, EndpointID: epID}) }()
	if got := receive(t, entered, "the endpoint's Leave"); got != epID {
		t.Fatalf("left %s, want %s", got, epID)
	}
	otherDriverCallsComplete(t, td, netID)
	if _, ok := td.endpoint(epID); !ok {
		t.Error("leaving endpoint forgotten before DeleteEndpoint")
	}
	close(release)
	if err := receive(t, left, "Leave to return"); err != nil {
		t.Fatal(err)
	}
}

// DeleteEndpoint forgets the endpoint under the lock and stops it after
// releasing it.
func TestDeleteEndpointDoesNotHoldDriverLock(t *testing.T) {
	td := newTestDriver(t, newFakeDocker())
	entered, release := make(chan string, 1), make(chan struct{})
	td.stopEndpoint = blockingOp(entered, release)
	netID, epID := fakeID("net"), fakeID("ep")
	td.createNetwork(t, netID, map[string]string{"tslink.authkey": "k"})
	td.join(t, netID, epID, sandbox)

	deleted := make(chan error, 1)
	go func() {
		deleted <- td.DeleteEndpoint(&network.DeleteEndpointRequest{NetworkID: netID, EndpointID: epID})
	}()
	receive(t, entered, "the endpoint's Stop")
	if _, ok := td.endpoint(epID); ok {
		t.Error("endpoint still known while it is being stopped")
	}
	otherDriverCallsComplete(t, td, netID)
	close(release)
	if err := receive(t, deleted, "DeleteEndpoint to return"); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownClosesClient(t *testing.T) {
	fake := newFakeDocker()
	d := newDriver(&core.Config{DataDir: t.TempDir()}, fake)
	d.hostIsolation = func(bool, []string) error { return nil }
	d.start()
	if err := d.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !fake.closed {
		t.Error("Docker client not closed")
	}
}

// After a restart recovery adopts the endpoints of running containers: it
// rebuilds their networks from inspect, restores the routing Join set up, and
// starts Tailscale with the container's name and labels.
func TestRecoverEndpoints(t *testing.T) {
	fake := newFakeDocker()
	netID := fake.addNetwork("billing_net", pluginName,
		map[string]string{"tslink.authkey": "tskey-auth-billing", "tslink.tags": "tag:billing"},
		map[string]string{core.StackLabel: "billing"})
	bridgeID := fake.addNetwork("bridge", "bridge", nil, nil)
	epID := fakeID("ep")
	fake.addContainer("billing_api.1.abc", map[string]string{
		core.StackLabel: "billing", "tslink.service": "svc:api",
		"tslink.serve.443": "https:8080",
	},
		attachment{"billing_net", netID, epID}, attachment{"bridge", bridgeID, fakeID("bridge-ep")})
	dns := netip.MustParseAddr("100.100.100.100")
	fake.setDNS(fakeID("container/billing_api.1.abc"), dns)
	td := newTestDriver(t, fake)

	if err := td.RecoverEndpoints(t.Context()); err != nil {
		t.Fatal(err)
	}

	net, ok := td.network(netID)
	if !ok || net.AuthKey != "tskey-auth-billing" ||
		!slices.Equal(net.Tags, []string{"tag:billing"}) {
		t.Fatalf("network not rebuilt from inspect: %+v", net)
	}
	if _, ok := td.network(bridgeID); ok {
		t.Error("adopted a network of another driver")
	}
	ep, ok := td.endpoint(epID)
	if !ok {
		t.Fatal("endpoint not recovered")
	}
	if _, ok := td.endpoint(fakeID("bridge-ep")); ok {
		t.Error("recovered an endpoint of another driver")
	}
	if ep.Network != net || ep.SandboxKey() != sandbox {
		t.Errorf("endpoint network %p (want %p), sandbox %q", ep.Network, net, ep.SandboxKey())
	}
	if want := filepath.Join(
		td.config.DataDir,
		"by-stack",
		"billing",
		"billing-api-1-abc",
	); ep.StateDir() != want {
		t.Errorf("state dir %q, want %q claimed before garbage collection", ep.StateDir(), want)
	}

	r := td.nextRun(t)
	if r.endpointID != epID || r.info.Name != "billing_api.1.abc" ||
		r.info.ID != fakeID("container/billing_api.1.abc") ||
		r.info.Hostname != "billing-api-1-abc" ||
		r.info.Stack != "billing" ||
		r.info.NetworkStack != "billing" ||
		r.info.Service != "svc:api" ||
		len(r.info.Endpoints) != 1 ||
		!slices.Equal(r.info.DNS, []netip.Addr{dns}) {
		t.Errorf("started Tailscale with %+v", r.info)
	}
	if info, ok := td.cache.GetByEndpoint(epID); !ok || info != r.info {
		t.Error("container info not cached for the event handler")
	}

	// The watchdog runs recovery again: nothing more to do
	if err := td.RecoverEndpoints(t.Context()); err != nil {
		t.Fatal(err)
	}
	td.noRun(t)
}

// A scan lists the networks once, instead of inspecting the network of every
// attachment of every container on the host: the watchdog scans every minute.
func TestRecoverEndpointsListsNetworksOnce(t *testing.T) {
	fake := newFakeDocker()
	opts := map[string]string{"tslink.authkey": "k"}
	nets := []string{
		fake.addNetwork("a", pluginName, opts, nil),
		fake.addNetwork("b", pluginName, opts, nil),
		fake.addNetwork("bridge", "bridge", nil, nil),
	}
	const containers = 3
	for c := range containers {
		name := "web" + strconv.Itoa(c)
		var atts []attachment
		for i, netID := range nets {
			atts = append(atts, attachment{"net" + strconv.Itoa(i), netID, fakeID(name + netID)})
		}
		fake.addContainer(name, nil, atts...)
	}
	td := newTestDriver(t, fake)

	if err := td.RecoverEndpoints(t.Context()); err != nil {
		t.Fatal(err)
	}
	for range containers * 2 {
		td.nextRun(t)
	}
	td.noRun(t) // nothing on the bridge network
	if n := fake.networkInspects(); n != 0 {
		t.Errorf("inspected networks %d times, want none", n)
	}
	fake.mu.Lock()
	lists := len(fake.networkLists)
	fake.mu.Unlock()
	if lists != 1 {
		t.Errorf("listed networks %d times, want once", lists)
	}
}

func TestRecoverEndpointsSkips(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, td *testDriver, fake *fakeDocker, netID, epID string)
	}{
		{
			name: "plugin disabled",
			setup: func(_ *testing.T, _ *testDriver, fake *fakeDocker, _, _ string) {
				fake.plugins[0].Enabled = false
			},
		},
		{
			name: "network of another plugin",
			setup: func(_ *testing.T, _ *testDriver, fake *fakeDocker, _, _ string) {
				fake.plugins[0].Config.Entrypoint = []string{"/other"}
			},
		},
		{
			name: "endpoint known",
			setup: func(t *testing.T, td *testDriver, _ *fakeDocker, netID, epID string) {
				// Join finds the container's info cached and starts Tailscale
				td.cache.Store(epID, parseContainerInfo("web", nil))
				td.join(t, netID, epID, sandbox)
				td.nextRun(t)
			},
		},
		{
			name: "endpoint without sandbox",
			setup: func(t *testing.T, td *testDriver, _ *fakeDocker, netID, epID string) {
				// Created, not yet joined: Join starts it
				if _, err := td.CreateEndpoint(
					&network.CreateEndpointRequest{NetworkID: netID, EndpointID: epID},
				); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "no sandbox key",
			setup: func(_ *testing.T, _ *testDriver, fake *fakeDocker, _, _ string) {
				fake.containers[0].NetworkSettings.SandboxKey = ""
			},
		},
		{
			name: "routing not restored",
			setup: func(_ *testing.T, td *testDriver, _ *fakeDocker, _, _ string) {
				td.recoverErr = errors.New("no such netns")
			},
		},
		{
			name: "network without credential",
			setup: func(_ *testing.T, td *testDriver, fake *fakeDocker, netID, _ string) {
				td.config.AuthKey = ""
				n := fake.networks[netID]
				n.Options = nil
				fake.networks[netID] = n
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeDocker()
			netID := fake.addNetwork(
				"net",
				pluginName,
				map[string]string{"tslink.authkey": "k"},
				nil,
			)
			epID := fakeID("ep")
			fake.addContainer("web", nil, attachment{"net", netID, epID})
			td := newTestDriver(t, fake)
			known := tt.name == "endpoint known" || tt.name == "endpoint without sandbox"
			tt.setup(t, td, fake, netID, epID)

			if err := td.RecoverEndpoints(t.Context()); err != nil {
				t.Fatalf("RecoverEndpoints: %v", err)
			}
			if _, ok := td.endpoint(epID); ok != known {
				t.Errorf("endpoint stored = %v, want %v", ok, known)
			}
			td.noRun(t)
		})
	}
}

func TestRecoverEndpointsDockerErrors(t *testing.T) {
	fake := newFakeDocker()
	fake.pluginErrs = []error{errors.New("daemon starting")}
	td := newTestDriver(t, fake)
	if err := td.RecoverEndpoints(t.Context()); err == nil {
		t.Error("RecoverEndpoints succeeded although listing plugins failed")
	}
	fake.listErr = errors.New("daemon starting")
	if err := td.RecoverEndpoints(t.Context()); err == nil {
		t.Error("RecoverEndpoints succeeded although listing containers failed")
	}
	fake.listErr = nil
	fake.networkListErr = errors.New("daemon starting")
	if err := td.RecoverEndpoints(t.Context()); err == nil {
		t.Error("RecoverEndpoints succeeded although listing networks failed")
	}
}

// The plugin can be installed under any name: its networks are those whose
// driver is a plugin running tslink's entrypoint.
func TestRecoverEndpointsPluginAlias(t *testing.T) {
	fake := newFakeDocker()
	fake.plugins[0].Name = "ghcr.io/example/tslink:v1"
	netID := fake.addNetwork(
		"net",
		"ghcr.io/example/tslink:v1",
		map[string]string{"tslink.authkey": "k"},
		nil,
	)
	epID := fakeID("ep")
	fake.addContainer("web", nil, attachment{"net", netID, epID})
	td := newTestDriver(t, fake)

	if err := td.RecoverEndpoints(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r := td.nextRun(t); r.endpointID != epID || r.info.Hostname != "web" {
		t.Errorf("started %+v", r)
	}
}

// Garbage collection runs once recovery knows every endpoint, and not before:
// it would take the state and sockets of endpoints not yet recovered.
func TestStartCollectsGarbageAfterRecovery(t *testing.T) {
	fake := newFakeDocker()
	fake.pluginErrs = []error{errors.New("daemon starting")}
	fake.pluginCalls = make(chan struct{}, 4)
	netID := fake.addNetwork("net", pluginName, map[string]string{"tslink.authkey": "k"}, nil)
	epID := fakeID("ep")
	fake.addContainer("web", nil, attachment{"net", netID, epID})
	td := newTestDriver(t, fake)

	sockDir := filepath.Join(td.config.DataDir, "sock")
	if err := os.MkdirAll(sockDir, 0o750); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	stale := filepath.Join(sockDir, fakeID("gone")[:12]+".sock")
	inUse := filepath.Join(sockDir, epID[:12]+".sock")
	for _, f := range []string{stale, inUse} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(f, old, old); err != nil {
			t.Fatal(err)
		}
	}

	td.start()

	receive(t, fake.pluginCalls, "the first recovery")
	// The first recovery failed: nothing is collected until one succeeds
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("collected garbage after a failed recovery: %v", err)
	}

	receive(t, fake.pluginCalls, "recovery to be retried")
	td.nextRun(t)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(stale); errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("socket of an unknown endpoint not collected after recovery")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(inUse); err != nil {
		t.Errorf("collected the socket of the recovered endpoint: %v", err)
	}
}

// A recovery that fails after claiming the container's state directory must
// give the claim up: otherwise the container, restarted with a new endpoint,
// cannot start Tailscale on its state and retries forever.
func TestFailedRecoveryReleasesStateDir(t *testing.T) {
	fake := newFakeDocker()
	netID := fake.addNetwork("net", pluginName, map[string]string{"tslink.authkey": "k"}, nil)
	fake.addContainer("web", nil, attachment{"net", netID, fakeID("ep")})
	td := newTestDriver(t, fake)
	td.recoverErr = errors.New("no such netns")

	if err := td.RecoverEndpoints(t.Context()); err != nil {
		t.Fatal(err)
	}
	net, _ := td.network(netID)
	restarted, err := core.NewEndpoint(
		fakeID("ep-new"),
		net,
		core.EndpointOptions{},
		td.config,
		td.claims,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.ClaimStateDir(parseContainerInfo("web", nil)); err != nil {
		t.Errorf("state directory still claimed by the endpoint whose recovery failed: %v", err)
	}
}

// An endpoint that joined but never saw its connect event, as when events
// were lost while the event stream was down, gets Tailscale from the
// watchdog's recovery, with the container's info from inspect.
func TestRecoveryStartsEndpointThatMissedItsEvent(t *testing.T) {
	fake := newFakeDocker()
	netID := fake.addNetwork("net", pluginName, map[string]string{"tslink.authkey": "k"}, nil)
	epID := fakeID("ep")
	fake.addContainer("web", nil, attachment{"net", netID, epID})
	td := newTestDriver(t, fake)
	td.createNetwork(t, netID, map[string]string{"tslink.authkey": "k"})
	td.join(t, netID, epID, sandbox)

	if err := td.RecoverEndpoints(t.Context()); err != nil {
		t.Fatal(err)
	}
	r := td.nextRun(t)
	if r.endpointID != epID || r.info.Hostname != "web" {
		t.Errorf("started %+v", r)
	}
	if info, ok := td.cache.GetByEndpoint(epID); !ok || info != r.info {
		t.Error("container info not cached")
	}

	// Started now: the next recovery leaves it alone
	if err := td.RecoverEndpoints(t.Context()); err != nil {
		t.Fatal(err)
	}
	td.noRun(t)
}

// Recovery skips network settings Docker leaves out, rather than crashing on
// them.
func TestRecoverEndpointsWithoutNetworkSettings(t *testing.T) {
	fake := newFakeDocker()
	netID := fake.addNetwork("net", pluginName, map[string]string{"tslink.authkey": "k"}, nil)
	epID, joinedID := fakeID("ep"), fakeID("joined")
	fake.addContainer("web", nil, attachment{"net", netID, epID})
	fake.addContainer("api", nil, attachment{"net", netID, joinedID})
	fake.containers[0].NetworkSettings.Networks["other"] = nil
	fake.noSettings = true
	td := newTestDriver(t, fake)
	td.createNetwork(t, netID, map[string]string{"tslink.authkey": "k"})
	td.join(t, netID, joinedID, sandbox) // missed its connect event

	if err := td.RecoverEndpoints(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := td.endpoint(epID); ok {
		t.Error("recovered an endpoint of a container without network settings")
	}
	td.noRun(t)
}

// The plugin applies its isolation setting before it serves requests, so no
// container joins while the host's tailnet is open to it.
// Guards: G1
func TestStartAppliesHostIsolation(t *testing.T) {
	for _, on := range []bool{true, false} {
		d := newDriver(&core.Config{DataDir: t.TempDir(), IsolateHostTailnet: on}, newFakeDocker())
		var calls []bool
		d.hostIsolation = func(on bool, _ []string) error {
			calls = append(calls, on)
			return nil
		}
		d.start()
		if !slices.Equal(calls, []bool{on}) {
			t.Errorf(
				"IsolateHostTailnet=%v: start applied %v, want [%v] before returning",
				on,
				calls,
				on,
			)
		}
		d.cancel()
		d.wg.Wait()
	}
}

// cancelingDocker cancels the plugin's context while Docker lists plugins, as
// a plugin shutdown during a watchdog scan does.
type cancelingDocker struct {
	*fakeDocker
	cancel context.CancelFunc
}

func (c cancelingDocker) PluginList(
	ctx context.Context,
	_ dockerclient.PluginListOptions,
) (dockerclient.PluginListResult, error) {
	c.cancel()
	return dockerclient.PluginListResult{}, ctx.Err()
}

// A watchdog scan cut short by the plugin's shutdown is not an error.
func TestWatchdogShutdownNotAnError(t *testing.T) {
	old := watchdogInterval
	watchdogInterval = 10 * time.Millisecond
	t.Cleanup(func() { watchdogInterval = old })
	var log lockedBuffer
	logger.SetOutput(&log)
	t.Cleanup(func() { logger.SetOutput(os.Stdout) })

	td := newTestDriver(t, newFakeDocker())
	ctx, cancel := context.WithCancel(t.Context())
	td.docker = cancelingDocker{td.fake, cancel}
	td.runWatchdog(ctx)
	if strings.Contains(log.String(), "level=ERROR") {
		t.Errorf("the watchdog logged an error at shutdown:\n%s", log.String())
	}
}

// A Docker listing that never returns fails the scan instead of holding
// recoverMu, and with it every later watchdog scan, for good.
func TestRecoverEndpointsTimesOut(t *testing.T) {
	old := dockerCallTimeout
	dockerCallTimeout = 50 * time.Millisecond
	t.Cleanup(func() { dockerCallTimeout = old })
	fake := newFakeDocker()
	fake.addNetwork("a", pluginName, map[string]string{"tslink.authkey": "k"}, nil)
	fake.hangList = true
	td := newTestDriver(t, fake)

	done := make(chan error, 1)
	go func() { done <- td.RecoverEndpoints(t.Context()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a scan whose network listing timed out succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RecoverEndpoints still waits for Docker")
	}
}

// Shutdown stops the endpoints concurrently: each stop can take seconds
// (tailscaled's grace period), and one after another they would exceed the
// shutdown deadline.
func TestShutdownStopsEndpointsConcurrently(t *testing.T) {
	td := newTestDriver(t, newFakeDocker())
	const n, stopTime = 5, time.Second
	var stopped atomic.Int32
	td.stopEndpoint = func(*core.Endpoint) error {
		time.Sleep(stopTime)
		stopped.Add(1)
		return nil
	}
	netID := fakeID("net")
	td.createNetwork(t, netID, map[string]string{"tslink.authkey": "k"})
	for i := range n {
		td.join(t, netID, fakeID(fmt.Sprintf("ep%d", i)), sandbox)
	}

	start := time.Now()
	if err := td.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > drainGrace+2*stopTime {
		t.Errorf("Shutdown took %v for %d stops of %v each", elapsed, n, stopTime)
	}
	if got := stopped.Load(); got != n {
		t.Errorf("stopped %d endpoints, want %d", got, n)
	}
}

// Shutdown waits for the stops no longer than its context allows.
func TestShutdownStopsWithinDeadline(t *testing.T) {
	td := newTestDriver(t, newFakeDocker())
	release := make(chan struct{})
	defer close(release)
	td.stopEndpoint = func(*core.Endpoint) error {
		<-release
		return nil
	}
	netID := fakeID("net")
	td.createNetwork(t, netID, map[string]string{"tslink.authkey": "k"})
	td.join(t, netID, fakeID("ep"), sandbox)

	ctx, cancel := context.WithTimeout(t.Context(), drainGrace+500*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- td.Shutdown(ctx) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Shutdown = %v, want the deadline exceeded", err)
		}
	case <-time.After(drainGrace + 2*time.Second):
		t.Fatal("Shutdown waited past its deadline for a stop")
	}
}

// Shutdown and a stop signal drain through the driver's drainService, and a
// recovery that fails stops its endpoint through stopEndpoint, as every other
// caller does: tests that replace them see every call.
func TestEndpointOpsUseDriverHooks(t *testing.T) {
	fake := newFakeDocker()
	netID := fake.addNetwork("net", pluginName, map[string]string{"tslink.authkey": "k"}, nil)
	recovered := fakeID("recovered")
	fake.addContainer("web", nil, attachment{"net", netID, recovered})
	td := newTestDriver(t, fake)
	var mu sync.Mutex
	var drained, stopped []string
	td.drainService = func(e *core.Endpoint) {
		mu.Lock()
		defer mu.Unlock()
		drained = append(drained, e.ID)
	}
	td.stopEndpoint = func(e *core.Endpoint) error {
		mu.Lock()
		defer mu.Unlock()
		stopped = append(stopped, e.ID)
		return nil
	}
	td.recoverErr = errors.New("no such netns")
	if err := td.RecoverEndpoints(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(stopped, []string{recovered}) {
		t.Errorf("failed recovery stopped %v, want the recovered endpoint", stopped)
	}

	joined := fakeID("joined")
	td.join(t, netID, joined, sandbox)
	td.onContainerStop("web", &container.InspectResponse{
		NetworkSettings: &container.NetworkSettings{
			Networks: map[string]*dockernetwork.EndpointSettings{
				"net": {NetworkID: netID, EndpointID: joined},
			},
		},
	})
	td.wg.Wait()
	if err := td.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(drained, []string{joined, joined}) {
		t.Errorf("drained %v, want the joined endpoint on its stop and on shutdown", drained)
	}
}
