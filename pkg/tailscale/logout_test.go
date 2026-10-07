package tailscale

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	testNodeKey    = "privkey:1111111111111111111111111111111111111111111111111111111111111111"
	testMachineKey = "privkey:2222222222222222222222222222222222222222222222222222222222222222"
)

// testState returns a tailscaled state file as tailscaled writes it: a JSON
// object of base64-encoded values, with the prefs of the current profile
// under the key "_current-profile" names.
func testState(t *testing.T, prefs map[string]any) []byte {
	t.Helper()
	prefsJSON, err := json.Marshal(prefs)
	if err != nil {
		t.Fatal(err)
	}
	state := map[string][]byte{
		"_machinekey":      []byte(testMachineKey),
		"_current-profile": []byte("profile-1a2b"),
		"_profiles": []byte(
			`{"1a2b":{"ID":"1a2b","Name":"web.example.ts.net","Key":"profile-1a2b"}}`,
		),
		"profile-1a2b":       prefsJSON,
		"_serve/1a2b":        []byte(`{"Services":{"svc:web":{"TCP":{"443":{"HTTPS":true}}}}}`),
		"_taildrop-received": []byte("1"),
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func runningPrefs() map[string]any {
	return map[string]any{
		"ControlURL":        "https://control.example.com",
		"WantRunning":       true,
		"LoggedOut":         false,
		"Hostname":          "web",
		"AdvertiseServices": []string{"svc:web"},
		"AdvertiseTags":     []string{"tag:web"},
		"Config":            map[string]any{"PrivateNodeKey": testNodeKey, "NodeID": "n1"},
	}
}

func TestLogoutState(t *testing.T) {
	out, err := logoutState(testState(t, runningPrefs()))
	if err != nil {
		t.Fatal(err)
	}
	if out == nil {
		t.Fatal("no state for a node that has a node key")
	}

	var state map[string][]byte
	if err := json.Unmarshal(out, &state); err != nil {
		t.Fatal(err)
	}
	// Only what logging out needs: serve config and everything else is gone
	keys := make([]string, 0, len(state))
	for k := range state {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{
		"_current-profile",
		"_machinekey",
		"_profiles",
		"profile-1a2b",
	}; !slices.Equal(
		keys,
		want,
	) {
		t.Errorf("keys = %v, want %v", keys, want)
	}
	if string(state["_machinekey"]) != testMachineKey {
		t.Error("machine key changed")
	}

	var prefs map[string]any
	if err := json.Unmarshal(state["profile-1a2b"], &prefs); err != nil {
		t.Fatal(err)
	}
	// Logged out, tailscaled starts its control client without logging in,
	// so the node neither polls a network map nor comes up
	if prefs["WantRunning"] != false || prefs["LoggedOut"] != true {
		t.Errorf(
			"WantRunning = %v, LoggedOut = %v; want false, true",
			prefs["WantRunning"],
			prefs["LoggedOut"],
		)
	}
	if _, ok := prefs["AdvertiseServices"]; ok {
		t.Error("AdvertiseServices kept")
	}
	// What logging out needs is kept: the control server and the node key
	if prefs["ControlURL"] != "https://control.example.com" {
		t.Errorf("ControlURL = %v", prefs["ControlURL"])
	}
	if cfg, _ := prefs["Config"].(map[string]any); cfg["PrivateNodeKey"] != testNodeKey {
		t.Error("node key not kept")
	}
}

func TestLogoutStateWithoutNode(t *testing.T) {
	noKey := runningPrefs()
	delete(noKey, "Config")
	nullKey := runningPrefs()
	nullKey["Config"] = nil
	zeroKey := runningPrefs()
	zeroKey["Config"] = map[string]any{"PrivateNodeKey": "privkey:" + strings.Repeat("0", 64)}

	noMachineKey := map[string][]byte{}
	if err := json.Unmarshal(testState(t, runningPrefs()), &noMachineKey); err != nil {
		t.Fatal(err)
	}
	delete(noMachineKey, "_machinekey")
	noMachineKeyRaw, err := json.Marshal(noMachineKey)
	if err != nil {
		t.Fatal(err)
	}

	for name, raw := range map[string][]byte{
		"empty":          []byte("{}"),
		"no node key":    testState(t, noKey),
		"null config":    testState(t, nullKey),
		"zero node key":  testState(t, zeroKey),
		"no machine key": noMachineKeyRaw,
	} {
		t.Run(name, func(t *testing.T) {
			out, err := logoutState(raw)
			if err != nil {
				t.Fatal(err)
			}
			if out != nil {
				t.Error("state returned for a node that never registered")
			}
		})
	}

	if _, err := logoutState([]byte("not json")); err == nil {
		t.Error("no error for a corrupt state file")
	}
}

func TestLogoutStateDir(t *testing.T) {
	stateDir := t.TempDir()
	socket := filepath.Join(t.TempDir(), "sock", "gc.sock")
	if err := os.WriteFile(
		filepath.Join(stateDir, "tailscaled.state"),
		testState(t, runningPrefs()),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	var workDir string
	saved := runLogout
	t.Cleanup(func() { runLogout = saved })
	runLogout = func(_ context.Context, tailscaleBin, tailscaledBin, dir, sock string) error {
		workDir = dir
		if tailscaleBin != "ts" || tailscaledBin != "tsd" || sock != socket {
			t.Errorf("runLogout(%q, %q, _, %q)", tailscaleBin, tailscaledBin, sock)
		}
		// tailscaled runs on a sanitized copy, never on the node's own state
		raw, err := os.ReadFile(filepath.Join(dir, "tailscaled.state"))
		if err != nil {
			t.Fatal(err)
		}
		var state map[string][]byte
		if err := json.Unmarshal(raw, &state); err != nil {
			t.Fatal(err)
		}
		if _, ok := state["_serve/1a2b"]; ok {
			t.Error("serve config in the copy")
		}
		if st, err := os.Stat(
			filepath.Join(dir, "tailscaled.state"),
		); err != nil ||
			st.Mode().Perm() != 0o600 {
			t.Errorf("copy mode = %v (err=%v), want 0600", st.Mode().Perm(), err)
		}
		if _, err := os.Stat(filepath.Dir(sock)); err != nil {
			t.Errorf("socket directory missing: %v", err)
		}
		return errors.New("404 node not found")
	}

	err := LogoutState(context.Background(), "ts", "tsd", stateDir, socket)
	if err == nil || !strings.Contains(err.Error(), "node not found") {
		t.Errorf("err = %v, want the logout's", err)
	}
	if workDir == "" {
		t.Fatal("runLogout not called")
	}
	// The copy goes with the state it was made from, even after a crash
	if filepath.Dir(workDir) != stateDir {
		t.Errorf("work directory %s outside %s", workDir, stateDir)
	}
	if _, err := os.Stat(workDir); !os.IsNotExist(err) {
		t.Errorf("work directory left behind (err=%v)", err)
	}
}

func TestLogoutStateDirWithoutNode(t *testing.T) {
	saved := runLogout
	t.Cleanup(func() { runLogout = saved })
	runLogout = func(context.Context, string, string, string, string) error {
		t.Error("runLogout called without a node to log out")
		return nil
	}

	// A start that failed before tailscaled wrote any state
	if err := LogoutState(
		context.Background(),
		"ts",
		"tsd",
		t.TempDir(),
		"unused.sock",
	); err != nil {
		t.Errorf("no state: %v", err)
	}

	stateDir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(stateDir, "tailscaled.state"),
		[]byte("{}"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := LogoutState(context.Background(), "ts", "tsd", stateDir, "unused.sock"); err != nil {
		t.Errorf("never registered: %v", err)
	}
}

// writeScript writes an executable shell script to dir/name.
func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunLogout(t *testing.T) {
	// Stand-ins: tailscaled creates its socket after a moment and runs until
	// killed; the CLI fails until the socket exists, and records its args
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	tailscaled := writeScript(t, dir, "tailscaled", `
for a; do case $a in --socket=*) sock=${a#--socket=};; esac; done
echo "$@" >"`+log+`.daemon"
env >"`+log+`.env"
sleep 0.3; touch "$sock"; exec sleep 60
`)
	tailscale := writeScript(t, dir, "tailscale", `
[ -e "${1#--socket=}" ] || exit 1
echo "$@" >>"`+log+`"
`)
	t.Setenv("TS_AUTHKEY", "tskey-auth-secret")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	socket := filepath.Join(dir, "gc.sock")
	if err := runLogout(ctx, tailscale, tailscaled, dir, socket); err != nil {
		t.Fatal(err)
	}

	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(strings.TrimSpace(string(calls)), "--socket="+socket+" logout") {
		t.Errorf("CLI calls:\n%s", calls)
	}
	daemonArgs, err := os.ReadFile(log + ".daemon")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--tun=userspace-networking", "--port=0", "--statedir=" + dir, "--socket=" + socket} {
		if !strings.Contains(string(daemonArgs), want) {
			t.Errorf("tailscaled args %q lack %s", daemonArgs, want)
		}
	}
	if env, err := os.ReadFile(
		log + ".env",
	); err != nil ||
		strings.Contains(string(env), "tskey-auth-secret") {
		t.Errorf("auth key passed to tailscaled (err=%v)", err)
	}
}

func TestRunLogoutDaemonExits(t *testing.T) {
	dir := t.TempDir()
	tailscaled := writeScript(t, dir, "tailscaled", "exit 1\n")
	tailscale := writeScript(t, dir, "tailscale", "exit 1\n")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	err := runLogout(ctx, tailscale, tailscaled, dir, filepath.Join(dir, "gc.sock"))
	if err == nil || ctx.Err() != nil {
		t.Errorf("err = %v after %v, want an early error", err, time.Since(start))
	}
}

func TestRunLogoutFails(t *testing.T) {
	// Control answers 404 node not found when the node is already gone
	dir := t.TempDir()
	tailscaled := writeScript(t, dir, "tailscaled", `
for a; do case $a in --socket=*) touch "${a#--socket=}";; esac; done
exec sleep 60
`)
	tailscale := writeScript(t, dir, "tailscale", `
case $2 in logout) echo "404 node not found" >&2; exit 1;; esac
`)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := runLogout(ctx, tailscale, tailscaled, dir, filepath.Join(dir, "gc.sock"))
	if err == nil || !strings.Contains(err.Error(), "node not found") {
		t.Errorf("err = %v, want the CLI's output", err)
	}
}
