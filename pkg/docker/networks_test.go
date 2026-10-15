package docker

import (
	"slices"
	"strings"
	"testing"

	"github.com/docker/go-plugins-helpers/network"
	dockerclient "github.com/moby/moby/client"

	"github.com/matchory/tslink/pkg/core"
)

const testNetworkID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func inspectResult(opts map[string]string) dockerclient.NetworkInspectResult {
	var res dockerclient.NetworkInspectResult
	res.Network.ID = testNetworkID
	res.Network.Options = opts
	return res
}

func TestNetworkFromInspect(t *testing.T) {
	net, err := networkFromInspect(inspectResult(map[string]string{
		"tslink.authkey": "hskey-auth-x",
		"tslink.tags":    "tag:a, tag:b",
		core.MTUOption:   "1450",
	}), &core.Config{AuthKey: "default", LoginServer: "http://10.0.0.1:8080"})
	if err != nil {
		t.Fatal(err)
	}
	if net.ID != testNetworkID || net.AuthKey != "hskey-auth-x" || net.MTU != 1450 ||
		net.LoginServer != "http://10.0.0.1:8080" || net.Refused() != nil ||
		!slices.Equal(net.Tags, []string{"tag:a", "tag:b"}) {
		t.Errorf("rebuilt network %+v", net)
	}
}

func TestNetworkFromInspectAuthKey(t *testing.T) {
	net, err := networkFromInspect(inspectResult(nil), &core.Config{AuthKey: "default"})
	if err != nil || net.AuthKey != "default" {
		t.Errorf("without tslink.authkey: %+v, %v; want the plugin's key", net, err)
	}
	if _, err := networkFromInspect(inspectResult(nil), &core.Config{}); err == nil {
		t.Error("no auth key at all: want an error")
	}
}

func TestAdoptNetworkKeepsExisting(t *testing.T) {
	existing := &core.Network{ID: testNetworkID, AuthKey: "kept"}
	d := &Driver{
		networks: map[string]*core.Network{testNetworkID: existing},
		config:   &core.Config{},
	}
	net, err := d.adoptNetwork(inspectResult(map[string]string{"tslink.authkey": "other"}))
	if err != nil || net != existing {
		t.Errorf("adoptNetwork = %+v, %v; want the network the driver has", net, err)
	}

	d = &Driver{networks: map[string]*core.Network{}, config: &core.Config{}}
	net, err = d.adoptNetwork(inspectResult(map[string]string{"tslink.authkey": "k"}))
	if err != nil || d.networks[testNetworkID] != net {
		t.Errorf("adoptNetwork did not store the rebuilt network: %+v, %v", net, err)
	}
}

// networkFor must not need Docker for a network the driver has.
func TestNetworkForKnown(t *testing.T) {
	existing := &core.Network{ID: testNetworkID}
	d := &Driver{networks: map[string]*core.Network{testNetworkID: existing}}
	if net, err := d.networkFor(t.Context(), testNetworkID); err != nil || net != existing {
		t.Errorf("networkFor = %+v, %v", net, err)
	}
}

// A network created with tslink.loginserver before the option was removed is
// rebuilt from Docker, but no endpoint joins it: none sends its key to the
// server it names, nor, with what may be a key of that server, to the
// plugin's.
//
// Guards: G4
func TestJoinRefusesNetworkWithLoginServer(t *testing.T) {
	fake := newFakeDocker()
	netID := fake.addNetwork("legacy", pluginName, map[string]string{
		"tslink.authkey": "hskey-auth-x", core.LoginServerOption: "https://control.example",
	}, nil)
	td := newTestDriver(t, fake)
	endpointID := fakeID("endpoint")
	if _, err := td.CreateEndpoint(
		&network.CreateEndpointRequest{NetworkID: netID, EndpointID: endpointID},
	); err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	_, err := td.Join(
		&network.JoinRequest{NetworkID: netID, EndpointID: endpointID, SandboxKey: sandbox},
	)
	if err == nil || !strings.Contains(err.Error(), core.LoginServerSetting) {
		t.Errorf("Join = %v, want an error naming %s", err, core.LoginServerSetting)
	}
	if ep, ok := td.endpoint(endpointID); ok && ep.SandboxKey() != "" {
		t.Error("the endpoint joined")
	}
	td.noRun(t)
}
