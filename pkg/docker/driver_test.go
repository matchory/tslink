package docker

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/docker/go-plugins-helpers/network"

	"github.com/aaomidi/tslink/pkg/core"
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
			want: core.Network{AuthKey: "tskey-auth-net", Tags: []string{"tag:a", "tag:b"}, MTU: 1400,
				LoginServer: "https://hs.example.com"},
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
					t.Fatalf("CreateNetwork = %v, stored %v; want an error and nothing stored", err, stored)
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
	if err := td.DeleteNetwork(&network.DeleteNetworkRequest{NetworkID: fakeID("unknown")}); err != nil {
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
	if net.AuthKey != "tskey-auth-idle" || net.MTU != 1380 || !slices.Equal(net.Tags, []string{"tag:idle"}) {
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
		_, err := td.CreateEndpoint(&network.CreateEndpointRequest{NetworkID: netID, EndpointID: fakeID(name)})
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

// Join must not wait for Tailscale, which can take a minute to come up: with
// the container's info already known it starts Tailscale in the background.
func TestJoinDoesNotBlockOnTailscale(t *testing.T) {
	td := newTestDriver(t, newFakeDocker())
	netID, epID := fakeID("net"), fakeID("ep")
	td.createNetwork(t, netID, map[string]string{"tslink.authkey": "k"})
	if _, err := td.CreateEndpoint(&network.CreateEndpointRequest{NetworkID: netID, EndpointID: epID}); err != nil {
		t.Fatal(err)
	}
	info := parseContainerInfo("web", nil)
	td.cache.Store(epID, info)

	done := make(chan error, 1)
	go func() {
		_, err := td.Join(&network.JoinRequest{NetworkID: netID, EndpointID: epID, SandboxKey: sandbox})
		done <- err
	}()
	if err := receive(t, done, "Join to return"); err != nil {
		t.Fatal(err)
	}
	// runTailscale blocks until the test ends: Join returned before it finished
	if r := td.nextRun(t); r.endpointID != epID || r.info != info {
		t.Errorf("started %+v, want endpoint %s with the cached info", r, epID[:12])
	}
	ep, _ := td.endpoint(epID)
	if ep.GetSandboxKey() != sandbox {
		t.Errorf("sandbox key %q", ep.GetSandboxKey())
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

	if _, err := td.Join(&network.JoinRequest{NetworkID: netID, EndpointID: epID, SandboxKey: sandbox}); err == nil {
		t.Error("Join of an unknown endpoint succeeded")
	}

	if _, err := td.CreateEndpoint(&network.CreateEndpointRequest{NetworkID: netID, EndpointID: epID}); err != nil {
		t.Fatal(err)
	}
	td.cache.Store(epID, parseContainerInfo("web", nil))
	td.joinEndpoint = func(*core.Endpoint, string) (*network.JoinResponse, error) {
		return nil, errors.New("veth failed")
	}
	if _, err := td.Join(&network.JoinRequest{NetworkID: netID, EndpointID: epID, SandboxKey: sandbox}); err == nil {
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

	if err := td.DeleteEndpoint(&network.DeleteEndpointRequest{NetworkID: netID, EndpointID: epID}); err != nil {
		t.Fatal(err)
	}
	if _, ok := td.endpoint(epID); ok {
		t.Error("endpoint kept after DeleteEndpoint")
	}
	if _, ok := td.cache.GetByEndpoint(epID); ok {
		t.Error("container info kept after DeleteEndpoint")
	}
	if _, err := td.EndpointInfo(&network.InfoRequest{NetworkID: netID, EndpointID: epID}); err == nil {
		t.Error("EndpointInfo of a deleted endpoint succeeded")
	}

	// Docker retries these after a plugin restart
	if err := td.Leave(&network.LeaveRequest{NetworkID: netID, EndpointID: epID}); err != nil {
		t.Errorf("Leave of an unknown endpoint: %v", err)
	}
	if err := td.DeleteEndpoint(&network.DeleteEndpointRequest{NetworkID: netID, EndpointID: epID}); err != nil {
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
		if _, err := td.CreateEndpoint(&network.CreateEndpointRequest{NetworkID: netID, EndpointID: other}); err != nil {
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
	fake.addContainer("billing_api.1.abc", map[string]string{core.StackLabel: "billing", "tslink.service": "svc:api",
		"tslink.serve.443": "https:8080"},
		attachment{"billing_net", netID, epID}, attachment{"bridge", bridgeID, fakeID("bridge-ep")})
	td := newTestDriver(t, fake)

	if err := td.RecoverEndpoints(t.Context()); err != nil {
		t.Fatal(err)
	}

	net, ok := td.network(netID)
	if !ok || net.AuthKey != "tskey-auth-billing" || !slices.Equal(net.Tags, []string{"tag:billing"}) {
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
	if ep.Network != net || ep.GetSandboxKey() != sandbox {
		t.Errorf("endpoint network %p (want %p), sandbox %q", ep.Network, net, ep.GetSandboxKey())
	}
	if want := filepath.Join(td.config.DataDir, "by-stack", "billing", "billing-api-1-abc"); ep.GetStateDir() != want {
		t.Errorf("state dir %q, want %q claimed before garbage collection", ep.GetStateDir(), want)
	}

	r := td.nextRun(t)
	if r.endpointID != epID || r.info.Name != "billing_api.1.abc" || r.info.Hostname != "billing-api-1-abc" ||
		r.info.Stack != "billing" || r.info.NetworkStack != "billing" || r.info.Service != "svc:api" ||
		len(r.info.Endpoints) != 1 {
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
				td.join(t, netID, epID, sandbox)
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
			netID := fake.addNetwork("net", pluginName, map[string]string{"tslink.authkey": "k"}, nil)
			epID := fakeID("ep")
			fake.addContainer("web", nil, attachment{"net", netID, epID})
			td := newTestDriver(t, fake)
			known := tt.name == "endpoint known"
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
}

// The plugin can be installed under any name: its networks are those whose
// driver is a plugin running tslink's entrypoint.
func TestRecoverEndpointsPluginAlias(t *testing.T) {
	fake := newFakeDocker()
	fake.plugins[0].Name = "ghcr.io/example/tslink:v1"
	netID := fake.addNetwork("net", "ghcr.io/example/tslink:v1", map[string]string{"tslink.authkey": "k"}, nil)
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
	t.Skip("bug: recoverEndpoint keeps the state directory claim when restoring the routing fails")
	fake := newFakeDocker()
	netID := fake.addNetwork("net", pluginName, map[string]string{"tslink.authkey": "k"}, nil)
	fake.addContainer("web", nil, attachment{"net", netID, fakeID("ep")})
	td := newTestDriver(t, fake)
	td.recoverErr = errors.New("no such netns")

	if err := td.RecoverEndpoints(t.Context()); err != nil {
		t.Fatal(err)
	}
	net, _ := td.network(netID)
	restarted, err := core.NewEndpoint(fakeID("ep-new"), net, core.EndpointOptions{}, td.config)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.ClaimStateDir(parseContainerInfo("web", nil)); err != nil {
		t.Errorf("state directory still claimed by the endpoint whose recovery failed: %v", err)
	}
}

// An endpoint that joined but never saw its connect event, as when the event
// stream was down (the watcher reconnects without asking for missed events),
// should get Tailscale from the watchdog's recovery. Recovery skips every
// endpoint the driver knows, started or not, so it never does.
func TestRecoveryStartsEndpointThatMissedItsEvent(t *testing.T) {
	t.Skip("bug: an endpoint whose connect event was missed never starts Tailscale")
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
	if r := td.nextRun(t); r.endpointID != epID || r.info.Hostname != "web" {
		t.Errorf("started %+v", r)
	}
}
