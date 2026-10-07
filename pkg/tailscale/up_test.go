package tailscale

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// upArgs runs tryBringUp against a fake tailscale CLI and returns its arguments.
func upArgs(t *testing.T, cfg DaemonConfig) []string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "tailscale")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + out + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.TailscaleBin = bin
	d := &Daemon{config: cfg, socketPath: filepath.Join(dir, "sock")}
	if err := d.tryBringUp(false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
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
