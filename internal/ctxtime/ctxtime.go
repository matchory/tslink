// Package ctxtime waits for a while, or until a context is done.
package ctxtime

import (
	"context"
	"time"
)

// Poll calls fn at once and then every interval until it reports done, or
// until ctx is done. It reports whether fn did.
func Poll(ctx context.Context, interval time.Duration, fn func() bool) bool {
	for !fn() {
		if !Sleep(ctx, interval) {
			return false
		}
	}
	return true
}

// Sleep waits for d, or until ctx is done. It reports whether it slept the
// whole time.
func Sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
