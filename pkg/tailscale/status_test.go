package tailscale

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// The status JSON is read from standard output alone: a warning on standard
// error does not break it.
func TestGetStatus(t *testing.T) {
	cli := &fakeCLI{
		out:    `{"Self":{"TailscaleIPs":["100.64.0.1","fd7a::1"],"HostName":"web","Online":true}}`,
		stderr: "Warning: client version differs\n",
	}
	d := newTestDaemon(t, cli, "")
	st, err := d.getStatus()
	if err != nil {
		t.Fatal(err)
	}
	if *st != (Status{IP: "100.64.0.1", Hostname: "web", Online: true}) {
		t.Errorf("status = %+v", *st)
	}
	want := []string{"--socket=" + testSocket, "status", "--json"}
	if len(cli.calls) != 1 || !slices.Equal(cli.calls[0], want) {
		t.Errorf("calls = %q, want %q", cli.calls, want)
	}

	cli.out, cli.stderr, cli.err = "", "failed to connect\n", errors.New("exit status 1")
	if _, err := d.getStatus(); err == nil || !strings.Contains(err.Error(), "failed to connect") {
		t.Errorf("error = %v, want the CLI's output", err)
	}
}

func TestWaitBackendState(t *testing.T) {
	cli := &fakeCLI{out: `{"BackendState":"NeedsLogin"}`, stderr: "Warning: something\n"}
	d := newTestDaemon(t, cli, "")
	if st := d.waitBackendState(); st != "NeedsLogin" {
		t.Errorf("backend state = %q, want NeedsLogin", st)
	}
	want := []string{"--socket=" + testSocket, "status", "--json"}
	if len(cli.calls) != 1 || !slices.Equal(cli.calls[0], want) {
		t.Errorf("calls = %q, want %q", cli.calls, want)
	}
}
