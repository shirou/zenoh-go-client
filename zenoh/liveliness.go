package zenoh

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/shirou/zenoh-go-client/internal/session"
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
	return s.enqueueDeclare(buildDeclareToken(id, keyExpr))
}

// sendDeclareTokenOn is the per-target variant for replay.
func (s *Session) sendDeclareTokenOn(target session.EntityReplayTarget, id uint32, keyExpr KeyExpr) error {
	return s.enqueueDeclareOn(target, buildDeclareToken(id, keyExpr))
}

func buildDeclareToken(id uint32, keyExpr KeyExpr) *wire.Declare {
	return &wire.Declare{Body: wire.NewDeclareToken(id, keyExpr.toWire())}
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

// ----------------------------------------------------------------------------
// Liveliness Subscriber
// ----------------------------------------------------------------------------

// LivelinessSubscriberOptions controls DeclareSubscriber.
type LivelinessSubscriberOptions struct {
	// History asks the router to deliver every currently-alive matching
	// token at declare time (Mode=CurrentFuture). Without it the
	// subscriber starts from the Future only.
	History bool
}

// LivelinessSubscriber receives a Sample for every D_TOKEN (as Put) and
// U_TOKEN (as Delete) on a matching key expression. Always call Drop.
type LivelinessSubscriber struct {
	session    *Session
	keyExpr    KeyExpr
	interestID uint32
	history    bool
	handle     any
	dropFn     func()
	dropped    atomic.Bool
}

// DeclareSubscriber registers a liveliness subscriber on keyExpr and
// emits an INTEREST so the router starts delivering matching D_TOKEN /
// U_TOKEN messages. With opts.History=true the router also replays
// currently-alive tokens.
func (l *Liveliness) DeclareSubscriber(keyExpr KeyExpr, handler Handler[Sample], opts *LivelinessSubscriberOptions) (*LivelinessSubscriber, error) {
	s := l.session
	if s.closed.Load() {
		return nil, ErrSessionClosed
	}
	if keyExpr.IsZero() {
		return nil, ErrInvalidKeyExpr
	}
	history := opts != nil && opts.History

	deliver, handlerDrop, handle := handler.Attach()
	id := s.inner.IDs().AllocInterestID()

	internalDeliver := func(ev session.LivelinessEvent) {
		deliver(livelinessEventToSample(ev))
	}
	s.inner.RegisterLivelinessSubscriber(id, keyExpr.internalKeyExpr(), internalDeliver)

	s.livelinessSubsMu.Lock()
	s.livelinessSubsReplay[id] = livelinessSubReplayState{keyExpr: keyExpr, history: history}
	s.livelinessSubsMu.Unlock()

	if err := s.sendLivelinessSubscriberInterest(id, keyExpr, history); err != nil {
		s.inner.UnregisterLivelinessSubscriber(id)
		s.livelinessSubsMu.Lock()
		delete(s.livelinessSubsReplay, id)
		s.livelinessSubsMu.Unlock()
		handlerDrop()
		return nil, fmt.Errorf("send liveliness INTEREST: %w", err)
	}
	return &LivelinessSubscriber{
		session:    s,
		keyExpr:    keyExpr,
		interestID: id,
		history:    history,
		handle:     handle,
		dropFn:     handlerDrop,
	}, nil
}

// KeyExpr returns the subscriber's bound key expression.
func (l *LivelinessSubscriber) KeyExpr() KeyExpr { return l.keyExpr }

// Handler returns the user-facing handle from Handler.Attach (a
// <-chan Sample for FifoChannel/RingChannel, nil for Closure).
func (l *LivelinessSubscriber) Handler() any { return l.handle }

// Drop emits INTEREST Final for the subscriber's interest_id and releases
// the dispatcher. Safe to call multiple times.
func (l *LivelinessSubscriber) Drop() {
	if !l.dropped.CompareAndSwap(false, true) {
		return
	}
	l.session.inner.UnregisterLivelinessSubscriber(l.interestID)
	l.session.livelinessSubsMu.Lock()
	delete(l.session.livelinessSubsReplay, l.interestID)
	l.session.livelinessSubsMu.Unlock()
	_ = l.session.sendInterestFinal(l.interestID)
	if l.dropFn != nil {
		l.dropFn()
	}
}

// sendLivelinessSubscriberInterest emits INTEREST[Mode=CurrentFuture|Future,
// Filter=KeyExprs+Tokens, restricted=ke].
func (s *Session) sendLivelinessSubscriberInterest(id uint32, ke KeyExpr, history bool) error {
	return s.enqueueControl(buildLivelinessSubscriberInterest(id, ke, history))
}

// sendLivelinessSubscriberInterestOn is the per-target variant for replay.
func (s *Session) sendLivelinessSubscriberInterestOn(target session.EntityReplayTarget, id uint32, ke KeyExpr, history bool) error {
	return s.enqueueControlOn(target, buildLivelinessSubscriberInterest(id, ke, history))
}

func buildLivelinessSubscriberInterest(id uint32, ke KeyExpr, history bool) *wire.Interest {
	mode := wire.InterestModeFuture
	if history {
		mode = wire.InterestModeCurrentFuture
	}
	wexpr := ke.toWire()
	return &wire.Interest{
		InterestID: id,
		Mode:       mode,
		Filter:     wire.InterestFilter{KeyExprs: true, Tokens: true},
		KeyExpr:    &wexpr,
	}
}

// livelinessEventToSample turns an internal event into a public Sample.
// Tokens carry no payload / encoding / timestamp so only kind + keyExpr
// are populated — kind=Put on alive, Delete on gone, mirroring
// zenoh-rust's LivelinessSubscriber API.
func livelinessEventToSample(ev session.LivelinessEvent) Sample {
	s := Sample{keyExpr: KeyExpr{inner: ev.ParsedKeyExpr}}
	if ev.IsAlive {
		s.kind = SampleKindPut
	} else {
		s.kind = SampleKindDelete
	}
	return s
}

// livelinessSubReplayState is the zenoh-layer shadow of a live
// subscriber so reconnect can re-emit its INTEREST.
type livelinessSubReplayState struct {
	keyExpr KeyExpr
	history bool
}

// ----------------------------------------------------------------------------
// Liveliness Get
// ----------------------------------------------------------------------------

// LivelinessGetOptions controls Session.Liveliness().Get.
type LivelinessGetOptions struct {
	// Timeout bounds how long the caller waits for replies. Zero means
	// no client-side limit; a hung router will keep the Get open until
	// the session closes.
	Timeout time.Duration
	// Buffer is the reply-channel capacity; 0 → default 16.
	Buffer int
}

// Get queries currently-alive liveliness tokens matching keyExpr. Returns
// a channel that yields one Reply per matching token, then closes when
// the router sends DECLARE Final for this Get's interest_id (or on
// timeout / session close).
func (l *Liveliness) Get(keyExpr KeyExpr, opts *LivelinessGetOptions) (<-chan Reply, error) {
	return l.GetWithContext(context.Background(), keyExpr, opts)
}

// GetWithContext is Get with a caller-supplied context. Cancelling ctx
// closes the reply channel on the next reply boundary.
func (l *Liveliness) GetWithContext(ctx context.Context, keyExpr KeyExpr, opts *LivelinessGetOptions) (<-chan Reply, error) {
	s := l.session
	if s.closed.Load() {
		return nil, ErrSessionClosed
	}
	if keyExpr.IsZero() {
		return nil, ErrInvalidKeyExpr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	bufSize := 16
	var timeout time.Duration
	if opts != nil {
		if opts.Buffer > 0 {
			bufSize = opts.Buffer
		}
		if opts.Timeout > 0 {
			timeout = opts.Timeout
		}
	}

	id := s.inner.IDs().AllocInterestID()
	inboundCh := s.inner.RegisterLivelinessQuery(id, bufSize)

	wexpr := keyExpr.toWire()
	msg := &wire.Interest{
		InterestID: id,
		Mode:       wire.InterestModeCurrent,
		Filter:     wire.InterestFilter{KeyExprs: true, Tokens: true},
		KeyExpr:    &wexpr,
	}
	if err := s.enqueueControl(msg); err != nil {
		s.inner.CancelLivelinessQuery(id)
		return nil, fmt.Errorf("send liveliness INTEREST: %w", err)
	}

	out := make(chan Reply, bufSize)
	translatorExited := make(chan struct{})
	if ctx.Done() != nil || timeout > 0 {
		go s.runLivelinessCancel(ctx, timeout, id, translatorExited)
	}
	go func() {
		defer close(out)
		defer close(translatorExited)
		streamRepliesGeneric(ctx, inboundCh, out, func(ev session.LivelinessEvent) (Reply, bool) {
			ke := KeyExpr{inner: ev.ParsedKeyExpr}
			return Reply{
				keyExpr:   ke,
				sample:    Sample{keyExpr: ke, kind: SampleKindPut},
				hasSample: true,
			}, true
		})
	}()
	return out, nil
}

// runLivelinessCancel fires CancelLivelinessQuery on ctx cancel or
// timeout; no-op when the translator exited first. Uses the shared
// runCancelWatcher so the ctx/timer/exited race logic stays in one place.
func (s *Session) runLivelinessCancel(ctx context.Context, timeout time.Duration, id uint32, translatorExited <-chan struct{}) {
	runCancelWatcher(ctx, timeout, translatorExited, func() { s.inner.CancelLivelinessQuery(id) })
}
