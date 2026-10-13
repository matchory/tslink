package netutil

import (
	"errors"
	"fmt"
	"runtime"
	"strings"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// Seams for tests: the setns call and the release of a locked thread.
var (
	setNS          = netns.Set
	unlockOSThread = runtime.UnlockOSThread
)

// dockerNetNSDirs are the two spellings of Docker's namespace directory. On
// a host where /var/run is not a link to /run, Docker may pass one while the
// namespace is under the other.
var dockerNetNSDirs = [2]string{"/var/run/docker/netns/", "/run/docker/netns/"}

// alternateNetNSPath returns the other spelling of a path in Docker's
// namespace directory, or "" for any other path.
func alternateNetNSPath(nsPath string) string {
	for i, dir := range dockerNetNSDirs {
		if rest, ok := strings.CutPrefix(nsPath, dir); ok {
			return dockerNetNSDirs[1-i] + rest
		}
	}
	return ""
}

// openNetNS opens the network namespace at nsPath, or at its alternate
// spelling in Docker's namespace directory if nsPath fails. It returns the
// path it opened.
func openNetNS(nsPath string) (netns.NsHandle, string, error) {
	ns, err := netns.GetFromPath(nsPath)
	if err == nil {
		return ns, nsPath, nil
	}
	err = fmt.Errorf("failed to open netns %s: %w", nsPath, err)
	alt := alternateNetNSPath(nsPath)
	if alt == "" {
		return netns.None(), "", err
	}
	ns, altErr := netns.GetFromPath(alt)
	if altErr != nil {
		return netns.None(), "", errors.Join(
			err,
			fmt.Errorf("failed to open netns %s: %w", alt, altErr),
		)
	}
	return ns, alt, nil
}

// inNetNS runs fn on an OS thread switched into the network namespace at
// nsPath, for what needs the thread there, such as creating a socket. The
// thread belongs to a goroutine of its own: if it cannot return to its
// original namespace, it stays locked, and Go terminates it when the
// goroutine exits instead of running other goroutines on it.
func inNetNS(nsPath string, fn func() error) error {
	target, _, err := openNetNS(nsPath)
	if err != nil {
		return err
	}
	defer target.Close()

	done := make(chan error, 1)
	go func() {
		// fn may end the goroutine without returning, like t.Fatal
		err := errors.New("function in netns did not return")
		defer func() { done <- err }()
		runtime.LockOSThread()
		err = runInNetNS(target, fn)
	}()
	return <-done
}

// runInNetNS switches the locked calling thread into target, runs fn and
// switches back. It unlocks the thread only if it is back in its original
// namespace.
func runInNetNS(target netns.NsHandle, fn func() error) (err error) {
	orig, err := netns.Get()
	if err != nil {
		unlockOSThread()
		return fmt.Errorf("failed to get current netns: %w", err)
	}
	defer orig.Close()

	if err := setNS(target); err != nil {
		unlockOSThread()
		return fmt.Errorf("failed to switch to target netns: %w", err)
	}
	defer func() {
		if restoreErr := setNS(orig); restoreErr != nil {
			// The thread stays locked, so Go terminates it
			err = errors.Join(err, fmt.Errorf("failed to restore original netns: %w", restoreErr))
			return
		}
		unlockOSThread()
	}()
	return fn()
}

// handleAt returns a netlink handle in the network namespace at nsPath. Its
// sockets belong to that namespace, so it works from any thread.
func handleAt(nsPath string) (*netlink.Handle, error) {
	var h *netlink.Handle
	err := inNetNS(nsPath, func() error {
		var err error
		h, err = netlink.NewHandle()
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open netlink in %s: %w", nsPath, err)
	}
	return h, nil
}
