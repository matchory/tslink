package core

import "testing"

func TestParseNetworkOptionsLoginServer(t *testing.T) {
	opts := ParseNetworkOptions(map[string]any{
		GenericOptionsKey: map[string]any{LoginServerOption: "https://headscale.example.com"},
	})
	if opts.LoginServer != "https://headscale.example.com" {
		t.Errorf("LoginServer = %q", opts.LoginServer)
	}
	if opts := ParseNetworkOptions(map[string]any{}); opts.LoginServer != "" {
		t.Errorf("LoginServer without the option = %q, want empty", opts.LoginServer)
	}
}

func TestValidateLoginServer(t *testing.T) {
	valid := []string{
		"",
		"https://headscale.example.com",
		"http://10.0.0.1:8080",
		"https://hs.example.com/prefix",
	}
	for _, s := range valid {
		if err := ValidateLoginServer(s); err != nil {
			t.Errorf("ValidateLoginServer(%q) = %v, want nil", s, err)
		}
	}
	invalid := []string{
		"headscale.example.com",
		"ftp://headscale.example.com",
		"https://",
		"http://[::1",
	}
	for _, s := range invalid {
		if err := ValidateLoginServer(s); err == nil {
			t.Errorf("ValidateLoginServer(%q) = nil, want an error", s)
		}
	}
}
