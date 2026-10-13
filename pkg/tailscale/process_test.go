package tailscale

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startFake starts script as tailscaled of d, with "$0" a directory for its
// marker files and args from "$1", and stops it when the test ends.
func startFake(t *testing.T, d *Daemon, script string, args ...string) string {
	t.Helper()
	dir := t.TempDir()
	args = append([]string{"-c", script, dir}, args...)
	cmd := exec.CommandContext(d.ctx, "sh", args...) // #nosec G204 -- the test's own script
	if err := d.startProcess(cmd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := d.Stop(); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return dir
}

// waitFile waits for path to exist.
func waitFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not appear", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Stop lets tailscaled shut down cleanly before it is killed: SIGTERM first,
// SIGKILL only if it is still running after stopGrace.
func TestStopTerminatesGracefully(t *testing.T) {
	setDuration(t, &stopGrace, 5*time.Second)
	d := newTestDaemon(t, &fakeCLI{}, "")
	dir := startFake(t, d, `trap 'touch "$0/term"; exit 0' TERM; touch "$0/ready"
while :; do sleep 0.05; done`)
	waitFile(t, filepath.Join(dir, "ready"))
	if err := d.Stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "term")); err != nil {
		t.Error("tailscaled got no SIGTERM before it was killed")
	}
}

// A tailscaled that does not exit on SIGTERM is killed after stopGrace.
func TestStopKillsAfterGrace(t *testing.T) {
	setDuration(t, &stopGrace, 200*time.Millisecond)
	d := newTestDaemon(t, &fakeCLI{}, "")
	dir := startFake(t, d, `trap '' TERM; touch "$0/ready"; while :; do sleep 0.05; done`)
	waitFile(t, filepath.Join(dir, "ready"))
	start := time.Now()
	if err := d.Stop(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("Stop took %v", took)
	}
	if d.cmd.ProcessState == nil {
		t.Error("tailscaled is still running")
	}
}

// Everything tailscaled wrote before it exited reaches its log, however far
// behind the reader is: its last lines tell why it exited.
func TestOutputKeptAfterExit(t *testing.T) {
	d := newTestDaemon(t, &fakeCLI{}, "")
	// The reader stops at the first line until the process has exited, with
	// more output than the reader buffers still in the pipe
	d.fetchMu.Lock()
	dir := startFake(
		t,
		d,
		`echo "$1"; i=0; while [ $i -lt 4000 ]; do echo "line $i"; i=$((i+1)); done
echo "last words" >&2; touch "$0/exiting"; exit 1`,
		servicesFetchLog,
	)
	waitFile(t, filepath.Join(dir, "exiting"))
	time.Sleep(200 * time.Millisecond) // for the exit to be seen
	d.fetchMu.Unlock()
	<-d.exited
	d.wg.Wait()
	log, err := os.ReadFile(filepath.Join(d.config.StateDir, "tailscaled.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"line 3999\n", "last words\n"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("tailscaled.log lacks %q", want)
		}
	}
}
