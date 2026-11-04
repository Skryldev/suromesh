package client

import "context"

// mergeContext derives a context that carries a's values/deadline and is
// cancelled when EITHER a or b is done. Used to splice the client's
// lifecycle cancellation into caller-provided operation contexts so Close
// deterministically interrupts Connect, Invoke, and retry backoff.
func mergeContext(a, b context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(a)
	go func() {
		select {
		case <-b.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}
