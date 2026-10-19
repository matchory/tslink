package core

import (
	"fmt"
	"net/netip"
	"net/url"
)

// ValidateLoginServer checks a login server URL, the value of
// LoginServerSetting. An empty one selects Tailscale's control server.
//
// It takes https, or http with an IP literal as the host: tailscaled resolves
// the control server's name with the container's resolvers, which a stack
// chooses, and plain http does not authenticate the server, so a name, even
// localhost, would let a stack have its nodes send their keys to a server of
// its own. A URL with userinfo is refused.
func ValidateLoginServer(s string) error {
	if s == "" {
		return nil
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.User != nil {
		return fmt.Errorf("invalid %s %q: want an http or https URL", LoginServerSetting, s)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if _, err := netip.ParseAddr(u.Hostname()); err != nil {
			return fmt.Errorf(
				"invalid %s %q: an http URL needs an IP address as its host, "+
					"since containers' resolvers answer for names; use https for a name",
				LoginServerSetting, s,
			)
		}
		return nil
	default:
		return fmt.Errorf("invalid %s %q: want an http or https URL", LoginServerSetting, s)
	}
}
