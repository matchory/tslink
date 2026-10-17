package core

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// The options of a tslink network, as docker network create --opt passes them.
const (
	AuthKeyOption     = "tslink.authkey"
	TagsOption        = "tslink.tags"
	EphemeralOption   = "tslink.ephemeral"   // for keys that do not say so themselves
	LoginServerOption = "tslink.loginserver" // a control server other than Tailscale's
	MTUOption         = "com.docker.network.driver.mtu"
)

// The container labels tslink reads.
const (
	HostnameLabel = "tslink.hostname"
	TagsLabel     = "tslink.tags"
	ServiceLabel  = "tslink.service"
	// ServiceLabelPrefix starts the labels tslink.service.<name>.serve.<port>.
	ServiceLabelPrefix = "tslink.service."
	DirectLabel        = "tslink.direct"
	// HealthLabel turns on the readiness endpoint on a loopback port of the
	// container's own, for a Docker healthcheck such as
	// wget -q -O- http://127.0.0.1:<port>/ready.
	HealthLabel = "tslink.health"
	// ServeLabelPrefix starts the labels tslink.serve.<port>.
	ServeLabelPrefix = "tslink.serve."
)

// KeyPrefix starts the keys of tslink's options and labels.
const KeyPrefix = "tslink."

// networkOptions sets each network option on NetworkOptions from its value.
var networkOptions = map[string]func(o *NetworkOptions, v string) error{
	AuthKeyOption: func(o *NetworkOptions, v string) error {
		o.AuthKey = v
		return nil
	},
	TagsOption: func(o *NetworkOptions, v string) error {
		o.Tags = ParseTags(v)
		return nil
	},
	// NewNetwork validates these two
	EphemeralOption: func(o *NetworkOptions, v string) error {
		o.Ephemeral = v
		return nil
	},
	LoginServerOption: func(o *NetworkOptions, v string) error {
		o.LoginServer = v
		return nil
	},
	MTUOption: func(o *NetworkOptions, v string) error {
		if o.MTU = ParseMTU(v); o.MTU == 0 {
			return fmt.Errorf("%s=%q: want 576 to 65535", MTUOption, v)
		}
		return nil
	},
}

// ParseNetworkOptions parses network creation options. Docker passes --opt
// values nested under "com.docker.network.generic". An option tslink does not
// know under its prefix, a value that is not a string or an invalid MTU is
// an error: a misspelt option would otherwise leave the network with another
// credential or other tags than meant. Docker's other options pass.
func ParseNetworkOptions(opts map[string]any) (NetworkOptions, error) {
	if nested, ok := opts[GenericOptionsKey].(map[string]any); ok {
		opts = nested
	}
	var options NetworkOptions
	var errs []error
	for _, key := range slices.Sorted(maps.Keys(opts)) {
		set, known := networkOptions[key]
		if !known {
			if strings.HasPrefix(key, KeyPrefix) {
				errs = append(errs, fmt.Errorf("unknown network option %s", key))
			}
			continue
		}
		v, ok := opts[key].(string)
		if !ok {
			errs = append(
				errs,
				fmt.Errorf("network option %s: want a string, not %T", key, opts[key]),
			)
			continue
		}
		errs = append(errs, set(&options, v))
	}
	return options, errors.Join(errs...)
}

// CheckEndpointOptions refuses endpoint options under tslink's prefix: a
// container's settings are its labels.
func CheckEndpointOptions(opts map[string]any) error {
	var errs []error
	for _, key := range slices.Sorted(maps.Keys(opts)) {
		if strings.HasPrefix(key, KeyPrefix) {
			errs = append(errs, fmt.Errorf(
				"unknown endpoint option %s: set it as a container label", key,
			))
		}
	}
	return errors.Join(errs...)
}

// ContainerName returns a container's name as Docker's API gives it, without
// the leading "/".
func ContainerName(name string) string {
	return strings.TrimPrefix(name, "/")
}
