package tailscale

import (
	"slices"
	"testing"
)

func TestServeOptionArgs(t *testing.T) {
	if got := serveOptionArgs(ServeEndpoint{Proto: "tcp", ProxyProtocol: "2"}); !slices.Equal(got, []string{"--proxy-protocol=2"}) {
		t.Errorf("with proxy protocol: %q", got)
	}
	if got := serveOptionArgs(ServeEndpoint{Proto: "http", AcceptAppCaps: "example.com/cap/a,example.com/cap/b"}); !slices.Equal(got, []string{"--accept-app-caps=example.com/cap/a,example.com/cap/b"}) {
		t.Errorf("with app capabilities: %q", got)
	}
	if got := serveOptionArgs(ServeEndpoint{Proto: "tcp"}); len(got) != 0 {
		t.Errorf("without options: %q", got)
	}
}
