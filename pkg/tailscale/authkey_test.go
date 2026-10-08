package tailscale

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// fakeKeyAPI is Tailscale's API as far as minting auth keys goes: it issues
// a token for the client secret and an auth key for it, and notes what the
// key was asked for.
type fakeKeyAPI struct {
	*httptest.Server

	mu     sync.Mutex
	secret string // The client secret the token was issued for
	caps   map[string]any
}

func newFakeKeyAPI(t *testing.T) *fakeKeyAPI {
	t.Helper()
	f := &fakeKeyAPI{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/oauth/token":
			if err := r.ParseForm(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			_, secret, _ := r.BasicAuth()
			f.mu.Lock()
			f.secret = secret + r.PostForm.Get("client_secret")
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write([]byte(
				`{"access_token":"token","token_type":"Bearer","expires_in":3600}`,
			)); err != nil {
				return
			}
		case "/api/v2/tailnet/-/keys":
			if r.Header.Get("Authorization") != "Bearer token" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			var req struct {
				Capabilities struct {
					Devices struct {
						Create map[string]any `json:"create"`
					} `json:"devices"`
				} `json:"capabilities"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			f.caps = req.Capabilities.Devices.Create
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write([]byte(`{"key":"tskey-auth-minted"}`)); err != nil {
				return
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

// An OAuth client secret becomes a single-use key for the tags, with the
// attributes "tailscale up" reads from it and its defaults.
func TestResolveAuthKeyMintsForOAuthSecrets(t *testing.T) {
	for name, tt := range map[string]struct {
		params string
		want   map[string]any
	}{
		"defaults": {"", map[string]any{
			"reusable": false, "ephemeral": true, "preauthorized": false,
			"tags": []any{"tag:a", "tag:b"},
		}},
		"persistent, preauthorized": {"&ephemeral=false&preauthorized=true", map[string]any{
			"reusable": false, "ephemeral": false, "preauthorized": true,
			"tags": []any{"tag:a", "tag:b"},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			api := newFakeKeyAPI(t)
			secret := "tskey-client-abc-def?baseURL=" + api.URL + tt.params
			got, err := resolveAuthKey(t.Context(), secret, []string{"tag:a", "tag:b"})
			if err != nil {
				t.Fatal(err)
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			if got != "tskey-auth-minted" {
				t.Errorf("key = %q, want the minted one", got)
			}
			if api.secret != "tskey-client-abc-def" {
				t.Errorf("token for %q, want the secret without its attributes", api.secret)
			}
			for k, v := range tt.want {
				if !reflect.DeepEqual(api.caps[k], v) {
					t.Errorf("%s = %v, want %v", k, api.caps[k], v)
				}
			}
		})
	}
}

func TestResolveAuthKeyPassesOtherKeys(t *testing.T) {
	for _, k := range []string{"tskey-auth-abc", "hskey-auth-abc", "0123456789abcdef"} {
		got, err := resolveAuthKey(t.Context(), k, []string{"tag:a"})
		if err != nil || got != k {
			t.Errorf("resolveAuthKey(%q) = %q, %v, want it unchanged", k, got, err)
		}
	}
}

func TestResolveAuthKeyRejects(t *testing.T) {
	for name, tt := range map[string]struct {
		secret string
		tags   []string
		want   string
	}{
		"no tags":       {"tskey-client-abc", nil, "tags"},
		"unknown":       {"tskey-client-abc?foo=1", []string{"tag:a"}, "foo"},
		"not a boolean": {"tskey-client-abc?ephemeral=maybe", []string{"tag:a"}, "ephemeral"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := resolveAuthKey(t.Context(), tt.secret, tt.tags)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want one naming %q", err, tt.want)
			}
		})
	}
}
