package tailscale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/moby/moby/client/pkg/stringid"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/tailcfg/nodecap"
	"tailscale.com/types/ipproto"

	"github.com/matchory/tslink/pkg/logger"
)

// Control routes a Service's traffic only to hosts that serve every port of
// its definition: it reports a host that serves some of them as partially
// configured, and sends it nothing. tslink compares the ports the host serves
// with the definition's, where it can read them.

// portsWarnEvery is how often the log repeats that a host does not serve all
// ports of its Service.
const portsWarnEvery = time.Hour

// definedPorts returns the ports of the Service's definition, and whether
// they are known: from the node's capabilities, which describe the Services
// visible to it, or else as read from Tailscale's API.
func (b *serviceBackend) definedPorts(status *ipnstate.Status) ([]tailcfg.ProtoPortRange, bool) {
	if status.Self != nil {
		for capability, values := range status.Self.CapMap {
			if !strings.HasPrefix(string(capability), string(nodecap.ServicesPrefix)) {
				continue
			}
			for _, raw := range values {
				var details tailcfg.ServiceDetails
				if json.Unmarshal([]byte(raw), &details) == nil &&
					string(details.Name) == b.name && len(details.Ports) > 0 {
					return details.Ports, true
				}
			}
		}
	}
	if ports := b.apiPorts.Load(); ports != nil {
		return *ports, true
	}
	return nil, false
}

// uncoveredPorts returns the ports of the Service's definition that config
// does not serve for it, and whether the definition's ports are known.
func (b *serviceBackend) uncoveredPorts(
	status *ipnstate.Status,
	config *ipn.ServeConfig,
) ([]string, bool) {
	defined, known := b.definedPorts(status)
	if !known {
		return nil, false
	}
	var svc *ipn.ServiceConfig
	if config != nil {
		svc = config.Services[tailcfg.ServiceName(b.name)]
	}
	if svc != nil && svc.Tun {
		return nil, true // all its traffic goes to the node
	}
	var uncovered []string
	for _, ports := range defined {
		if !covered(svc, ports) {
			uncovered = append(uncovered, ports.String())
		}
	}
	return uncovered, true
}

// covered reports whether svc serves every port of ports without forwarding
// all traffic: TCP ports only, which a definition for any protocol counts as.
func covered(svc *ipn.ServiceConfig, ports tailcfg.ProtoPortRange) bool {
	if svc == nil || (ports.Proto != 0 && ports.Proto != int(ipproto.TCP)) ||
		ports.Ports.First > ports.Ports.Last {
		return false
	}
	for port := ports.Ports.First; ; port++ {
		if svc.TCP[port] == nil {
			return false
		}
		if port == ports.Ports.Last {
			return true
		}
	}
}

// notePorts warns in the endpoint's status about the ports of the Service's
// definition the node does not serve, or clears the warning if there are
// none.
func (b *serviceBackend) notePorts(uncovered []string) {
	key := "ports/" + b.name
	if len(uncovered) == 0 {
		b.d.warn(key, "", 0)
		return
	}
	b.d.warn(key, fmt.Sprintf(
		"%s defines %s, which endpoint %s does not serve: control routes no traffic to a host "+
			"that does not serve every port of the Service. Serve them as well, or remove them "+
			"from the Service's definition",
		b.name, strings.Join(uncovered, ", "), stringid.TruncateID(b.d.config.EndpointID),
	), portsWarnEvery)
}

// refreshDefinedPorts reads the ports of the Service's definition from
// Tailscale's API, if the credential is an OAuth client secret for Tailscale's
// control server: the node's capabilities may not describe its own Service.
// A definition that lists no ports, or does not have control validate them,
// leaves them unknown. A failed read, as without the services:read scope,
// keeps what was read before, and is logged once per backend.
func (b *serviceBackend) refreshDefinedPorts() {
	cfg := b.d.config
	if cfg.AuthKey == nil || (cfg.LoginServer != "" && cfg.LoginServer != defaultControlURL) {
		return
	}
	secret, err := cfg.AuthKey()
	if err == nil {
		ctx, cancel := context.WithTimeout(b.d.ctx, 10*time.Second)
		defer cancel()
		var ports []tailcfg.ProtoPortRange
		if ports, err = servicePorts(ctx, secret, b.name); err == nil {
			if ports == nil {
				b.apiPorts.Store(nil)
			} else {
				b.apiPorts.Store(&ports)
			}
			return
		}
	}
	if errors.Is(err, errNotOAuthClient) || b.d.ctx.Err() != nil ||
		!b.apiPortsLogged.CompareAndSwap(false, true) {
		return
	}
	logger.Infof(
		"Cannot read the ports of %s from Tailscale's API, so endpoint %s does not check that "+
			"it serves them all (an OAuth client needs the services:read scope): %v",
		b.name, stringid.TruncateID(cfg.EndpointID), err,
	)
}
