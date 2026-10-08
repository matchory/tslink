package tailscale

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"tailscale.com/ipn"
	"tailscale.com/types/preftype"
)

// loginDaemon returns a daemon with cfg whose tailscaled is api.
func loginDaemon(t *testing.T, api *fakeLocalAPI, cfg DaemonConfig) (*Daemon, *fakeCLI) {
	t.Helper()
	cli := &fakeCLI{}
	d := newTestDaemon(t, cli, "")
	cfg.EndpointID, cfg.StateDir = d.config.EndpointID, d.config.StateDir
	if cfg.AuthKey == nil {
		cfg.AuthKey = func() (string, error) { return "tskey-auth-secret", nil }
	}
	d.config = cfg
	d.lc = api.client()
	return d, cli
}

// A login applies the settings "tailscale up" would, with the tags, and
// starts tailscaled's login with the key.
func TestLogIn(t *testing.T) {
	for name, tt := range map[string]struct {
		loginServer, controlURL string
	}{
		"login server": {"https://hs.example.com", "https://hs.example.com"},
		"tailscale":    {"", defaultControlURL},
	} {
		t.Run(name, func(t *testing.T) {
			api := newFakeLocalAPI(t)
			api.loggedOut()
			d, _ := loginDaemon(t, api, DaemonConfig{
				Hostname: "web", LoginServer: tt.loginServer, Tags: []string{"tag:a"},
			})
			if err := d.logIn(true, false); err != nil {
				t.Fatal(err)
			}
			if got := api.keys(); !slices.Equal(got, []string{"tskey-auth-secret"}) {
				t.Errorf("Start got keys %q, want the auth key", got)
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			p := api.prefs
			if p.ControlURL != tt.controlURL || p.Hostname != "web" || !p.RouteAll ||
				!p.CorpDNS || p.NetfilterMode != preftype.NetfilterOff ||
				!p.NoStatefulFiltering.EqualBool(true) || !p.WantRunning ||
				!slices.Equal(p.AdvertiseTags, []string{"tag:a"}) {
				t.Errorf("prefs = %+v", p)
			}
		})
	}
}

// The auth key reaches tailscaled only over its socket, in the login request:
// no CLI runs, so it is in no process's arguments or environment.
// Guards: G5
func TestLogInAuthKeyOnlyOverSocket(t *testing.T) {
	api := newFakeLocalAPI(t)
	api.loggedOut()
	d, cli := loginDaemon(t, api, DaemonConfig{Hostname: "web"})
	if err := d.logIn(true, false); err != nil {
		t.Fatal(err)
	}
	if len(cli.calls) != 0 {
		t.Errorf("ran %q, want no CLI", cli.calls)
	}
	if got := api.keys(); !slices.Equal(got, []string{"tskey-auth-secret"}) {
		t.Errorf("Start got keys %q, want the auth key", got)
	}
	for _, r := range api.paths() {
		if strings.Contains(r, "tskey") {
			t.Errorf("request %q holds the key", r)
		}
	}
}

// An OAuth client secret is exchanged for a key with the node's tags, which
// tailscaled gets instead of the secret.
func TestLogInMintsKeyForOAuthSecret(t *testing.T) {
	keyAPI := newFakeKeyAPI(t)
	api := newFakeLocalAPI(t)
	api.loggedOut()
	d, _ := loginDaemon(t, api, DaemonConfig{
		Hostname: "web",
		Tags:     []string{"tag:a"},
		AuthKey: func() (string, error) {
			return "tskey-client-abc-def?baseURL=" + keyAPI.URL, nil
		},
	})
	if err := d.logIn(true, false); err != nil {
		t.Fatal(err)
	}
	if got := api.keys(); !slices.Equal(got, []string{"tskey-auth-minted"}) {
		t.Errorf("Start got keys %q, want the minted one", got)
	}
}

// An error tailscaled reports during the login fails it, with its message,
// so that bringUp recognizes stale state.
func TestLogInFails(t *testing.T) {
	api := newFakeLocalAPI(t)
	api.loggedOut()
	api.startErr = "register request: invalid key"
	d, _ := loginDaemon(t, api, DaemonConfig{Hostname: "web"})
	err := d.logIn(true, false)
	if err == nil || !strings.Contains(err.Error(), "invalid key") || !isStateError(err) {
		t.Errorf("err = %v, want a state error with tailscaled's message", err)
	}
}

// A login that never gets the node running times out.
func TestLogInTimesOut(t *testing.T) {
	setDuration(t, &loginTimeout, 200*time.Millisecond)
	api := newFakeLocalAPI(t)
	api.loggedOut()
	d, _ := loginDaemon(t, api, DaemonConfig{Hostname: "web"})
	// Without a key or a node key, tailscaled keeps needing a login
	if err := d.logIn(false, false); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Errorf("err = %v, want a timeout", err)
	}
}

// Logging in again as a new device waits for the new node key: the node is
// still Running with the old one meanwhile.
func TestLogInForceReauth(t *testing.T) {
	api := newFakeLocalAPI(t)
	api.loggedIn(ipn.Prefs{ControlURL: defaultControlURL})
	old := api.status().Self.PublicKey
	d, _ := loginDaemon(t, api, DaemonConfig{Hostname: "web"})
	if err := d.logIn(true, true); err != nil {
		t.Fatal(err)
	}
	if api.status().Self.PublicKey == old {
		t.Error("node key unchanged")
	}
	if !slices.Contains(api.paths(), "POST /localapi/v0/login-interactive") {
		t.Errorf("requests %q start no new login", api.paths())
	}
}

// bringUpLoggedIn runs bringUp on a node that is still logged in with prefs,
// and returns tailscaled's prefs afterwards and the keys Start got.
func bringUpLoggedIn(t *testing.T, cfg DaemonConfig, prefs ipn.Prefs) (ipn.Prefs, []string) {
	t.Helper()
	api := newFakeLocalAPI(t)
	api.loggedIn(prefs)
	d, _ := loginDaemon(t, api, cfg)
	if err := os.WriteFile(
		filepath.Join(d.config.StateDir, "tailscaled.state"),
		[]byte("{}"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := d.bringUp(); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	return api.prefs, slices.Clone(api.authKeys)
}

// A node still logged in with the wanted control server and tags only gets
// its settings. A new login would restart its control client right after
// tailscaled dialled control, and tailscaled then dials port 443 only, for
// good, whatever the login server's port.
func TestBringUpLoggedInAppliesSettings(t *testing.T) {
	cfg := DaemonConfig{
		Hostname:    "web",
		LoginServer: "http://hs.example.com:8080",
		Tags:        []string{"tag:a", "tag:b"},
	}
	prefs, keys := bringUpLoggedIn(t, cfg, ipn.Prefs{
		ControlURL:    "http://hs.example.com:8080",
		AdvertiseTags: []string{"tag:b", "tag:a"},
	})
	if keys != nil {
		t.Errorf("started a login with keys %q", keys)
	}
	if prefs.Hostname != "web" || !prefs.RouteAll || prefs.NetfilterMode != preftype.NetfilterOff {
		t.Errorf("settings not applied: %+v", prefs)
	}

	// Tailscale's control server, which tailscaled names when none is set
	_, keys = bringUpLoggedIn(t, DaemonConfig{Hostname: "web"},
		ipn.Prefs{ControlURL: defaultControlURL})
	if keys != nil {
		t.Errorf("started a login with keys %q", keys)
	}
}

// Applying settings leaves the node's Services alone: tailscaled's config
// file would set them, but tslink's serve configuration advertises them.
func TestBringUpKeepsAdvertisedServices(t *testing.T) {
	prefs, _ := bringUpLoggedIn(t, DaemonConfig{Hostname: "web"}, ipn.Prefs{
		ControlURL:        defaultControlURL,
		AdvertiseServices: []string{"svc:web"},
	})
	if !slices.Equal(prefs.AdvertiseServices, []string{"svc:web"}) {
		t.Errorf("advertised Services = %q, want svc:web kept", prefs.AdvertiseServices)
	}
}

// Other tags or another control server need a new registration, which a
// logged-in node gets without the auth key.
func TestBringUpLoggedInChangedLogsIn(t *testing.T) {
	for name, prefs := range map[string]ipn.Prefs{
		"tags": {
			ControlURL: "http://hs.example.com:8080", AdvertiseTags: []string{"tag:a"},
		},
		"server": {
			ControlURL: "https://other.example.com", AdvertiseTags: []string{"tag:a", "tag:b"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := DaemonConfig{
				Hostname:    "web",
				LoginServer: "http://hs.example.com:8080",
				Tags:        []string{"tag:a", "tag:b"},
			}
			got, keys := bringUpLoggedIn(t, cfg, prefs)
			if !slices.Equal(keys, []string{""}) {
				t.Errorf("Start got keys %q, want one login without the key", keys)
			}
			if got.ControlURL != cfg.LoginServer ||
				!slices.Equal(got.AdvertiseTags, cfg.Tags) {
				t.Errorf("prefs = %+v", got)
			}
		})
	}
}

// Logging out goes to tailscaled's LocalAPI.
func TestLogout(t *testing.T) {
	api := newFakeLocalAPI(t)
	api.loggedIn(ipn.Prefs{})
	d, _ := loginDaemon(t, api, DaemonConfig{})
	if err := d.Logout(); err != nil {
		t.Fatal(err)
	}
	if st := api.status().BackendState; st != needsLogin {
		t.Errorf("backend state = %s, want %s", st, needsLogin)
	}
}
