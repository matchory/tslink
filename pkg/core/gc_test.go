package core

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aaomidi/tslink/pkg/tailscale"
)

func TestCollectGarbage(t *testing.T) {
	data := t.TempDir()
	old := time.Now().Add(-time.Hour)
	mkdir := func(rel string, ephemeral bool, mtime time.Time) string {
		t.Helper()
		dir := filepath.Join(data, rel)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if ephemeral {
			if err := tailscale.MarkEphemeral(dir); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chtimes(dir, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	sock := func(name string, mtime time.Time) string {
		t.Helper()
		p := filepath.Join(data, "sock", name)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return p
	}

	dead := mkdir("by-stack/app/app_web.1.dead", true, old)
	inUse := mkdir("by-stack/app/app_web.2.live", true, old)
	fresh := mkdir("by-stack/app/app_web.3.new", true, time.Now())
	kept := mkdir("by-hostname/db", false, old) // not ephemeral: identity reused
	deadHost := mkdir("by-hostname/tmp", true, old)
	deadSock := sock("aaaaaaaaaaaa.sock", old)
	liveSock := sock("bbbbbbbbbbbb.sock", old)
	freshSock := sock("cccccccccccc.sock", time.Now())
	deadStatus := sock("../status/aaaaaaaaaaaa.json", old)
	liveStatus := sock("../status/bbbbbbbbbbbb.json", old)

	CollectGarbage(data, map[string]bool{inUse: true}, map[string]bool{"bbbbbbbbbbbb": true}, time.Minute)

	for _, p := range []string{dead, deadHost, deadSock, deadStatus} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s should be removed (err=%v)", p, err)
		}
	}
	for _, p := range []string{inUse, fresh, kept, liveSock, freshSock, liveStatus} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s should be kept: %v", p, err)
		}
	}
}

func TestClaimStateDir(t *testing.T) {
	// Recovery must claim the directory before garbage collection runs; the
	// start that would claim it otherwise runs in the background.
	e := &Endpoint{ID: "0123456789abcdef", DataDir: "/data"}
	e.ClaimStateDir(&ContainerInfo{Hostname: "app_web.1.abc", Stack: "app"})
	if got := e.GetStateDir(); got != "/data/by-stack/app/app_web.1.abc" {
		t.Errorf("StateDir = %q", got)
	}
	e.ClaimStateDir(&ContainerInfo{Hostname: "../x"})
	if got := e.GetStateDir(); got != "/data/by-stack/app/app_web.1.abc" {
		t.Errorf("invalid hostname changed StateDir to %q", got)
	}
}

func TestLeaveRemovesStateOfFailedStart(t *testing.T) {
	// A task whose Tailscale never started still claimed and created its
	// state directory; with an ephemeral key nothing will reuse it.
	data := t.TempDir()
	e := &Endpoint{
		ID:      "0123456789abcdef",
		DataDir: data,
		Network: &Network{AuthKey: "tskey-client-x?ephemeral=true"},
	}
	e.ClaimStateDir(&ContainerInfo{Hostname: "app_web.1.abc", Stack: "app"})
	dir := e.GetStateDir()
	if err := tailscale.MarkEphemeral(dir); err != nil {
		t.Fatal(err)
	}
	if err := e.Leave(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("%s should be removed (err=%v)", dir, err)
	}
}

func TestStateDirClaimedOnce(t *testing.T) {
	// Two replicas with the same tslink.hostname on one node would share a
	// tailscaled.state, and with it a node key.
	data := t.TempDir()
	info := &ContainerInfo{Hostname: "dup", Stack: "app"}
	a := &Endpoint{ID: "aaaaaaaaaaaaaaaa", DataDir: data, Network: &Network{}}
	b := &Endpoint{ID: "bbbbbbbbbbbbbbbb", DataDir: data, Network: &Network{}}
	if err := a.ClaimStateDir(info); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := a.ClaimStateDir(info); err != nil {
		t.Errorf("claiming again for the same endpoint: %v", err)
	}
	if err := b.ClaimStateDir(info); err == nil {
		t.Fatal("second endpoint claimed a directory in use")
	}
	if err := a.Leave(); err != nil {
		t.Fatal(err)
	}
	if err := b.ClaimStateDir(info); err != nil {
		t.Errorf("claim after the first endpoint left: %v", err)
	}
	if err := b.Leave(); err != nil {
		t.Fatal(err)
	}
}
