//go:build linux

package netutil

import (
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// testHandle returns a netlink handle in the namespace at nsPath and its
// link ifName.
func testHandle(t *testing.T, nsPath, ifName string) (*netlink.Handle, netlink.Link) {
	t.Helper()
	h, err := handleAt(nsPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	link, err := h.LinkByName(ifName)
	if err != nil {
		t.Fatalf("%s in %s: %v", ifName, nsPath, err)
	}
	return h, link
}

// stubNetNSSeams replaces setNS by set and counts the threads unlocked,
// until the test ends.
func stubNetNSSeams(t *testing.T, set func(netns.NsHandle) error) *int {
	t.Helper()
	origSet, origUnlock := setNS, unlockOSThread
	t.Cleanup(func() { setNS, unlockOSThread = origSet, origUnlock })
	unlocked := new(int)
	setNS = set
	unlockOSThread = func() {
		*unlocked++
		origUnlock()
	}
	return unlocked
}

// A thread that cannot return to its original namespace must not go back to
// Go's pool: other goroutines would run in the container's namespace.
func TestInNetNSRestoreFailureKeepsThreadLocked(t *testing.T) {
	calls := 0
	unlocked := stubNetNSSeams(t, func(netns.NsHandle) error {
		calls++
		if calls == 2 { // The switch back
			return errors.New("injected")
		}
		return nil
	})

	ran := false
	err := inNetNS("/proc/self/ns/net", func() error {
		ran = true
		return nil
	})
	if !ran {
		t.Error("fn did not run")
	}
	if err == nil || !strings.Contains(err.Error(), "restore original netns") {
		t.Errorf("err = %v, want the failed restore", err)
	}
	if *unlocked != 0 {
		t.Errorf("thread unlocked %d times, want it to stay locked", *unlocked)
	}
}

func TestInNetNSUnlocksAfterRestore(t *testing.T) {
	calls := 0
	unlocked := stubNetNSSeams(t, func(netns.NsHandle) error {
		calls++
		return nil
	})
	want := errors.New("from fn")
	if err := inNetNS("/proc/self/ns/net", func() error { return want }); !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
	if calls != 2 || *unlocked != 1 {
		t.Errorf("setns %d times, unlocked %d times; want 2 and 1", calls, *unlocked)
	}
}

// fn may end its goroutine, as t.Fatal does: inNetNS must return.
func TestInNetNSGoexit(t *testing.T) {
	stubNetNSSeams(t, func(netns.NsHandle) error { return nil })
	if err := inNetNS("/proc/self/ns/net", func() error {
		runtime.Goexit()
		return nil
	}); err == nil {
		t.Error("inNetNS returned nil for a function that did not return")
	}
}
