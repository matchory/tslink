package core

// Network represents a Docker network using Tailscale.
type Network struct {
	ID      string
	AuthKey string
	Tags    []string // If set, overrides the tslink.tags container label
	MTU     int      // veth MTU; 0 for the default

	LoginServer string // control server URL; empty for Tailscale's
}
