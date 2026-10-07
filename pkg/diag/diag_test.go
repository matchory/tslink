package diag

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunFindsStateDirsAndSockets(t *testing.T) {
	// The test's sockets never answer
	statusTimeout = 200 * time.Millisecond
	t.Cleanup(func() { statusTimeout = 5 * time.Second })

	// Short path: Unix socket paths are limited to about 100 bytes.
	//nolint:usetesting // t.TempDir is too long for a socket path
	dataDir, err := os.MkdirTemp("/tmp", "diag")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dataDir) })

	for _, dir := range []string{
		"by-hostname/web",
		"by-stack/billing/billing-api-1-abc",
		"tailscale-bin/1.0.0",
		"sock",
	} {
		if err := os.MkdirAll(filepath.Join(dataDir, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(
		filepath.Join(dataDir, "by-stack/billing/billing-api-1-abc/tailscaled.state"),
		[]byte("{}"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	listen := func(id string) {
		l, err := net.Listen("unix", filepath.Join(dataDir, "sock", id+".sock"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
	}
	link := func(stateDir, id string) {
		target := filepath.Join(
			strings.Repeat("../", strings.Count(stateDir, "/")+1),
			"sock",
			id+".sock",
		)
		if err := os.Symlink(
			target,
			filepath.Join(dataDir, stateDir, "tailscaled.sock"),
		); err != nil {
			t.Fatal(err)
		}
	}
	listen("aaaaaaaaaaaa")
	link("by-hostname/web", "aaaaaaaaaaaa")            // running
	link("by-stack/billing/billing-api-1-abc", "dead") // stopped: dangling link
	listen("bbbbbbbbbbbb")                             // running, no state dir links to it

	var out bytes.Buffer
	if err := Run(dataDir, &out); err != nil {
		t.Fatal(err)
	}
	report := out.String()

	// The sockets never answer a status query, so querying one is an error.
	for _, want := range []string{
		"Total: 3 | Online: 0 | Offline: 1 | Errors: 2",
		"--- by-hostname/web ---\n  State dir: " + dataDir + "/by-hostname/web\n  Socket:    true",
		"--- by-stack/billing/billing-api-1-abc ---\n  State dir: " + dataDir + "/by-stack/billing/billing-api-1-abc\n  Socket:    false\n  State:     true",
		"--- socket bbbbbbbbbbbb ---",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, "aaaaaaaaaaaa ---") || strings.Contains(report, "tailscale-bin") {
		t.Errorf("report lists a linked socket or the binary cache separately:\n%s", report)
	}
}

func TestRunReportsEndpointsNotRunning(t *testing.T) {
	// A task whose Tailscale never starts has no state directory or socket;
	// only its status file shows it.
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "status"), 0o700); err != nil {
		t.Fatal(err)
	}
	for id, body := range map[string]string{
		"aaaaaaaaaaaa": `{"endpoint":"aaaaaaaaaaaa","hostname":"web","stack":"app","state":"retrying","error":"control plane unreachable","attempts":3}`,
		"bbbbbbbbbbbb": `{"endpoint":"bbbbbbbbbbbb","hostname":"api","state":"running"}`,
	} {
		if err := os.WriteFile(
			filepath.Join(dataDir, "status", id+".json"),
			[]byte(body),
			0o600,
		); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := Run(dataDir, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"Errors: 1", "app/web", "retrying", "control plane unreachable", "attempt 3"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "api") {
		t.Errorf("running endpoint reported as not running:\n%s", got)
	}
}
