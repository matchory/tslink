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
	return Env{
		Docker: newFakeDocker(),
		Run: fakeRun(
			map[string]string{
				"tailscale --socket=" + sock + " lock status --json": `{"Enabled":true}`,
			},
		),
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

func TestMangleTable(t *testing.T) {
	env := testEnv(t)
	if r := result(t, Check(context.Background(), env), "E5"); r.Status != OK {
		t.Errorf("E5 = %+v, want ok", r)
	}
	env.Run = fakeRun(nil, "ip6tables -t mangle")
	r := result(t, Check(context.Background(), env), "E5")
	if r.Status != Violated || !strings.Contains(r.Detail, "ip6tables") {
		t.Errorf("E5 = %+v, want violated by ip6tables", r)
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

func TestWrite(t *testing.T) {
	var buf bytes.Buffer
	failed, err := Write(&buf, []Result{
		{Property: "E1", Status: OK, Detail: "fine"},
		{Property: "E9", Status: Info, Detail: "off"},
	})
	if err != nil || failed {
		t.Fatalf("Write = %v, %v; want not failed", failed, err)
	}
	if want := "E1  ok        fine\nE9  info      off\n"; buf.String() != want {
		t.Errorf("Write printed %q, want %q", buf.String(), want)
	}
	for _, s := range []Status{Violated, Error} {
		if failed, _ := Write(&bytes.Buffer{}, []Result{{Property: "E2", Status: s}}); !failed {
			t.Errorf("Write with %s: not failed", s)
		}
	}
}
