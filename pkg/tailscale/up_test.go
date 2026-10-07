package tailscale

import (
	"slices"
	"strings"
	"testing"
)

// upArgs runs tryBringUp against a fake tailscale CLI and returns its arguments.
func upArgs(t *testing.T, cfg DaemonConfig) []string {
	t.Helper()
	cli := &fakeCLI{}
	d := newTestDaemon(t, cli, "")
	cfg.EndpointID = d.config.EndpointID
	d.config = cfg
	if err := d.tryBringUp(false); err != nil {
		t.Fatal(err)
	}
	if len(cli.calls) != 1 {
		t.Fatalf("ran %q, want one command", cli.calls)
	}
	return cli.calls[0]
}

func TestUpLoginServer(t *testing.T) {
	args := upArgs(t, DaemonConfig{Hostname: "web", LoginServer: "https://hs.example.com"})
	if !slices.Contains(args, "--login-server=https://hs.example.com") {
		t.Errorf("args %q lack --login-server", args)
	}
	for _, a := range upArgs(t, DaemonConfig{Hostname: "web"}) {
		if strings.HasPrefix(a, "--login-server") {
			t.Errorf("unexpected %q without a login server", a)
		}
	}
}

// The auth key goes to the CLI's standard input, never into its arguments,
// which every process on the host can read.
func TestUpAuthKeyOnStdin(t *testing.T) {
	cli := &fakeCLI{}
	d := newTestDaemon(t, cli, "")
	d.config.AuthKey = func() (string, error) { return "tskey-auth-secret", nil }
	if err := d.tryBringUp(true); err != nil {
		t.Fatal(err)
	}
	if len(cli.calls) != 1 {
		t.Fatalf("ran %q, want one command", cli.calls)
	}
	if !slices.Contains(cli.calls[0], "--authkey=file:/dev/stdin") {
		t.Errorf("args %q do not read the key from stdin", cli.calls[0])
	}
	if strings.Contains(strings.Join(cli.calls[0], " "), "tskey-auth-secret") {
		t.Errorf("args %q contain the key", cli.calls[0])
	}
	if cli.stdins[0] != "tskey-auth-secret" {
		t.Errorf("stdin = %q, want the key", cli.stdins[0])
	}
}
