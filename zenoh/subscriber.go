package zenoh

import (
	"fmt"
	"sync/atomic"

	"github.com/shirou/zenoh-go-client/internal/session"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// Subscriber is a receiver bound to a key expression. Construct via
// Session.DeclareSubscriber.
//
// Always call Drop when done. Forgetting to Drop leaks the dispatcher
// goroutine(s) owned by the Handler (Closure keeps one running per
// subscriber; Fifo/Ring leak the delivery channel). There is no finalizer
// fallback — Drop is mandatory.
type Subscriber struct {
	session *Session
	keyExpr KeyExpr
	id      uint32

	// handle is whatever the Handler.Attach returned — typically a
	// <-chan Sample for FifoChannel / RingChannel, nil for Closure.
	handle  any
	dropFn  func()
	dropped atomic.Bool
}

// DeclareSubscriber registers a subscriber on keyExpr. Incoming PUSH
// messages that intersect this key are delivered to the given handler.
func (s *Session) DeclareSubscriber(keyExpr KeyExpr, handler Handler[Sample]) (*Subscriber, error) {
	if s.closed.Load() {
		return nil, ErrSessionClosed
	}
	if keyExpr.IsZero() {
		return nil, ErrInvalidKeyExpr
	}

	deliver, handlerDrop, handle := handler.Attach()
	id := s.inner.IDs().AllocSubsID()

	// Adapter: internal PushSample → public Sample → user deliver.
	internalDeliver := func(p session.PushSample) {
		deliver(buildSample(p))
	}
	s.inner.RegisterSubscriber(id, keyExpr.internalKeyExpr(), internalDeliver)

	// Emit D_SUBSCRIBER to the peer so the router starts forwarding PUSH
	// messages matching this key.
	if err := s.sendDeclareSubscriber(id, keyExpr); err != nil {
		s.inner.UnregisterSubscriber(id)
		handlerDrop()
		return nil, fmt.Errorf("send D_SUBSCRIBER: %w", err)
	}

	return &Subscriber{
		session: s,
		keyExpr: keyExpr,
		id:      id,
		handle:  handle,
		dropFn:  handlerDrop,
	}, nil
}

// KeyExpr returns the subscriber's bound key expression.
func (s *Subscriber) KeyExpr() KeyExpr { return s.keyExpr }

// Handler returns the user-facing handle from the Handler.Attach call.
// For FifoChannel/RingChannel this is a <-chan Sample; for Closure it is nil.
func (s *Subscriber) Handler() any { return s.handle }

// Drop undeclares the subscriber and releases its dispatcher goroutine.
// Safe to call multiple times; subsequent calls are no-ops.
func (s *Subscriber) Drop() {
	if !s.dropped.CompareAndSwap(false, true) {
		return
	}
	s.session.inner.UnregisterSubscriber(s.id)
	// Best-effort U_SUBSCRIBER emission. If the link is already gone, the
	// router cleaned up when our session dropped.
	_ = s.session.sendUndeclareSubscriber(s.id, s.keyExpr)
	if s.dropFn != nil {
		s.dropFn()
	}
}

// sendDeclareSubscriber encodes a DECLARE[D_SUBSCRIBER] and queues it for
// transmission on the session's control priority.
func (s *Session) sendDeclareSubscriber(id uint32, keyExpr KeyExpr) error {
	msg := &wire.Declare{
		Body: wire.NewDeclareSubscriber(id, keyExpr.toWire()),
	}
	return s.enqueueDeclare(msg)
}

func (s *Session) sendUndeclareSubscriber(id uint32, keyExpr KeyExpr) error {
	msg := &wire.Declare{
		Body: &wire.UndeclareEntity{
			Kind:     wire.IDUndeclareSubscriber,
			EntityID: id,
			WireExpr: keyExpr.toWire(),
		},
	}
	return s.enqueueDeclare(msg)
}

func (s *Session) enqueueDeclare(msg *wire.Declare) error {
	return s.enqueueControl(msg)
}

// sendDeclareSubscriberOn is the per-target variant used by the
// reconnect-replay and the multicast new-peer-discovered paths so a
// freshly-installed Link or multicast group sees D_SUBSCRIBER for every
// existing local subscriber.
func (s *Session) sendDeclareSubscriberOn(target session.EntityReplayTarget, id uint32, keyExpr KeyExpr) error {
	return s.enqueueDeclareOn(target, &wire.Declare{
		Body: wire.NewDeclareSubscriber(id, keyExpr.toWire()),
	})
}

func (s *Session) enqueueDeclareOn(target session.EntityReplayTarget, msg *wire.Declare) error {
	return s.enqueueControlOn(target, msg)
}

// buildSample converts an internal PushSample to a public Sample. The
// internal PushSample.Payload may alias the reader buffer, so we copy.
// ParsedKeyExpr is reused directly to avoid re-parsing a string the
// dispatcher already validated.
func buildSample(p session.PushSample) Sample {
	out := Sample{
		keyExpr: KeyExpr{inner: p.ParsedKeyExpr},
		payload: NewZBytes(p.Payload),
	}
	if p.Kind == wire.IDDataDel {
		out.kind = SampleKindDelete
	} else {
		out.kind = SampleKindPut
	}
	if p.Encoding != nil {
		out.encoding = encodingFromWire(*p.Encoding)
		out.hasEnc = true
	}
	if p.Timestamp != nil {
		out.ts = NewTimeStamp(p.Timestamp.NTP64, IdFromWireID(p.Timestamp.ZID))
		out.hasTS = true
	}
	return out
}
