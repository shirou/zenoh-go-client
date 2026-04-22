package zenoh

import (
	"fmt"
	"sync/atomic"

	"github.com/shirou/zenoh-go-client/internal/session"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// Publisher is a handle bound to a specific key expression that can emit
// repeated Put / Delete samples without re-supplying the key.
//
// On declaration a Publisher sends an INTEREST[Mode=CurrentFuture,
// Filter=KEYEXPRS+SUBSCRIBERS, restricted=keyExpr] so the router starts
// tracking matching subscribers. The count drives
// DeclareMatchingListener notifications; Drop emits INTEREST[Final] to
// release it.
type Publisher struct {
	session    *Session
	keyExpr    KeyExpr
	options    PublisherOptions
	interestID uint32
	dropped    atomic.Bool
}

// PublisherOptions sets per-publisher defaults for Put / Delete. It is a
// type alias of PutOptions because the per-emission option set is the
// same; keeping the name distinct mirrors zenoh-go's public surface.
type PublisherOptions = PutOptions

// DeclarePublisher returns a Publisher bound to keyExpr.
func (s *Session) DeclarePublisher(keyExpr KeyExpr, opts *PublisherOptions) (*Publisher, error) {
	if s.closed.Load() {
		return nil, ErrSessionClosed
	}
	if keyExpr.IsZero() {
		return nil, ErrInvalidKeyExpr
	}
	id := s.inner.IDs().AllocInterestID()

	s.publishersMu.Lock()
	s.publishers[id] = &publisherState{keyExpr: keyExpr}
	s.publishersMu.Unlock()
	s.inner.RegisterMatching(id, keyExpr.internalKeyExpr(), session.MatchingSubscribers)

	if err := s.sendPublisherInterest(id, keyExpr); err != nil {
		s.publishersMu.Lock()
		delete(s.publishers, id)
		s.publishersMu.Unlock()
		s.inner.UnregisterMatching(id)
		return nil, fmt.Errorf("send INTEREST: %w", err)
	}

	p := &Publisher{session: s, keyExpr: keyExpr, interestID: id}
	if opts != nil {
		p.options = *opts
	}
	return p, nil
}

// KeyExpr returns the publisher's bound key expression.
func (p *Publisher) KeyExpr() KeyExpr { return p.keyExpr }

// Put emits a PUT sample. perCallOpts, when non-nil, overrides the
// publisher defaults on every field it sets.
func (p *Publisher) Put(payload ZBytes, perCallOpts *PublisherOptions) error {
	if p.dropped.Load() {
		return ErrAlreadyDropped
	}
	return p.session.Put(p.keyExpr, payload, p.mergeOptions(perCallOpts))
}

// Delete emits a DEL sample on the publisher's key expression.
func (p *Publisher) Delete(perCallOpts *PublisherOptions) error {
	if p.dropped.Load() {
		return ErrAlreadyDropped
	}
	return p.session.Delete(p.keyExpr, p.mergeOptions(perCallOpts))
}

// Drop marks the publisher unusable and tears down its INTEREST. Safe to
// call multiple times. During reconnect the INTEREST Final send is a
// no-op (ErrSessionNotReady) — the router drops the old link's interest
// when the link closes, so callers see no difference.
func (p *Publisher) Drop() {
	if !p.dropped.CompareAndSwap(false, true) {
		return
	}
	p.session.publishersMu.Lock()
	delete(p.session.publishers, p.interestID)
	p.session.publishersMu.Unlock()
	p.session.inner.UnregisterMatching(p.interestID)
	_ = p.session.sendInterestFinal(p.interestID)
}

// mergeOptions overlays per-call options on the publisher defaults.
// Uses the Has* flags to distinguish "unset" from "zero-valued".
func (p *Publisher) mergeOptions(perCall *PublisherOptions) *PutOptions {
	merged := p.options
	if perCall == nil {
		return &merged
	}
	if perCall.HasEncoding {
		merged.Encoding, merged.HasEncoding = perCall.Encoding, true
	}
	if perCall.HasPriority {
		merged.Priority, merged.HasPriority = perCall.Priority, true
	}
	if perCall.HasCongestion {
		merged.CongestionControl, merged.HasCongestion = perCall.CongestionControl, true
	}
	if perCall.IsExpress {
		merged.IsExpress = true
	}
	return &merged
}

// sendPublisherInterest emits INTEREST[Mode=CurrentFuture, K+S, restricted=ke].
// Filter.KeyExprs is set so the router is free to alias the keyExpr
// before sending matching D_SUBSCRIBER back.
func (s *Session) sendPublisherInterest(id uint32, ke KeyExpr) error {
	wexpr := ke.toWire()
	return s.enqueueControl(&wire.Interest{
		InterestID: id,
		Mode:       wire.InterestModeCurrentFuture,
		Filter:     wire.InterestFilter{KeyExprs: true, Subscribers: true},
		KeyExpr:    &wexpr,
	})
}

// publisherState is the replay-side view of a live Publisher. Kept in
// Session.publishers so the reconnect orchestrator can re-emit the
// INTEREST on a fresh link.
type publisherState struct {
	keyExpr KeyExpr
}
