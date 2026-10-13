package netutil

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"

	"github.com/vishvananda/netlink"

	"github.com/matchory/tslink/pkg/logger"
)

// Tailscale's address ranges. Traffic to these must leave through the
// container's own tailscaled (table 52) or not at all; otherwise it falls
// through the default route and out of the host's tailscaled, with the
// host's identity.
var (
	tailnetV4 = &net.IPNet{IP: net.IPv4(100, 64, 0, 0).To4(), Mask: net.CIDRMask(10, 32)}
	tailnetV6 = &net.IPNet{IP: net.ParseIP("fd7a:115c:a1e0::"), Mask: net.CIDRMask(48, 128)}
)

// setupTailnetBlackhole installs unreachable routes for the tailnet ranges in
// the main table of h's namespace. tailscaled's policy rules consult table 52
// first, so peers it can reach still route via tailscale0.
//
// The IPv4 route is mandatory. The IPv6 route may fail when IPv6 is disabled
// in the namespace; that is accepted only if the namespace has no route to
// the IPv6 range anyway.
func setupTailnetBlackhole(h *netlink.Handle) error {
	if err := h.RouteReplace(unreachableRoute(tailnetV4)); err != nil {
		return fmt.Errorf("failed to add unreachable route %s: %w", tailnetV4, err)
	}

	if err := h.RouteReplace(unreachableRoute(tailnetV6)); err != nil {
		if _, getErr := h.RouteGet(tailnetV6.IP); getErr == nil {
			return fmt.Errorf("failed to add unreachable route %s: %w", tailnetV6, err)
		}
		logger.Warnf(
			"Skipping unreachable route %s, namespace has no IPv6 route: %v",
			tailnetV6,
			err,
		)
	}
	return nil
}

// CleanupTailnetBlackhole removes the routes added by setupTailnetBlackhole.
// A namespace or route that is already gone is not an error.
//
// Note that a container that stays attached to another network regains a
// path to the tailnet ranges through the host once these routes are removed.
func CleanupTailnetBlackhole(nsPath string) error {
	h, err := handleAt(nsPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer h.Close()
	var errs []error
	for _, dst := range []*net.IPNet{tailnetV4, tailnetV6} {
		if err := h.RouteDel(unreachableRoute(dst)); err != nil && !errors.Is(err, syscall.ESRCH) {
			errs = append(errs, fmt.Errorf("failed to delete unreachable route %s: %w", dst, err))
		}
	}
	return errors.Join(errs...)
}

// rtnUnreachable is unix.RTN_UNREACHABLE, which x/sys defines only on Linux;
// spelled out so the package still builds on other platforms.
const rtnUnreachable = 7

func unreachableRoute(dst *net.IPNet) *netlink.Route {
	return &netlink.Route{Dst: dst, Type: rtnUnreachable}
}
