package core

import (
	"fmt"
	"net/url"
)

// ValidateLoginServer checks a login server URL. An empty one selects
// Tailscale's control server.
func ValidateLoginServer(s string) error {
	if s == "" {
		return nil
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("invalid %s %q: want an http or https URL", LoginServerOption, s)
	}
	return nil
}
