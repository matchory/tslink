package preflight

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/plugin"
)

// fakeRun answers commands by their name and arguments, joined with spaces.
func fakeRun(
	answers map[string]string,
	failing ...string,
) func(context.Context, string, ...string) ([]byte, error) {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		cmd := strings.Join(append([]string{name}, args...), " ")
		for _, f := range failing {
			if strings.HasPrefix(cmd, f) {
				return []byte("table does not exist"), errors.New("exit status 1")
			}
		}
		return []byte(answers[cmd]), nil
	}
}

// isolatedTables answers the mangle listings of a host where tslink's
// isolation is in force.
func isolatedTables() map[string]string {
	return map[string]string{
		"iptables -t mangle -S FORWARD": "-P FORWARD ACCEPT\n" +
			"-A FORWARD -j TSLINK-VETH-FWD\n-A FORWARD -j TSLINK-ISOLATE-FWD\n" +
			"-A FORWARD -i eth0 -j ACCEPT\n",
		"iptables -t mangle -S INPUT": "-P INPUT ACCEPT\n-A INPUT -j TSLINK-ISOLATE-IN\n",
		"ip6tables -t mangle -S FORWARD": "-P FORWARD ACCEPT\n" +
			"-A FORWARD -j TSLINK-ISOLATE-FWD\n",
		"ip6tables -t mangle -S INPUT": "-P INPUT ACCEPT\n-A INPUT -j TSLINK-ISOLATE-IN\n",
	}
}

func testEnv(t *testing.T) Env {
	t.Helper()
	data := t.TempDir()
	if err := os.MkdirAll(filepath.Join(data, "sock"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "sock", "abc.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(data, "sock", "abc.sock")
	answers := isolatedTables()
	answers["tailscale --socket="+sock+" lock status --json"] = `{"Enabled":true}`
	return Env{
		Docker:  newFakeDocker(),
		Run:     fakeRun(answers),
		DataDir: data,
	}
}

func TestCheckOrder(t *testing.T) {
	var got []string
	for _, r := range Check(context.Background(), testEnv(t)) {
		got = append(got, r.Property)
	}
	if want := "E1 E2 E4 E5 E6 E7 E8 E9"; strings.Join(got, " ") != want {
		t.Errorf("properties %v, want %s", got, want)
	}
}

func TestHostIsolation(t *testing.T) {
	env := testEnv(t)
	r := result(t, Check(context.Background(), env), "E5")
	if r.Status != OK || !strings.Contains(r.Detail, "on this host") {
		t.Errorf("E5 = %+v, want ok on this host", r)
	}
	for _, tt := range []struct {
		name, cmd, listing, want string
	}{
		{
			"veth jump missing", "iptables -t mangle -S FORWARD",
			"-P FORWARD ACCEPT\n-A FORWARD -j TSLINK-ISOLATE-FWD\n", "TSLINK-VETH-FWD",
		},
		{
			"jump behind a foreign rule", "iptables -t mangle -S INPUT",
			"-P INPUT ACCEPT\n-A INPUT -i lo -j ACCEPT\n-A INPUT -j TSLINK-ISOLATE-IN\n",
			"TSLINK-ISOLATE-IN",
		},
		{
			"IPv6 forward jump missing", "ip6tables -t mangle -S FORWARD",
			"-P FORWARD ACCEPT\n", "TSLINK-ISOLATE-FWD",
		},
		{
			"chains in the other order", "iptables -t mangle -S FORWARD",
			"-P FORWARD ACCEPT\n-A FORWARD -j TSLINK-ISOLATE-FWD\n-A FORWARD -j TSLINK-VETH-FWD\n",
			"",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			answers := isolatedTables()
			answers[tt.cmd] = tt.listing
			env := testEnv(t)
			env.Run = fakeRun(answers)
			r := result(t, Check(context.Background(), env), "E5")
			if tt.want == "" {
				if r.Status != OK {
					t.Errorf("E5 = %+v, want ok", r)
				}
				return
			}
			if r.Status != Violated || !strings.Contains(r.Detail, tt.want) {
				t.Errorf("E5 = %+v, want violated naming %s", r, tt.want)
			}
		})
	}
	env.Run = fakeRun(nil, "ip6tables -t mangle -S INPUT")
	if r := result(t, Check(context.Background(), env), "E5"); r.Status != Violated {
		t.Errorf("listing fails: E5 = %+v, want violated", r)
	}
}

func TestIsolationOffStillChecksVethJump(t *testing.T) {
	answers := map[string]string{"iptables -t mangle -S FORWARD": "-P FORWARD ACCEPT\n"}
	env := testEnv(t)
	f := newFakeDocker()
	f.plugins[0].Settings.Env = []string{"TSLINK_ISOLATE_HOST_TAILNET=false"}
	env.Docker, env.Run = f, fakeRun(answers)
	r := result(t, Check(context.Background(), env), "E5")
	if r.Status != Violated || !strings.Contains(r.Detail, "TSLINK-VETH-FWD") ||
		strings.Contains(r.Detail, "ip6tables") {
		t.Errorf("E5 = %+v, want violated for the setting and the veth jump only", r)
	}
}

func TestTagScopeSetting(t *testing.T) {
	for _, tt := range []struct {
		env    []string
		status Status
	}{
		{nil, Unknown},
		{[]string{"TSLINK_TAG_SCOPE=exact"}, Unknown},
		{[]string{"TSLINK_TAG_SCOPE=prefix"}, Violated},
	} {
		env := testEnv(t)
		f := newFakeDocker()
		f.plugins[0].Settings.Env = tt.env
		env.Docker = f
		if r := result(t, Check(context.Background(), env), "E8"); r.Status != tt.status {
			t.Errorf("settings %v: E8 = %+v, want %s", tt.env, r, tt.status)
		}
	}
}

func TestHostIsolationSetting(t *testing.T) {
	for _, tt := range []struct {
		env    []string
		status Status
	}{
		{nil, OK},
		{[]string{"TSLINK_ISOLATE_HOST_TAILNET=true"}, OK},
		{[]string{"TSLINK_ISOLATE_HOST_TAILNET=yes"}, OK}, // the plugin keeps isolating
		{[]string{"TSLINK_ISOLATE_HOST_TAILNET=false"}, Violated},
		{[]string{"TS_AUTHKEY=", "TSLINK_ISOLATE_HOST_TAILNET=FALSE"}, Violated},
	} {
		env := testEnv(t)
		f := newFakeDocker()
		f.plugins[0].Settings.Env = tt.env
		env.Docker = f
		r := result(t, Check(context.Background(), env), "E5")
		if r.Status != tt.status {
			t.Errorf("settings %v: E5 = %+v, want %s", tt.env, r, tt.status)
		}
		if r.Status == Violated && !strings.Contains(r.Detail, "TSLINK_ISOLATE_HOST_TAILNET") {
			t.Errorf("settings %v: E5 detail %q does not name the setting", tt.env, r.Detail)
		}
	}
	env := testEnv(t)
	f := newFakeDocker()
	f.pluginErr = errDockerDown
	env.Docker = f
	if r := result(t, Check(context.Background(), env), "E5"); r.Status != Error {
		t.Errorf("Docker down: E5 = %+v, want error", r)
	}
}

func TestPluginDigest(t *testing.T) {
	for _, tt := range []struct {
		ref    string
		status Status
	}{
		{"ghcr.io/matchory/tslink:v1.2.0-amd64@sha256:" + strings.Repeat("a", 64), OK},
		{"ghcr.io/matchory/tslink:latest-amd64", Violated},
		{"", Violated},
	} {
		env := testEnv(t)
		f := newFakeDocker()
		f.plugins[0].PluginReference = tt.ref
		env.Docker = f
		if r := result(t, Check(context.Background(), env), "E6"); r.Status != tt.status {
			t.Errorf("reference %q: E6 = %+v, want %s", tt.ref, r, tt.status)
		}
	}
	env := testEnv(t)
	f := newFakeDocker()
	f.plugins = []plugin.Plugin{}
	env.Docker = f
	if r := result(t, Check(context.Background(), env), "E6"); r.Status != Unknown {
		t.Errorf("no plugin: E6 = %+v, want unknown", r)
	}
}

func TestSharedDir(t *testing.T) {
	env := testEnv(t)
	if r := result(t, Check(context.Background(), env), "E7"); r.Status != Unknown {
		t.Errorf("without --shared-dir: E7 = %+v, want unknown", r)
	}
	env.SharedDir = t.TempDir()
	if err := os.Chmod(env.SharedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	r := result(t, Check(context.Background(), env), "E7")
	if r.Status != Violated || !strings.Contains(r.Detail, "0755") {
		t.Errorf("mode 0755: E7 = %+v, want violated naming the mode", r)
	}
	if os.Getuid() == 0 {
		if err := os.Chmod(env.SharedDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if r := result(t, Check(context.Background(), env), "E7"); r.Status != OK {
			t.Errorf("root, 0700: E7 = %+v, want ok", r)
		}
	}
}

func TestTailnetLock(t *testing.T) {
	env := testEnv(t)
	if r := result(t, Check(context.Background(), env), "E9"); r.Status != OK {
		t.Errorf("enabled: E9 = %+v, want ok", r)
	}
	sock := filepath.Join(env.DataDir, "sock", "abc.sock")
	env.Run = fakeRun(
		map[string]string{
			"tailscale --socket=" + sock + " lock status --json": `{"Enabled":false}`,
		},
	)
	if r := result(t, Check(context.Background(), env), "E9"); r.Status != Info {
		t.Errorf("disabled: E9 = %+v, want info", r)
	}
	env.DataDir = t.TempDir()
	if r := result(t, Check(context.Background(), env), "E9"); r.Status != Unknown {
		t.Errorf("no tailscaled: E9 = %+v, want unknown", r)
	}
}

// TestTailnetLockEveryNode covers a host with several tslink nodes: each
// answers for itself.
func TestTailnetLockEveryNode(t *testing.T) {
	data := t.TempDir()
	if err := os.MkdirAll(filepath.Join(data, "sock"), 0o700); err != nil {
		t.Fatal(err)
	}
	socks := map[string]string{}
	for _, name := range []string{"a", "b", "c"} {
		sock := filepath.Join(data, "sock", name+".sock")
		if err := os.WriteFile(sock, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		socks[name] = "tailscale --socket=" + sock + " lock status --json"
	}
	on, off := `{"Enabled":true}`, `{"Enabled":false}`
	for _, tt := range []struct {
		name    string
		answers map[string]string
		failing []string
		status  Status
		detail  string
	}{
		{"all on", map[string]string{socks["a"]: on, socks["b"]: on, socks["c"]: on}, nil, OK, ""},
		{
			"one off",
			map[string]string{socks["a"]: on, socks["b"]: off, socks["c"]: on},
			nil, Info,
			"1 of 3",
		},
		{
			"last off",
			map[string]string{socks["a"]: on, socks["b"]: on, socks["c"]: off},
			nil, Info,
			"1 of 3",
		},
		{
			"one silent",
			map[string]string{socks["b"]: on, socks["c"]: on},
			[]string{socks["a"]},
			OK,
			"",
		},
		{"none answers", nil, []string{socks["a"], socks["b"], socks["c"]}, Unknown, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := testEnv(t)
			env.DataDir = data
			env.Run = fakeRun(tt.answers, tt.failing...)
			r := result(t, Check(context.Background(), env), "E9")
			if r.Status != tt.status || !strings.Contains(r.Detail, tt.detail) {
				t.Errorf("E9 = %+v, want %s naming %q", r, tt.status, tt.detail)
			}
		})
	}
}

func TestWrite(t *testing.T) {
	var buf bytes.Buffer
	failed, err := Write(&buf, []Result{
		{Property: "E1", Status: OK, Detail: "fine"},
		{Property: "E9", Status: Info, Detail: "off"},
		{Property: "E7", Status: Unknown, Detail: "cannot check"},
	})
	if err != nil || failed {
		t.Fatalf("Write = %v, %v; want not failed", failed, err)
	}
	if want := "E1  ok        fine\nE9  info      off\nE7  unknown   cannot check\n"; buf.String() != want {
		t.Errorf("Write printed %q, want %q", buf.String(), want)
	}
	for _, s := range []Status{Violated, Error} {
		if failed, _ := Write(&bytes.Buffer{}, []Result{{Property: "E2", Status: s}}); !failed {
			t.Errorf("Write with %s: not failed", s)
		}
	}
}
