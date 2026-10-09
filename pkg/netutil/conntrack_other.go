//go:build !linux

package netutil

import (
	"errors"
	"net/netip"
)

// forgetUDPFlows needs Linux conntrack.
func forgetUDPFlows(_ netip.Addr, _ int) (uint, error) {
	return 0, errors.New("conntrack requires Linux")
}
