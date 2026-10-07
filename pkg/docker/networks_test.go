package docker

import (
	"slices"
	"testing"

	dockerclient "github.com/moby/moby/client"

	"github.com/aaomidi/tslink/pkg/core"
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
		"tslink.authkey":       "hskey-auth-x",
		"tslink.tags":          "tag:a, tag:b",
		core.MTUOption:         "1450",
		core.LoginServerOption: "http://10.0.0.1:8080",
	}), &core.Config{AuthKey: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if net.ID != testNetworkID || net.AuthKey != "hskey-auth-x" || net.MTU != 1450 ||
		net.LoginServer != "http://10.0.0.1:8080" || !slices.Equal(net.Tags, []string{"tag:a", "tag:b"}) {
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
