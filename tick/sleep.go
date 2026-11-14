package tick

import (
	"context"
	"time"
)

// Sleep waits for d or until ctx is done, and reports whether the full wait ran. Use it in
// place of time.Sleep wherever a context can end the wait. A nil ctx waits the full d.
func Sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx == nil || ctx.Err() == nil
	}
	if ctx == nil {
		time.Sleep(d)
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
