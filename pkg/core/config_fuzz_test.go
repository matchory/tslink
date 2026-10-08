package core

import (
	"net/url"
	"strings"
	"testing"
)

// FuzzParseNetworkOptions checks what ParseNetworkOptions hands on from
// network options: the key, login server and ephemeral setting unchanged,
// tags as single non-empty elements of a comma-separated list, an MTU in
// range, and a login server ValidateLoginServer accepts only if it is an
// http or https URL with a host.
//
// Guards: G3
func FuzzParseNetworkOptions(f *testing.F) {
	f.Add("tskey-auth-x", "tag:a,tag:b", "1400", "https://hs.example.com", "true")
	f.Add("", " , tag:a ,,", "575", "ftp://x", "")
	f.Add("k?ephemeral=false", "tag:a\ntag:b", "65536", "http://u:p@h:8080/x", "maybe")
	f.Fuzz(func(t *testing.T, key, tags, mtu, login, ephemeral string) {
		o := ParseNetworkOptions(map[string]any{GenericOptionsKey: map[string]any{
			"tslink.authkey": key, "tslink.tags": tags, MTUOption: mtu,
			LoginServerOption: login, EphemeralOption: ephemeral,
		}})
		if o.AuthKey != key || o.LoginServer != login || o.Ephemeral != ephemeral {
			t.Fatalf("ParseNetworkOptions changed a value: %+v", o)
		}
		for _, tag := range o.Tags {
			if tag == "" || strings.Contains(tag, ",") || tag != strings.TrimSpace(tag) {
				t.Fatalf("tags %q from %q: element %q", o.Tags, tags, tag)
			}
		}
		if o.MTU != 0 && (o.MTU < 576 || o.MTU > 65535) {
			t.Fatalf("MTU %d from %q", o.MTU, mtu)
		}
		if login != "" && ValidateLoginServer(login) == nil {
			u, err := url.Parse(login)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				t.Fatalf("ValidateLoginServer accepted %q", login)
			}
		}
	})
}
