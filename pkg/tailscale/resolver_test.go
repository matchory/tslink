package tailscale

import (
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestTailscaledResolvConf(t *testing.T) {
	quad100 := []netip.Addr{netip.MustParseAddr("100.100.100.100")}
	const resolvedStub = "# This is /run/systemd/resolve/stub-resolv.conf managed by man:systemd-resolved(8).\n" +
		"nameserver 127.0.0.53\noptions edns0 trust-ad\nsearch corp.example\n"
	const resolvedUpstreams = "# This is /run/systemd/resolve/resolv.conf managed by man:systemd-resolved(8).\n" +
		"nameserver 10.0.0.2\nnameserver 2001:db8::53\nsearch corp.example\n"

	tests := []struct {
		name      string
		host      string
		resolved  string
		container []netip.Addr
		want      string
		warns     bool
	}{
		{
			name: "host with public resolvers",
			host: "nameserver 1.1.1.1\nnameserver 9.9.9.9\n",
			want: "nameserver 1.1.1.1\nnameserver 9.9.9.9\n",
		},
		{
			name:      "a container using only Tailscale's resolver gets the host's",
			host:      "nameserver 1.1.1.1\n",
			container: quad100,
			want:      "nameserver 1.1.1.1\n",
		},
		{
			name:     "systemd-resolved stub uses its upstreams",
			host:     resolvedStub,
			resolved: resolvedUpstreams,
			want:     "nameserver 10.0.0.2\nnameserver 2001:db8::53\nsearch corp.example\n",
		},
		{
			name:     "upstreams on the host are kept over systemd-resolved's",
			host:     "nameserver 127.0.0.1\nnameserver 192.168.1.1\n",
			resolved: resolvedUpstreams,
			want:     "nameserver 192.168.1.1\n",
		},
		{
			name:  "stub without systemd-resolved's file falls back to Docker's resolver",
			host:  resolvedStub,
			want:  "nameserver 127.0.0.11\nsearch corp.example\noptions edns0 trust-ad\n",
			warns: true,
		},
		{
			name:     "systemd-resolved without upstreams falls back to Docker's resolver",
			host:     resolvedStub,
			resolved: "# no upstreams\n",
			want:     "nameserver 127.0.0.11\nsearch corp.example\noptions edns0 trust-ad\n",
			warns:    true,
		},
		{
			name:      "Docker's resolver would loop through a container using 100.100.100.100",
			host:      resolvedStub,
			container: quad100,
			want:      "nameserver 8.8.8.8\nnameserver 8.8.4.4\nsearch corp.example\noptions edns0 trust-ad\n",
			warns:     true,
		},
		{
			name:      "the container's resolvers win over the host's",
			host:      "nameserver 1.1.1.1\nsearch host.example\n",
			container: []netip.Addr{netip.MustParseAddr("10.0.0.2")},
			want:      "nameserver 10.0.0.2\nsearch host.example\n",
		},
		{
			name: "the container's resolvers besides Tailscale's are used, without a fallback",
			host: resolvedStub,
			container: []netip.Addr{
				netip.MustParseAddr("100.100.100.100"),
				netip.MustParseAddr("10.0.0.2"),
			},
			want: "nameserver 10.0.0.2\nsearch corp.example\noptions edns0 trust-ad\n",
		},
		{
			name: "Tailscale's IPv6 resolver and Docker's are dropped from the container's",
			host: "nameserver 1.1.1.1\n",
			container: []netip.Addr{
				netip.MustParseAddr("fd7a:115c:a1e0::53"),
				netip.MustParseAddr("127.0.0.11"),
				netip.MustParseAddr("2001:db8::53"),
			},
			want: "nameserver 2001:db8::53\n",
		},
		{
			name:      "a container using only Docker's resolver gets the host's",
			host:      "nameserver 1.1.1.1\n",
			container: []netip.Addr{netip.MustParseAddr("127.0.0.11")},
			want:      "nameserver 1.1.1.1\n",
		},
		{
			name:      "a resolver on the container's loopback is kept",
			host:      "nameserver 1.1.1.1\n",
			container: []netip.Addr{netip.MustParseAddr("127.0.0.1")},
			want:      "nameserver 127.0.0.1\n",
		},
		{
			name:      "Docker's resolver would loop through a container using Tailscale's IPv6 resolver",
			host:      "nameserver ::1\n",
			container: []netip.Addr{netip.MustParseAddr("fd7a:115c:a1e0::53")},
			want:      "nameserver 8.8.8.8\nnameserver 8.8.4.4\n",
			warns:     true,
		},
		{
			name: "IPv6 resolvers are kept, loopback and link-local dropped",
			host: "nameserver ::1\nnameserver fe80::1%eth0\nnameserver 2001:db8::1\nnameserver 169.254.169.253\n",
			want: "nameserver 2001:db8::1\nnameserver 169.254.169.253\n",
		},
		{
			name: "Tailscale's own resolver is dropped",
			host: "nameserver 100.100.100.100\nnameserver 1.1.1.1\nsearch tail1234.ts.net\n",
			want: "nameserver 1.1.1.1\nsearch tail1234.ts.net\n",
		},
		{
			name: "last search line wins, domain counts as search, options accumulate, the rest is dropped",
			host: "# comment\n; comment\ndomain old.example\nnameserver 1.1.1.1\nnameserver bogus\n" +
				"search a.example b.example\noptions ndots:2\noptions timeout:1\nsortlist 10.0.0.0\n",
			want: "nameserver 1.1.1.1\nsearch a.example b.example\noptions ndots:2 timeout:1\n",
		},
		{
			name:  "no host resolv.conf falls back to Docker's resolver",
			want:  "nameserver 127.0.0.11\n",
			warns: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var resolved []byte
			if tt.resolved != "" {
				resolved = []byte(tt.resolved)
			}
			got, warning := tailscaledResolvConf([]byte(tt.host), resolved, tt.container)
			if string(got) != tt.want {
				t.Errorf("resolv.conf:\n%s\nwant:\n%s", got, tt.want)
			}
			if (warning != "") != tt.warns {
				t.Errorf("warning = %q, want a warning: %v", warning, tt.warns)
			}
		})
	}
}

func TestTailscaledCommandWritesResolvConf(t *testing.T) {
	dir := t.TempDir()
	host := filepath.Join(dir, "host-resolv.conf")
	if err := os.WriteFile(host, []byte("nameserver 127.0.0.53\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resolved := filepath.Join(dir, "resolved-resolv.conf")
	if err := os.WriteFile(resolved, []byte("nameserver 10.0.0.2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldHost, oldResolved := hostResolvConfPath, resolvedResolvConfPath
	hostResolvConfPath, resolvedResolvConfPath = host, resolved
	t.Cleanup(func() { hostResolvConfPath, resolvedResolvConfPath = oldHost, oldResolved })

	stateDir := filepath.Join(dir, "state")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{
		config: DaemonConfig{
			StateDir:      stateDir,
			NetNSPath:     "/run/netns/x",
			TailscaledBin: "/tailscaled",
		},
	}
	d.ctx = t.Context()
	cmd := d.tailscaledCommand([]string{"--tun=tailscale0"})

	got, err := os.ReadFile(filepath.Join(stateDir, "resolv.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "nameserver 10.0.0.2\n" {
		t.Errorf("resolv.conf = %q", got)
	}
	backup := filepath.Join(stateDir, "resolv.pre-tailscale-backup.conf")
	if _, err := os.Stat(backup); err != nil {
		t.Errorf("backup file: %v", err)
	}

	// Run the shell part with mount and nsenter replaced by scripts that
	// print their arguments. As root it would create the backup in /etc.
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	sh := slices.Index(cmd.Args, "sh")
	if sh < 0 {
		t.Fatalf("no sh in %q", cmd.Args)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mount", "nsenter"} {
		script := "#!/bin/sh\necho " + name + ` "$@"` + "\n"
		if err := os.WriteFile(
			filepath.Join(bin, name),
			[]byte(script),
			0o700,
		); err != nil { // #nosec G306 -- test script
			t.Fatal(err)
		}
	}
	run := exec.Command(cmd.Args[sh], cmd.Args[sh+1:]...) // #nosec G204 -- the command under test
	run.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
	out, err := run.Output()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	want := "mount --bind " + filepath.Join(stateDir, "resolv.conf") + " /etc/resolv.conf\n"
	if !strings.HasPrefix(string(out), want) {
		t.Errorf("output %q does not start with %q", out, want)
	}
	want = "nsenter --net=/run/netns/x -- /tailscaled --tun=tailscale0\n"
	if !strings.HasSuffix(string(out), want) {
		t.Errorf("output %q does not end with %q", out, want)
	}
}

// Falling back to other resolvers than the host's is reported in the
// endpoint's status, and a later start with the host's resolvers clears it.
func TestTailscaledCommandWarnsAboutFallback(t *testing.T) {
	dir := t.TempDir()
	host := filepath.Join(dir, "host-resolv.conf")
	if err := os.WriteFile(host, []byte("nameserver 127.0.0.53\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldHost, oldResolved := hostResolvConfPath, resolvedResolvConfPath
	hostResolvConfPath, resolvedResolvConfPath = host, filepath.Join(dir, "absent")
	t.Cleanup(func() { hostResolvConfPath, resolvedResolvConfPath = oldHost, oldResolved })

	d, rec := newWarningDaemon(t)
	d.config.ContainerDNS = []netip.Addr{netip.MustParseAddr("100.100.100.100")}
	d.tailscaledCommand(nil)
	if got := rec.get("dns-upstreams"); !strings.Contains(got, "8.8.8.8") {
		t.Errorf("warning = %q, want one naming 8.8.8.8", got)
	}

	if err := os.WriteFile(host, []byte("nameserver 1.1.1.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d.tailscaledCommand(nil)
	if got := rec.get("dns-upstreams"); got != "" {
		t.Errorf("warning kept after a start with the host's resolvers: %q", got)
	}
}

// tailscaled does not get the plugin's TS_AUTHKEY: it does not need it, and
// its peerapi serves its environment (/v0/env) to peers that the control
// server lets debug it.
func TestTailscaledCommandWithoutAuthKey(t *testing.T) {
	t.Setenv("TS_AUTHKEY", "tskey-auth-secret")
	t.Setenv("TSLINK_TEST_KEPT", "1")
	d := &Daemon{config: DaemonConfig{StateDir: t.TempDir(), TailscaledBin: "/tailscaled"}}
	d.ctx = t.Context()
	env := d.tailscaledCommand(nil).Env
	if !slices.Contains(env, "TSLINK_TEST_KEPT=1") {
		t.Errorf("env %q lacks the plugin's other variables", env)
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "TS_AUTHKEY=") {
			t.Errorf("env contains %q", kv)
		}
	}
}

// runSandboxed runs tailscaled's command as root in a mount namespace of the
// test's own, with nsenter replaced by a script and tailscaled by show, after
// setup prepares the namespace. /etc and /run are scratch directories there.
// It returns what show printed.
func runSandboxed(t *testing.T, setup, show string) string {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root for mount namespaces")
	}
	dir := t.TempDir()
	host := filepath.Join(dir, "host-resolv.conf")
	if err := os.WriteFile(host, []byte("nameserver 10.0.0.2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldHost, oldResolved := hostResolvConfPath, resolvedResolvConfPath
	hostResolvConfPath, resolvedResolvConfPath = host, filepath.Join(dir, "absent")
	t.Cleanup(func() { hostResolvConfPath, resolvedResolvConfPath = oldHost, oldResolved })

	stateDir := filepath.Join(dir, "state")
	bin := filepath.Join(dir, "bin")
	for _, d := range []string{stateDir, bin, filepath.Join(dir, "etc")} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	scripts := map[string]string{
		// nsenter --net=NETNS -- PROGRAM ARGS...
		"nsenter":    "#!/bin/sh\nshift 2\nexec \"$@\"\n",
		"tailscaled": "#!/bin/sh\n" + show + "\n",
	}
	for name, script := range scripts {
		// #nosec G306 -- test script
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	d := &Daemon{config: DaemonConfig{
		StateDir:      stateDir,
		NetNSPath:     "/run/netns/x",
		TailscaledBin: filepath.Join(bin, "tailscaled"),
	}}
	d.ctx = t.Context()
	cmd := d.tailscaledCommand(nil)

	script := "set -e\nmount --bind \"$0/etc\" /etc\ntouch /etc/resolv.conf\n" +
		"mount -t tmpfs tmpfs /run\n" + setup + "\nexec \"$@\""
	args := append(
		[]string{"--mount", "--propagation", "private", "--", "sh", "-c", script, dir},
		cmd.Args...)
	run := exec.Command("unshare", args...) // #nosec G204 -- the command under test
	run.Env = []string{"PATH=" + bin + ":/usr/sbin:/usr/bin:/sbin:/bin"}
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return string(out)
}

// tailscaled gets its own resolv.conf even when the plugin's is a bind mount
// of a file that has since been replaced, as systemd-resolved replaces
// stub-resolv.conf whenever the host's DNS changes. The kernel refuses to
// mount over a deleted file (move_mount: ENOENT).
func TestTailscaledCommandOverStaleResolvConf(t *testing.T) {
	// /etc as the plugin sees it after systemd-resolved replaced the file
	// its resolv.conf is a bind mount of
	setup := `echo "nameserver 127.0.0.53" >"$0/stub"
mount --bind "$0/stub" /etc/resolv.conf
rm "$0/stub"
grep -q "//deleted /etc/resolv.conf " /proc/self/mountinfo`
	if out := runSandboxed(t, setup, "cat /etc/resolv.conf"); out != "nameserver 10.0.0.2\n" {
		t.Errorf("tailscaled saw %q, want its own resolv.conf", out)
	}
}

// tailscaled does not reach the host's system bus, which the plugin sees in
// the host's /run. Through it, tailscaled would configure the host's
// systemd-resolved, with DNS settings from the control server, for the host
// link whose index its tailscale0 has in the container.
func TestTailscaledCommandHidesSystemBus(t *testing.T) {
	setup := "mkdir /run/dbus\ntouch /run/dbus/system_bus_socket"
	if out := runSandboxed(t, setup, "ls -A /run/dbus"); out != "" {
		t.Errorf("tailscaled sees %q in /run/dbus", out)
	}
}
