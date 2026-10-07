package core

// Network represents a Docker network using Tailscale.
type Network struct {
	ID      string
	AuthKey string
	Tags    []string // If set, overrides the tslink.tags container label
}
