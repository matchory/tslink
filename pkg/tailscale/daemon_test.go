package tailscale

import (
	"errors"
	"os/exec"
	"slices"
	"strconv"
	"testing"

	"tailscale.com/ipn"
)

// tailscaled exits by itself after "tailscale down" or a logout: killing it
// then is no failure.
func TestKillErrorIgnoresExitedProcess(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if err := killError(cmd.Process.Kill()); err != nil {
		t.Errorf("killing an exited process: %v, want nil", err)
	}
	if err := killError(errors.New("operation not permitted")); err == nil {
		t.Error("other kill errors must be reported")
	}
}

// Stopping takes the node down before killing tailscaled, and kills it even
// if that fails, as it does when tailscaled has crashed.
func TestKillProcessTakesNodeDown(t *testing.T) {
	for _, crashed := range []bool{false, true} {
		d := newTestDaemon(t, &fakeCLI{}, "")
		api := newFakeLocalAPI(t)
		api.loggedIn(ipn.Prefs{WantRunning: true})
		if !crashed {
			d.lc = api.client()
		}
		cmd := exec.Command("sleep", "30")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		d.killProcess(cmd)
		api.mu.Lock()
		running := api.prefs.WantRunning
		api.mu.Unlock()
		if running == !crashed {
			t.Errorf("crashed %v: WantRunning = %v", crashed, running)
		}
		if cmd.ProcessState == nil {
			t.Errorf("crashed %v: tailscaled was not killed", crashed)
		}
	}
}

// tailscaled listens on a fixed WireGuard port: the host lets only that port
// through between tslink's veths, so colocated nodes keep a direct path.
func TestTailscaledArgsFixedPort(t *testing.T) {
	d := &Daemon{config: DaemonConfig{StateDir: "/s"}, socketPath: "/sock"}
	args := d.tailscaledArgs()
	want := "--port=" + strconv.Itoa(WireGuardPort)
	if !slices.Contains(args, want) {
		t.Errorf("args %q lack %s", args, want)
	}
}
