package tailscale

import (
	"errors"
	"os/exec"
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
