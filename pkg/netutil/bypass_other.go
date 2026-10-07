//go:build !linux

package netutil

import "errors"

// SetupBypassRoute needs Linux policy routing.
func SetupBypassRoute(_, _, _ string) error {
	return errors.New("policy routing requires Linux")
}
