package zenoh

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/shirou/zenoh-go-client/internal/wire"
)

// QuerierOptions is the per-declare configuration for a Querier. Each
// field has the same semantics as the matching field on GetOptions; a
// Querier applies them as defaults to every Querier.Get call.
type QuerierOptions struct {
	Target           QueryTarget
	HasTarget        bool
	Consolidation    ConsolidationMode
	HasConsolidation bool
	Timeout          time.Duration // default per-Get timeout; 0 = unbounded
}

// QuerierGetOptions overrides a subset of the Querier's defaults for one
// Get call. Only the fields with an explicit presence bit (or non-empty
// Parameters) take effect — everything else inherits from the Querier.
type QuerierGetOptions struct {
	Parameters       string
	Consolidation    ConsolidationMode
	HasConsolidation bool
	Target           QueryTarget
	HasTarget        bool
	Timeout          time.Duration
	Budget           uint32
	Buffer           int
}

// Querier is a handle that expresses a long-lived interest in a set of
// queryables: on declare it sends INTEREST (Mode=CurrentFuture, Filter=Q,
// restricted to keyExpr) so the router starts tracking them; subsequent
// Get calls inherit the Querier's defaults. Drop emits INTEREST Final.
type Querier struct {
	session    *Session
	keyExpr    KeyExpr
	opts       QuerierOptions
	interestID uint32
	dropped    atomic.Bool
}

// DeclareQuerier registers a querier bound to keyExpr. Sends the
// subscribe-style INTEREST immediately so the router can stream matching
// queryable state to us (today the client does not expose the stream;
// reporting it via MatchingListener-style callback is a Phase 6 follow-up).
func (s *Session) DeclareQuerier(keyExpr KeyExpr, opts *QuerierOptions) (*Querier, error) {
	if s.closed.Load() {
		return nil, ErrSessionClosed
	}
	if keyExpr.IsZero() {
		return nil, ErrInvalidKeyExpr
	}
	id := s.inner.IDs().AllocInterestID()
	if opts == nil {
		opts = &QuerierOptions{}
	}

	// Track at session level for replay on reconnect.
	s.queriersMu.Lock()
	s.queriers[id] = &querierState{keyExpr: keyExpr, opts: *opts}
	s.queriersMu.Unlock()

	if err := s.sendQuerierInterest(id, keyExpr); err != nil {
		s.queriersMu.Lock()
		delete(s.queriers, id)
		s.queriersMu.Unlock()
		return nil, fmt.Errorf("send INTEREST: %w", err)
	}
	return &Querier{session: s, keyExpr: keyExpr, opts: *opts, interestID: id}, nil
}

// KeyExpr returns the querier's bound key expression.
func (q *Querier) KeyExpr() KeyExpr { return q.keyExpr }

// Get issues a REQUEST using the Querier's key expression. Overrides in
// callOpts (if any) take precedence over the Querier's defaults.
func (q *Querier) Get(callOpts *QuerierGetOptions) (<-chan Reply, error) {
	if q.dropped.Load() {
		return nil, ErrAlreadyDropped
	}
	getOpts := q.mergeGetOptions(callOpts)
	return q.session.Get(q.keyExpr, getOpts)
}

// GetWithContext is Get with a caller-supplied context.
func (q *Querier) GetWithContext(ctx context.Context, callOpts *QuerierGetOptions) (<-chan Reply, error) {
	if q.dropped.Load() {
		return nil, ErrAlreadyDropped
	}
	return q.session.GetWithContext(ctx, q.keyExpr, q.mergeGetOptions(callOpts))
}

// Drop emits INTEREST Final for the querier's interest_id and marks it
// unusable. Safe to call multiple times.
//
// During reconnect the send is a no-op (ErrSessionNotReady); the router
// cleans up the old session's interest when that link drops, so the
// caller sees no observable difference.
func (q *Querier) Drop() {
	if !q.dropped.CompareAndSwap(false, true) {
		return
	}
	q.session.queriersMu.Lock()
	delete(q.session.queriers, q.interestID)
	q.session.queriersMu.Unlock()
	_ = q.session.sendInterestFinal(q.interestID)
}

// mergeGetOptions overlays a QuerierGetOptions on the Querier defaults.
func (q *Querier) mergeGetOptions(call *QuerierGetOptions) *GetOptions {
	out := &GetOptions{
		Consolidation:    q.opts.Consolidation,
		HasConsolidation: q.opts.HasConsolidation,
		Target:           q.opts.Target,
		HasTarget:        q.opts.HasTarget,
		Timeout:          q.opts.Timeout,
	}
	if call == nil {
		return out
	}
	out.Parameters = call.Parameters
	if call.HasConsolidation {
		out.Consolidation, out.HasConsolidation = call.Consolidation, true
	}
	if call.HasTarget {
		out.Target, out.HasTarget = call.Target, true
	}
	if call.Timeout > 0 {
		out.Timeout = call.Timeout
	}
	if call.Budget > 0 {
		out.Budget = call.Budget
	}
	if call.Buffer > 0 {
		out.Buffer = call.Buffer
	}
	return out
}

// sendQuerierInterest emits INTEREST[Mode=CurrentFuture, Q, restricted=ke].
func (s *Session) sendQuerierInterest(id uint32, ke KeyExpr) error {
	wexpr := ke.toWire()
	return s.enqueueControl(&wire.Interest{
		InterestID: id,
		Mode:       wire.InterestModeCurrentFuture,
		Filter:     wire.InterestFilter{Queryables: true},
		KeyExpr:    &wexpr,
	})
}

// sendInterestFinal emits INTEREST[Mode=Final] for id. Tear-down path for
// any INTEREST we own (querier, future liveliness subscriber, etc.).
func (s *Session) sendInterestFinal(id uint32) error {
	return s.enqueueControl(&wire.Interest{InterestID: id, Mode: wire.InterestModeFinal})
}

// querierState is the replay-side view of a live Querier. Kept in
// Session.queriers so the reconnect orchestrator can re-emit the
// INTEREST on a fresh link.
type querierState struct {
	keyExpr KeyExpr
	opts    QuerierOptions
}
