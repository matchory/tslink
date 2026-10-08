package tailscale

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
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
// Guards: G5
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

// bringUpCommands runs bringUp on a node that is still logged in, with prefs
// as tailscaled reports them, and returns the commands it ran apart from
// status and prefs queries.
func bringUpCommands(t *testing.T, cfg DaemonConfig, prefs string) [][]string {
	t.Helper()
	d := newTestDaemon(t, &fakeCLI{}, "")
	cfg.EndpointID, cfg.StateDir = d.config.EndpointID, d.config.StateDir
	cfg.AuthKey = func() (string, error) { return "tskey-auth-secret", nil }
	d.config = cfg
	if err := os.WriteFile(
		filepath.Join(cfg.StateDir, "tailscaled.state"),
		[]byte("{}"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	var cmds [][]string
	d.runCLI = func(_ context.Context, c cliCall) (cliOutput, error) {
		switch c.args[1] {
		case "status":
			return cliOutput{stdout: `{"BackendState":"Running"}`}, nil
		case "debug":
			return cliOutput{stdout: prefs}, nil
		}
		cmds = append(cmds, c.args[1:])
		return cliOutput{}, nil
	}
	if err := d.bringUp(); err != nil {
		t.Fatal(err)
	}
	return cmds
}

// A node still logged in with the wanted control server and tags gets the
// rest of its settings with "tailscale set". "tailscale up" would restart
// its control client right after tailscaled dialled control, and tailscaled
// then dials port 443 only, for good, whatever the login server's port.
func TestBringUpLoggedInUsesSet(t *testing.T) {
	cfg := DaemonConfig{
		Hostname:    "web",
		LoginServer: "http://hs.example.com:8080",
		Tags:        []string{"tag:a", "tag:b"},
	}
	cmds := bringUpCommands(t, cfg,
		`{"ControlURL":"http://hs.example.com:8080","AdvertiseTags":["tag:b","tag:a"]}`)
	want := [][]string{{"set", "--hostname=web", "--accept-routes", "--netfilter-mode=off"}}
	if !reflect.DeepEqual(cmds, want) {
		t.Errorf("ran %q, want %q", cmds, want)
	}

	// Tailscale's control server, which tailscaled names when none is set
	cmds = bringUpCommands(t, DaemonConfig{Hostname: "web"},
		`{"ControlURL":"https://controlplane.tailscale.com"}`)
	if len(cmds) != 1 || cmds[0][0] != "set" {
		t.Errorf("ran %q, want tailscale set", cmds)
	}
}

// Other tags or another control server need a new registration, which only
// "tailscale up" does.
func TestBringUpLoggedInChangedUsesUp(t *testing.T) {
	for name, prefs := range map[string]string{
		"tags":    `{"ControlURL":"http://hs.example.com:8080","AdvertiseTags":["tag:a"]}`,
		"server":  `{"ControlURL":"https://other.example.com","AdvertiseTags":["tag:a","tag:b"]}`,
		"unknown": `not json`,
	} {
		t.Run(name, func(t *testing.T) {
			cfg := DaemonConfig{
				Hostname:    "web",
				LoginServer: "http://hs.example.com:8080",
				Tags:        []string{"tag:a", "tag:b"},
			}
			cmds := bringUpCommands(t, cfg, prefs)
			if len(cmds) != 1 || cmds[0][0] != "up" {
				t.Errorf("ran %q, want tailscale up", cmds)
			}
			if slices.Contains(cmds[0], "--authkey=file:/dev/stdin") {
				t.Errorf("args %q pass the auth key to a logged-in node", cmds[0])
			}
		})
	}
}
