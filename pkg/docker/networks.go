package docker

import (
	"context"
	"fmt"
	"time"

	dockerclient "github.com/moby/moby/client"

	"github.com/aaomidi/tslink/pkg/core"
	"github.com/aaomidi/tslink/pkg/logger"
)

// networkInspectTimeout bounds the Docker API call that rebuilds a network.
const networkInspectTimeout = 10 * time.Second

// networkFromInspect rebuilds a network from the options Docker stored when
// it was created. It is the only place that builds one from inspect data.
func networkFromInspect(res dockerclient.NetworkInspectResult, cfg *core.Config) (*core.Network, error) {
	// As in CreateNetwork
	opts := make(map[string]any, len(res.Network.Options))
	for k, v := range res.Network.Options {
		opts[k] = v
	}
	net, err := core.NewNetwork(res.Network.ID, core.ParseNetworkOptions(opts), cfg)
	if err != nil {
		return nil, fmt.Errorf("network %s: %w", res.Network.ID, err)
	}
	return net, nil
}

// adoptNetwork stores the network rebuilt from an inspect result, unless the
// driver already has it, and returns the network the driver keeps.
func (d *Driver) adoptNetwork(res dockerclient.NetworkInspectResult) (*core.Network, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if net, ok := d.networks[res.Network.ID]; ok {
		return net, nil
	}
	net, err := networkFromInspect(res, d.config)
	if err != nil {
		return nil, err
	}
	d.networks[net.ID] = net
	logger.Infof("Recovered network %s", net.ID)
	return net, nil
}

// networkFor returns the network with the given ID. The driver keeps networks
// in memory only, so after a plugin restart it rebuilds one it has not seen
// since, such as a network without containers.
func (d *Driver) networkFor(ctx context.Context, id string) (*core.Network, error) {
	d.mu.RLock()
	net, ok := d.networks[id]
	d.mu.RUnlock()
	if ok {
		return net, nil
	}

	// Outside the lock: the Docker API can take a while
	ctx, cancel := context.WithTimeout(ctx, networkInspectTimeout)
	defer cancel()
	res, err := d.docker.NetworkInspect(ctx, id, dockerclient.NetworkInspectOptions{})
	if err != nil {
		return nil, fmt.Errorf("network %s not found: %w", id, err)
	}
	return d.adoptNetwork(res)
}
