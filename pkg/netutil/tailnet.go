package netutil

import (
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"syscall"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	"github.com/aaomidi/tslink/pkg/logger"
)

// Tailscale's address ranges. Traffic to these must leave through the
// container's own tailscaled (table 52) or not at all; otherwise it falls
// through the default route and out of the host's tailscaled, with the
// host's identity.
var (
	tailnetV4 = &net.IPNet{IP: net.IPv4(100, 64, 0, 0).To4(), Mask: net.CIDRMask(10, 32)}
	tailnetV6 = &net.IPNet{IP: net.ParseIP("fd7a:115c:a1e0::"), Mask: net.CIDRMask(48, 128)}
)

// SetupTailnetBlackhole installs unreachable routes for the tailnet ranges in
// the main table of the container namespace. tailscaled's policy rules consult
// table 52 first, so peers it can reach still route via tailscale0.
//
// The IPv4 route is mandatory. The IPv6 route may fail when IPv6 is disabled
// in the namespace; that is accepted only if the namespace has no route to
// the IPv6 range anyway.
func SetupTailnetBlackhole(nsPath string) error {
	return inNetNS(nsPath, func() error {
		if err := netlink.RouteReplace(unreachableRoute(tailnetV4)); err != nil {
			return fmt.Errorf("failed to add unreachable route %s: %w", tailnetV4, err)
		}

		if err := netlink.RouteReplace(unreachableRoute(tailnetV6)); err != nil {
			if _, getErr := netlink.RouteGet(tailnetV6.IP); getErr == nil {
				return fmt.Errorf("failed to add unreachable route %s: %w", tailnetV6, err)
			}
			logger.Warnf(
				"Skipping unreachable route %s, namespace has no IPv6 route: %v",
				tailnetV6,
				err,
			)
		}

		logger.Debugf("Tailnet ranges blackholed in %s", nsPath)
		return nil
	})
}

// CleanupTailnetBlackhole removes the routes added by SetupTailnetBlackhole.
// A namespace or route that is already gone is not an error.
//
// Note that a container that stays attached to another network regains a
// path to the tailnet ranges through the host once these routes are removed.
func CleanupTailnetBlackhole(nsPath string) error {
	err := inNetNS(nsPath, func() error {
		var errs []error
		for _, dst := range []*net.IPNet{tailnetV4, tailnetV6} {
			if err := netlink.RouteDel(
				unreachableRoute(dst),
			); err != nil &&
				!errors.Is(err, syscall.ESRCH) {
				errs = append(
					errs,
					fmt.Errorf("failed to delete unreachable route %s: %w", dst, err),
				)
			}
		}
		return errors.Join(errs...)
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// rtnUnreachable is unix.RTN_UNREACHABLE, which x/sys defines only on Linux;
// spelled out so the package still builds on other platforms.
const rtnUnreachable = 7

func unreachableRoute(dst *net.IPNet) *netlink.Route {
	return &netlink.Route{Dst: dst, Type: rtnUnreachable}
}

// inNetNS runs fn with the calling thread switched into the namespace at nsPath.
func inNetNS(nsPath string, fn func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	origNS, err := netns.Get()
	if err != nil {
		return fmt.Errorf("failed to get current netns: %w", err)
	}
	defer origNS.Close()

	targetNS, err := netns.GetFromPath(nsPath)
	if err != nil {
		return fmt.Errorf("failed to get target netns: %w", err)
	}
	defer targetNS.Close()

	if err := netns.Set(targetNS); err != nil {
		return fmt.Errorf("failed to switch to target netns: %w", err)
	}
	defer func() {
		if err := netns.Set(origNS); err != nil {
			logger.Warnf("failed to restore original netns: %v", err)
		}
	}()

	return fn()
}
