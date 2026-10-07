//go:build linux

package netutil

import (
	"errors"
	"fmt"
	"net"
	"syscall"

	"github.com/vishvananda/netlink"
)

// tailscaled marks its own sockets (control, DERP, WireGuard) with this mark
// and routes them by policy rules from priority 5210 on.
const (
	tailscaledBypassMark = 0x80000
	tailscaledMarkMask   = 0xff0000
)

// bypassTable and bypassPriority route tailscaled's own traffic ahead of
// tailscaled's rules.
const (
	bypassTable    = 5200
	bypassPriority = 5200
)

// SetupBypassRoute sends tailscaled's own traffic through the tslink veth,
// whatever the container's default route. A task on an overlay network
// defaults via docker_gwbridge, and Docker may detach that network before
// tslink's: tailscaled would then lose the control plane before it can log
// out, and the device would linger.
func SetupBypassRoute(nsPath, ifName, gatewayIP string) error {
	gw := net.ParseIP(gatewayIP)
	if gw == nil {
		return fmt.Errorf("failed to parse gateway IP: %s", gatewayIP)
	}
	return inNetNS(nsPath, func() error {
		link, err := netlink.LinkByName(ifName)
		if err != nil {
			return fmt.Errorf("interface %s not found: %w", ifName, err)
		}
		route := &netlink.Route{LinkIndex: link.Attrs().Index, Gw: gw, Table: bypassTable}
		if err := netlink.RouteReplace(route); err != nil {
			return fmt.Errorf("failed to add route to table %d: %w", bypassTable, err)
		}

		rule := netlink.NewRule()
		rule.Priority = bypassPriority
		rule.Table = bypassTable
		rule.Mark = tailscaledBypassMark
		mask := uint32(tailscaledMarkMask)
		rule.Mask = &mask
		if err := netlink.RuleAdd(rule); err != nil && !errors.Is(err, syscall.EEXIST) {
			return fmt.Errorf("failed to add rule for table %d: %w", bypassTable, err)
		}
		return nil
	})
}
