package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/netip"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/go-plugins-helpers/network"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	dockernetwork "github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/plugin"
	dockerclient "github.com/moby/moby/client"

	"github.com/matchory/tslink/pkg/core"
)

// pluginName is the name tslink is installed under in the fake Docker.
const pluginName = "tslink:latest"

// fakeID returns a 64-character hex ID derived from name, like Docker's.
func fakeID(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])
}

// fakeDocker is an in-memory Docker API: plugins, networks and containers,
// and an event stream the test feeds.
type fakeDocker struct {
	mu         sync.Mutex
	plugins    []plugin.Plugin
	networks   map[string]dockernetwork.Inspect
	containers []container.InspectResponse

	pluginErrs     []error       // returned by successive PluginList calls, then nil
	pluginCalls    chan struct{} // receives on each PluginList call, if set
	listErr        error
	networkInspect int  // NetworkInspect calls
	noSettings     bool // ContainerInspect returns no network settings

	events      chan events.Message
	errs        chan error
	eventsCalls []dockerclient.EventsListOptions
	closed      bool
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		plugins: []plugin.Plugin{
			{
				Name:    pluginName,
				Enabled: true,
				Config:  plugin.Config{Entrypoint: []string{"/tslink"}},
			},
			{
				Name:    "other:latest",
				Enabled: true,
				Config:  plugin.Config{Entrypoint: []string{"/other"}},
			},
		},
		networks: make(map[string]dockernetwork.Inspect),
		events:   make(chan events.Message),
		errs:     make(chan error, 1),
	}
}

func (f *fakeDocker) PluginList(
	context.Context,
	dockerclient.PluginListOptions,
) (dockerclient.PluginListResult, error) {
	f.mu.Lock()
	var err error
	if len(f.pluginErrs) > 0 {
		err, f.pluginErrs = f.pluginErrs[0], f.pluginErrs[1:]
	}
	items := append([]plugin.Plugin(nil), f.plugins...)
	calls := f.pluginCalls
	f.mu.Unlock()
	if calls != nil {
		calls <- struct{}{}
	}
	if err != nil {
		return dockerclient.PluginListResult{}, err
	}
	return dockerclient.PluginListResult{Items: items}, nil
}

// attachment is a container's endpoint on a network.
type attachment struct {
	netName, netID, endpointID string
}

func (f *fakeDocker) ContainerList(
	context.Context,
	dockerclient.ContainerListOptions,
) (dockerclient.ContainerListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return dockerclient.ContainerListResult{}, f.listErr
	}
	var res dockerclient.ContainerListResult
	for _, c := range f.containers {
		res.Items = append(res.Items, container.Summary{
			ID:    c.ID,
			Names: []string{c.Name},
			NetworkSettings: &container.NetworkSettingsSummary{
				Networks: c.NetworkSettings.Networks,
			},
		})
	}
	return res, nil
}

func (f *fakeDocker) ContainerInspect(
	_ context.Context,
	id string,
	_ dockerclient.ContainerInspectOptions,
) (dockerclient.ContainerInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.containers {
		if c.ID == id {
			if f.noSettings {
				c.NetworkSettings = nil
			}
			return dockerclient.ContainerInspectResult{Container: c}, nil
		}
	}
	return dockerclient.ContainerInspectResult{}, errors.New("no such container")
}

func (f *fakeDocker) NetworkInspect(
	_ context.Context,
	id string,
	_ dockerclient.NetworkInspectOptions,
) (dockerclient.NetworkInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.networkInspect++
	n, ok := f.networks[id]
	if !ok {
		return dockerclient.NetworkInspectResult{}, errors.New("no such network")
	}
	return dockerclient.NetworkInspectResult{Network: n}, nil
}

func (f *fakeDocker) Events(
	_ context.Context,
	options dockerclient.EventsListOptions,
) dockerclient.EventsResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.eventsCalls = append(f.eventsCalls, options)
	return dockerclient.EventsResult{Messages: f.events, Err: f.errs}
}

func (f *fakeDocker) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// addNetwork adds a network with the given driver and returns its ID.
func (f *fakeDocker) addNetwork(name, driver string, opts, labels map[string]string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := fakeID("network/" + name)
	var n dockernetwork.Inspect
	n.ID, n.Name, n.Driver, n.Options, n.Labels = id, name, driver, opts, labels
	f.networks[id] = n
	return id
}

// addContainer adds a running container attached to the given networks,
// whose namespace is sandbox.
func (f *fakeDocker) addContainer(
	name string,
	labels map[string]string,
	nets ...attachment,
) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := fakeID("container/" + name)
	settings := make(map[string]*dockernetwork.EndpointSettings, len(nets))
	for _, a := range nets {
		settings[a.netName] = &dockernetwork.EndpointSettings{
			NetworkID:  a.netID,
			EndpointID: a.endpointID,
		}
	}
	var c container.InspectResponse
	c.ID = id
	c.Name = "/" + name
	c.Config = &container.Config{Labels: labels}
	c.NetworkSettings = &container.NetworkSettings{SandboxKey: sandbox, Networks: settings}
	f.containers = append(f.containers, c)
	return id
}

// setDNS sets the DNS servers the container was given.
func (f *fakeDocker) setDNS(containerID string, dns ...netip.Addr) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.containers {
		if f.containers[i].ID == containerID {
			f.containers[i].HostConfig = &container.HostConfig{DNS: dns}
		}
	}
}

func (f *fakeDocker) eventStreams() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.eventsCalls)
}

// eventsOptions returns the options of each event stream opened.
func (f *fakeDocker) eventsOptions() []dockerclient.EventsListOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.eventsCalls)
}

func (f *fakeDocker) networkInspects() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.networkInspect
}

// tsRun is a call of the driver's runTailscale.
type tsRun struct {
	endpointID string
	info       *core.ContainerInfo
	traceback  string // Header line of the run goroutine's traceback
}

// testDriver is a driver on a fake Docker whose endpoint operations need
// neither root nor tailscaled: Join and Recover only record the sandbox key,
// and starting Tailscale reports on runs and then blocks until the test ends,
// as a slow tailscale up would.
type testDriver struct {
	*Driver
	fake       *fakeDocker
	runs       chan tsRun
	recoverErr error
}

func newTestDriver(t *testing.T, fake *fakeDocker) *testDriver {
	t.Helper()
	cfg := &core.Config{AuthKey: "tskey-auth-plugin", DataDir: t.TempDir()}
	td := &testDriver{Driver: newDriver(cfg, fake), fake: fake, runs: make(chan tsRun, 16)}
	td.hostIsolation = func(bool) error { return nil }
	release := make(chan struct{})

	// Nothing else reads the sandbox key while Join or recovery runs: events
	// are sent by the tests only once these returned
	td.joinEndpoint = func(e *core.Endpoint, sandboxKey string) (*network.JoinResponse, error) {
		e.SandboxKey = sandboxKey
		return &network.JoinResponse{}, nil
	}
	td.recoverRouting = func(e *core.Endpoint, sandboxKey string) error {
		if td.recoverErr != nil {
			return td.recoverErr
		}
		e.SandboxKey = sandboxKey
		return nil
	}
	td.runTailscale = func(e *core.Endpoint, info *core.ContainerInfo) {
		buf := make([]byte, 512)
		header, _, _ := strings.Cut(string(buf[:runtime.Stack(buf, false)]), "\n")
		td.runs <- tsRun{e.ID, info, header}
		<-release
	}

	t.Cleanup(func() {
		td.cancel()
		close(release)
		td.wg.Wait()
	})
	return td
}

// watch runs the event watcher with the driver's callbacks, and reports on
// handled each container info the driver has handled.
func (td *testDriver) watch(handled chan<- string, stopped chan<- string) {
	td.wg.Go(func() {
		WatchEvents(td.ctx, td.docker, td.cache, td.ownsNetwork,
			func(endpointID string, info *core.ContainerInfo) {
				td.onContainerInfo(endpointID, info)
				if handled != nil {
					handled <- endpointID
				}
			},
			func(containerID string) {
				td.onContainerStop(containerID)
				if stopped != nil {
					stopped <- containerID
				}
			})
	})
}

// nextRun returns the next start of Tailscale, failing after a second.
func (td *testDriver) nextRun(t *testing.T) tsRun {
	t.Helper()
	select {
	case r := <-td.runs:
		return r
	case <-time.After(time.Second):
		t.Fatal("Tailscale was not started")
		return tsRun{}
	}
}

// noRun fails if Tailscale is started within 50 ms: runs start in the
// background.
func (td *testDriver) noRun(t *testing.T) {
	t.Helper()
	select {
	case r := <-td.runs:
		t.Fatalf("Tailscale started for endpoint %s (%+v)", r.endpointID[:12], r.info)
	case <-time.After(50 * time.Millisecond):
	}
}

// createNetwork creates a network through the driver with the given options.
func (td *testDriver) createNetwork(t *testing.T, id string, opts map[string]string) {
	t.Helper()
	generic := make(map[string]any, len(opts))
	for k, v := range opts {
		generic[k] = v
	}
	req := &network.CreateNetworkRequest{
		NetworkID: id,
		Options:   map[string]any{core.GenericOptionsKey: generic},
	}
	if err := td.CreateNetwork(req); err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
}

// join creates an endpoint on the network and joins it.
func (td *testDriver) join(t *testing.T, netID, endpointID, sandboxKey string) {
	t.Helper()
	if _, err := td.CreateEndpoint(
		&network.CreateEndpointRequest{NetworkID: netID, EndpointID: endpointID},
	); err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if _, err := td.Join(
		&network.JoinRequest{NetworkID: netID, EndpointID: endpointID, SandboxKey: sandboxKey},
	); err != nil {
		t.Fatalf("Join: %v", err)
	}
}

func (td *testDriver) endpoint(id string) (*core.Endpoint, bool) {
	td.mu.RLock()
	defer td.mu.RUnlock()
	ep, ok := td.endpoints[id]
	return ep, ok
}

func (td *testDriver) network(id string) (*core.Network, bool) {
	td.mu.RLock()
	defer td.mu.RUnlock()
	n, ok := td.networks[id]
	return n, ok
}

// connectEvent is the event Docker sends when a container joins a network.
func connectEvent(netID, netName, containerID string) events.Message {
	return events.Message{
		Type:   events.NetworkEventType,
		Action: events.ActionConnect,
		Actor: events.Actor{
			ID: netID,
			Attributes: map[string]string{
				"container": containerID,
				"name":      netName,
				"type":      pluginName,
			},
		},
	}
}

// killEvent is the event Docker sends when it signals a container.
func killEvent(containerID, signal string) events.Message {
	return events.Message{
		Type:   events.ContainerEventType,
		Action: events.ActionKill,
		Actor:  events.Actor{ID: containerID, Attributes: map[string]string{"signal": signal}},
	}
}

// send delivers an event to the watcher, failing after three seconds: it
// waits a second before reconnecting.
func (f *fakeDocker) send(t *testing.T, msg events.Message) {
	t.Helper()
	select {
	case f.events <- msg:
	case <-time.After(3 * time.Second):
		t.Fatal("event watcher did not take the event")
	}
}

// receive returns the next value from ch, failing after three seconds: the
// driver retries recovery every second.
func receive[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}
