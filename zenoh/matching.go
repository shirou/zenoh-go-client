package zenoh

import (
	"sync/atomic"

	"github.com/shirou/zenoh-go-client/internal/session"
)

// MatchingStatus reports whether at least one remote entity matches a
// Publisher's or Querier's key expression. Mirrors zenoh-go's field shape
// (single `Matching` bool) so the same callers apply to both APIs.
type MatchingStatus struct {
	Matching bool
}

// MatchingListener is the handle returned by DeclareMatchingListener.
// Drop / Undeclare detach the listener without tearing down the
// underlying Publisher / Querier INTEREST; the listener stops receiving
// deliveries but the matching count continues to track for any other
// listener (and for GetMatchingStatus).
type MatchingListener struct {
	session    *Session
	interestID uint32
	listenerID session.MatchingListenerID
	drop       func()
	handle     <-chan MatchingStatus
	detached   atomic.Bool
}

// Handler returns the channel associated with a channel-backed handler
// (FifoChannel / RingChannel); nil for Closure-backed listeners.
func (m *MatchingListener) Handler() <-chan MatchingStatus { return m.handle }

// Undeclare detaches the listener. Idempotent.
func (m *MatchingListener) Undeclare() error {
	m.detach()
	return nil
}

func (m *MatchingListener) Drop() { m.detach() }

func (m *MatchingListener) detach() {
	if !m.detached.CompareAndSwap(false, true) {
		return
	}
	m.session.inner.DetachMatchingListener(m.interestID, m.listenerID)
	if m.drop != nil {
		m.drop()
	}
}

// DeclareMatchingListener registers a handler that receives a
// MatchingStatus delivery after the Publisher's initial INTEREST
// snapshot completes and on every subsequent transition between "zero
// matching subscribers" and "at least one matching subscriber".
//
// The returned *MatchingListener must be retained and Dropped when no
// longer needed; a dropped Publisher automatically invalidates every
// listener bound to it.
func (p *Publisher) DeclareMatchingListener(handler Handler[MatchingStatus]) (*MatchingListener, error) {
	return declareMatchingListener(p.session, p.interestID, handler)
}

// DeclareBackgroundMatchingListener attaches a Closure that fires on the
// same cadence as DeclareMatchingListener but does not expose a handle —
// the returned error only reports attach-time failures. The listener
// lives until the Publisher is dropped.
func (p *Publisher) DeclareBackgroundMatchingListener(closure Closure[MatchingStatus]) error {
	_, err := declareMatchingListener(p.session, p.interestID, closure)
	return err
}

// GetMatchingStatus reads the current matching-subscribers flag
// synchronously. Before the initial snapshot has completed it reports
// Matching=false; callers that need "true as soon as the router
// confirms" should prefer a MatchingListener.
func (p *Publisher) GetMatchingStatus() (MatchingStatus, error) {
	return getMatchingStatus(p.session, p.interestID)
}

// DeclareMatchingListener — see [Publisher.DeclareMatchingListener]. The
// Querier variant tracks matching Queryables instead of Subscribers.
func (q *Querier) DeclareMatchingListener(handler Handler[MatchingStatus]) (*MatchingListener, error) {
	return declareMatchingListener(q.session, q.interestID, handler)
}

// DeclareBackgroundMatchingListener — see [Publisher.DeclareBackgroundMatchingListener].
func (q *Querier) DeclareBackgroundMatchingListener(closure Closure[MatchingStatus]) error {
	_, err := declareMatchingListener(q.session, q.interestID, closure)
	return err
}

// GetMatchingStatus — see [Publisher.GetMatchingStatus].
func (q *Querier) GetMatchingStatus() (MatchingStatus, error) {
	return getMatchingStatus(q.session, q.interestID)
}

func declareMatchingListener(s *Session, interestID uint32, handler Handler[MatchingStatus]) (*MatchingListener, error) {
	if s.closed.Load() {
		return nil, ErrSessionClosed
	}
	deliver, drop, userHandle := handler.Attach()
	ml := &MatchingListener{session: s, interestID: interestID, drop: drop}
	onEntryRemoved := func() {
		// Parent Publisher / Querier dropped: close the handler channel
		// so consumers exit cleanly. Mark detached to keep the user's
		// explicit Drop idempotent.
		if ml.detached.CompareAndSwap(false, true) && drop != nil {
			drop()
		}
	}
	listenerID, ok := s.inner.AttachMatchingListener(interestID,
		func(st session.MatchingStatus) {
			deliver(MatchingStatus{Matching: st.Matching})
		},
		onEntryRemoved,
	)
	if !ok {
		// Entry already gone (Publisher/Querier dropped); tear down the
		// attached handler so no goroutine leaks.
		if drop != nil {
			drop()
		}
		return nil, ErrAlreadyDropped
	}
	ml.listenerID = listenerID
	if ch, hasCh := userHandle.(<-chan MatchingStatus); hasCh {
		ml.handle = ch
	}
	return ml, nil
}

func getMatchingStatus(s *Session, interestID uint32) (MatchingStatus, error) {
	if s.closed.Load() {
		return MatchingStatus{}, ErrSessionClosed
	}
	st, ok := s.inner.GetMatchingStatus(interestID)
	if !ok {
		return MatchingStatus{}, ErrAlreadyDropped
	}
	return MatchingStatus{Matching: st.Matching}, nil
}
