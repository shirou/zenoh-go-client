package zenoh

import "sync/atomic"

// Publisher is a handle bound to a specific key expression that can emit
// repeated Put / Delete samples without re-supplying the key.
type Publisher struct {
	session *Session
	keyExpr KeyExpr
	options PublisherOptions
	dropped atomic.Bool
}

// PublisherOptions sets per-publisher defaults for Put / Delete. It is a
// type alias of PutOptions because the per-emission option set is the
// same; keeping the name distinct mirrors zenoh-go's public surface.
type PublisherOptions = PutOptions

// DeclarePublisher returns a Publisher bound to keyExpr. The associated
// INTEREST (matching-subscribers tracking) will be emitted in Phase 6 when
// MatchingListener lands.
func (s *Session) DeclarePublisher(keyExpr KeyExpr, opts *PublisherOptions) (*Publisher, error) {
	if s.closed.Load() {
		return nil, ErrSessionClosed
	}
	if keyExpr.IsZero() {
		return nil, ErrInvalidKeyExpr
	}
	p := &Publisher{session: s, keyExpr: keyExpr}
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

// Drop marks the publisher unusable. No wire message is sent because
// publishers have no per-wire entity (PUSH is self-contained); the
// INTEREST teardown for matching-status is handled by Phase 6.
func (p *Publisher) Drop() { p.dropped.Store(true) }

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
