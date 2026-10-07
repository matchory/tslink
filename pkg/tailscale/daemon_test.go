package tailscale

import (
	"errors"
	"os/exec"
	"slices"
	"testing"
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
	for _, cliErr := range []error{nil, errors.New("exit status 1")} {
		cli := &fakeCLI{err: cliErr}
		d := newTestDaemon(t, cli, "")
		cmd := exec.Command("sleep", "30")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		d.killProcess(cmd)
		want := []string{"--socket=" + testSocket, "down"}
		if len(cli.calls) != 1 || !slices.Equal(cli.calls[0], want) {
			t.Errorf("calls = %q, want %q", cli.calls, want)
		}
		if cmd.ProcessState == nil {
			t.Errorf("with down error %v: tailscaled was not killed", cliErr)
		}
	}
}
