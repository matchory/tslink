package core

import "testing"

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
