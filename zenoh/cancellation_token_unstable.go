//go:build zenoh_unstable

package zenoh

import (
	"context"
	"sync"
)

// CancellationToken mirrors zenoh-rust's `zenoh::ext::CancellationToken`.
// Call Cancel to signal any operation that has accepted the token (today:
// Session.GetWithContext via a derived context) to abort.
//
// Zero-value construction works: `var ct CancellationToken`. The first
// call to Context / Cancel / IsCancelled lazily initialises the underlying
// context.
type CancellationToken struct {
	once   sync.Once
	cancel context.CancelFunc
	ctx    context.Context
}

// NewCancellationToken allocates a fresh token.
func NewCancellationToken() *CancellationToken { return &CancellationToken{} }

// Context returns a derived context that is cancelled when Cancel is
// called. The parent is used only on the first call; subsequent calls
// ignore their parent argument and return the cached context. Passing nil
// is equivalent to context.Background(). Note: if Cancel() runs before any
// Context() call, the parent is pinned to context.Background().
func (t *CancellationToken) Context(parent context.Context) context.Context {
	t.once.Do(func() {
		if parent == nil {
			parent = context.Background()
		}
		t.ctx, t.cancel = context.WithCancel(parent)
	})
	return t.ctx
}

// Cancel signals cancellation. Idempotent; safe to call on a zero-value
// token (initialises it first).
func (t *CancellationToken) Cancel() {
	t.Context(nil)
	t.cancel()
}

// IsCancelled reports whether Cancel has been called. Safe on a zero-value
// token (returns false).
func (t *CancellationToken) IsCancelled() bool {
	select {
	case <-t.Context(nil).Done():
		return true
	default:
		return false
	}
}

// WithCancellation attaches a CancellationToken to a GetOptions. Calling
// t.Cancel() aborts the Get exactly as if the caller had cancelled its
// own context; the two sources are merged so either one triggers
// termination. Returns o for call-chaining.
//
// Mutates o in place. Reusing the same *GetOptions across multiple
// Session.Get calls means every Get shares the same token — so
// t.Cancel() aborts all of them. Usually the intended behaviour; if you
// need per-Get tokens, allocate fresh GetOptions per call.
//
// Available only under the zenoh_unstable build tag — zenoh-rust exposes
// this feature as `zenoh::ext::CancellationToken` and has the same
// stability disclaimer.
func (o *GetOptions) WithCancellation(t *CancellationToken) *GetOptions {
	o.cancelCtx = t.Context(nil)
	return o
}
