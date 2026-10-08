package core

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matchory/tslink/pkg/tailscale"
)

func TestValidHostname(t *testing.T) {
	for _, h := range []string{"web", "my-app_1", "billing_api.1.rhqn99qc3kawxeu8hgljewtff", "A1"} {
		if !validHostname.MatchString(h) {
			t.Errorf("%q should be valid", h)
		}
	}
	for _, h := range []string{"", ".", "..", "../other", "a/b", "/abs", "-lead", ".hidden", "a b", "a\x00b"} {
		if validHostname.MatchString(h) {
			t.Errorf("%q should be invalid", h)
		}
	}
}

func TestStateDirFor(t *testing.T) {
	tests := []struct {
		stack, hostname, want string
	}{
		{"", "web", "/data/by-hostname/web"},
		{"billing", "billing_api.1.abc", "/data/by-stack/billing/billing_api.1.abc"},
	}
	for _, tt := range tests {
		got, err := stateDirFor("/data", tt.stack, tt.hostname)
		if err != nil || got != tt.want {
			t.Errorf(
				"stateDirFor(%q, %q) = %q, %v; want %q",
				tt.stack,
				tt.hostname,
				got,
				err,
				tt.want,
			)
		}
	}
	for _, tt := range [][2]string{{"", "../x"}, {"..", "web"}, {"a/b", "web"}, {"billing", ""}} {
		if got, err := stateDirFor("/data", tt[0], tt[1]); err == nil {
			t.Errorf("stateDirFor(%q, %q) = %q, want error", tt[0], tt[1], got)
		}
	}
}

func TestStartTailscaleRejectsForeignStack(t *testing.T) {
	e := &Endpoint{
		ID:         "0123456789abcdef",
		Network:    &Network{ID: "net", AuthKey: "tskey-test"},
		SandboxKey: "/var/run/docker/netns/test",
		DataDir:    t.TempDir(),
	}
	for _, stack := range []string{"other", ""} {
		info := &ContainerInfo{Hostname: "web", Stack: stack, NetworkStack: "billing"}
		err := e.StartTailscale(info)
		if err == nil || !strings.Contains(err.Error(), "does not match network stack") {
			t.Errorf("stack %q: got %v, want stack mismatch error", stack, err)
		}
	}
}

func TestSocketPathForFitsSunPath(t *testing.T) {
	// Longest valid stack and hostname must not push the socket past the
	// 108-byte sun_path limit, inside the plugin or as seen from the host.
	id := strings.Repeat("a", 64)
	for _, dataDir := range []string{"/data", "/var/lib/docker-plugins/tailscale"} {
		p := socketPathFor(dataDir, id)
		if len(p) >= 108 {
			t.Errorf("socket path %q is %d bytes, want < 108", p, len(p))
		}
	}
	if got := socketPathFor("/data", id); got != "/data/sock/aaaaaaaaaaaa.sock" {
		t.Errorf("socketPathFor = %q", got)
	}
}

// Two endpoints whose IDs hash to the same /30 get different ones: with the
// same subnet on two host veths, the host routes the second container's
// traffic to the first.
func TestPickVethSubnetSkipsUsed(t *testing.T) {
	const id = "0123456789abcdef"
	host, ctr, err := pickVethSubnet(id, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Unused: the subnet the hash names, as before, so recovered endpoints
	// of older versions keep theirs
	wantHost, wantCtr := generateVethIPs(id)
	if host.String() != wantHost || ctr.String() != wantCtr {
		t.Errorf("got %s %s, want %s %s", host, ctr, wantHost, wantCtr)
	}
	if netip.PrefixFrom(host, 30).Masked() != netip.PrefixFrom(ctr, 30).Masked() {
		t.Errorf("%s and %s are not in one /30", host, ctr)
	}

	used := map[netip.Prefix]bool{netip.PrefixFrom(host, 30).Masked(): true}
	host2, ctr2, err := pickVethSubnet(id, used)
	if err != nil {
		t.Fatal(err)
	}
	if used[netip.PrefixFrom(host2, 30).Masked()] || host2.Next() != ctr2 {
		t.Errorf("got %s %s, used %v", host2, ctr2, used)
	}
}

// The last slot wraps to the first, and a full range is an error.
func TestPickVethSubnetFull(t *testing.T) {
	used := map[netip.Prefix]bool{}
	for slot := 1; slot <= vethSlots; slot++ {
		host, _ := vethSlotIPs(slot)
		used[netip.PrefixFrom(host, 30).Masked()] = true
	}
	free, _ := vethSlotIPs(1)
	delete(used, netip.PrefixFrom(free, 30).Masked())
	// Any ID: only slot 1 is free
	host, _, err := pickVethSubnet("ffffffffffff", used)
	if err != nil || host != free {
		t.Errorf("got %s, %v; want %s", host, err, free)
	}
	used[netip.PrefixFrom(free, 30).Masked()] = true
	if _, _, err := pickVethSubnet("ffffffffffff", used); err == nil {
		t.Error("no error with every subnet in use")
	}
}

func TestWipeStateOnKeyChange(t *testing.T) {
	// newState returns a state directory registered with key
	newState := func(t *testing.T, key string) string {
		t.Helper()
		dir := t.TempDir()
		state := filepath.Join(dir, "tailscaled.state")
		if err := os.WriteFile(state, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := tailscale.SaveAuthKeyHash(dir, key); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	tests := []struct {
		name    string
		network *Network
		wiped   bool
	}{
		{"same key", &Network{AuthKey: "tskey-auth-a"}, false},
		{"changed key", &Network{AuthKey: "tskey-auth-b"}, true},
		// The cluster credential is read at each login, not hashed
		{"cluster credential", &Network{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := newState(t, "tskey-auth-a")
			wipeStateOnKeyChange(dir, tt.network)
			if got := !tailscale.StateExists(dir); got != tt.wiped {
				t.Errorf("wiped = %v, want %v", got, tt.wiped)
			}
		})
	}
}
