package zenoh

import (
	"fmt"
	"sync/atomic"

	"github.com/shirou/zenoh-go-client/internal/wire"
)

// Liveliness is a namespace for liveliness operations, reached via
// Session.Liveliness(). Mirrors zenoh-go's `session.Liveliness().…` shape
// so user code can port directly.
type Liveliness struct {
	session *Session
}

// Liveliness returns the liveliness-scoped API for this session.
func (s *Session) Liveliness() *Liveliness { return &Liveliness{session: s} }

// LivelinessTokenOptions is reserved for future extensions (timestamp,
// attachment, etc.); empty today, matching zenoh-go.
type LivelinessTokenOptions struct{}

// LivelinessToken is a router-advertised "I exist" marker for a key
// expression. While the token lives, peers that declare a liveliness
// subscriber / run a liveliness get on an intersecting key see it. Drop
// emits U_TOKEN so the router can tell peers the token is gone.
type LivelinessToken struct {
	session *Session
	keyExpr KeyExpr
	id      uint32
	dropped atomic.Bool
}

// DeclareToken registers a liveliness token on keyExpr and emits D_TOKEN to
// the router. Always call Drop when done so peers are notified. The token
// is tracked at the session level so it is automatically re-declared on
// reconnect.
func (l *Liveliness) DeclareToken(keyExpr KeyExpr, opts *LivelinessTokenOptions) (*LivelinessToken, error) {
	_ = opts // no fields yet; reserved for future use
	s := l.session
	if s.closed.Load() {
		return nil, ErrSessionClosed
	}
	if keyExpr.IsZero() {
		return nil, ErrInvalidKeyExpr
	}
	id := s.inner.IDs().AllocTokenID()
	s.tokensMu.Lock()
	s.tokens[id] = keyExpr
	s.tokensMu.Unlock()
	if err := s.sendDeclareToken(id, keyExpr); err != nil {
		s.tokensMu.Lock()
		delete(s.tokens, id)
		s.tokensMu.Unlock()
		return nil, fmt.Errorf("send D_TOKEN: %w", err)
	}
	return &LivelinessToken{session: s, keyExpr: keyExpr, id: id}, nil
}

// KeyExpr returns the token's bound key expression.
func (t *LivelinessToken) KeyExpr() KeyExpr { return t.keyExpr }

// Drop emits U_TOKEN and marks the handle unusable. Safe to call multiple
// times; subsequent calls are no-ops.
func (t *LivelinessToken) Drop() {
	if !t.dropped.CompareAndSwap(false, true) {
		return
	}
	t.session.tokensMu.Lock()
	delete(t.session.tokens, t.id)
	t.session.tokensMu.Unlock()
	// Best-effort U_TOKEN emission. If the link is already gone, peers
	// observe the token loss through session-level cleanup.
	_ = t.session.sendUndeclareToken(t.id, t.keyExpr)
}

func (s *Session) sendDeclareToken(id uint32, keyExpr KeyExpr) error {
	return s.enqueueDeclare(&wire.Declare{
		Body: wire.NewDeclareToken(id, keyExpr.toWire()),
	})
}

func (s *Session) sendUndeclareToken(id uint32, keyExpr KeyExpr) error {
	return s.enqueueDeclare(&wire.Declare{
		Body: &wire.UndeclareEntity{
			Kind:     wire.IDUndeclareToken,
			EntityID: id,
			WireExpr: keyExpr.toWire(),
		},
	})
}
