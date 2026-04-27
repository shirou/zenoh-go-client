package zenoh

import (
	"context"
	goruntime "runtime"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/session"
	"github.com/shirou/zenoh-go-client/internal/transport"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// PutOptions controls a Session.Put / Publisher.Put emission.
//
// Optional fields use a "Has" boolean because Go zero-value semantics
// would otherwise conflate "unset" with "value 0" (e.g. the zero Priority
// is PriorityControl, which is a legitimate value — not "default").
type PutOptions struct {
	Encoding          Encoding
	HasEncoding       bool
	Priority          Priority
	HasPriority       bool
	CongestionControl CongestionControl
	HasCongestion     bool
	IsExpress         bool
}

// Put publishes a PUT sample to keyExpr with the given payload. opts may
// be nil to use defaults. Equivalent to PutWithContext(context.Background(), …).
func (s *Session) Put(keyExpr KeyExpr, payload ZBytes, opts *PutOptions) error {
	return s.PutWithContext(context.Background(), keyExpr, payload, opts)
}

// PutWithContext is Put with a caller-supplied context. The context is
// checked before enqueue and, when the outbound queue is momentarily
// full, while waiting for a slot — so a slow consumer cannot pin the
// caller forever.
func (s *Session) PutWithContext(ctx context.Context, keyExpr KeyExpr, payload ZBytes, opts *PutOptions) error {
	if s.closed.Load() {
		return ErrSessionClosed
	}
	if keyExpr.IsZero() {
		return ErrInvalidKeyExpr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.enqueuePush(ctx, keyExpr, &wire.PutBody{
		Payload:  payload.unsafeBytes(),
		Encoding: resolveEncoding(opts),
	}, opts)
}

// Delete emits a DEL sample on keyExpr. Equivalent to
// DeleteWithContext(context.Background(), …).
func (s *Session) Delete(keyExpr KeyExpr, opts *PutOptions) error {
	return s.DeleteWithContext(context.Background(), keyExpr, opts)
}

// DeleteWithContext is Delete with a caller-supplied context.
func (s *Session) DeleteWithContext(ctx context.Context, keyExpr KeyExpr, opts *PutOptions) error {
	if s.closed.Load() {
		return ErrSessionClosed
	}
	if keyExpr.IsZero() {
		return ErrInvalidKeyExpr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.enqueuePush(ctx, keyExpr, &wire.DelBody{}, opts)
}

func (s *Session) enqueuePush(ctx context.Context, keyExpr KeyExpr, body wire.PushBody, opts *PutOptions) error {
	push := &wire.Push{KeyExpr: keyExpr.toWire(), Body: body}
	prio, reliable, express := pushQoS(opts)
	// Receiver-visible Sample.priority / congestion_control / express is
	// read from the Push-level QoS ext, not the Frame-level one. Omitted
	// on default values to match zenoh-rust's wire.
	if prio != wire.QoSPriorityDefault || reliable || express {
		q := wire.QoS{Priority: prio, DontDrop: reliable, Express: express}
		push.Extensions = []codec.Extension{codec.NewZ64Ext(wire.ExtIDQoS, false, q.EncodeZ64())}
	}
	return s.enqueueNetwork(ctx, push, prio, reliable, express)
}

// enqueueControl emits a control-plane message on the Control priority
// lane, reliable, non-express. Always uses a background ctx because
// declare / tear-down / INTEREST sends must not be abandoned mid-wire;
// cancellation is via Session.Close (propagated through LinkClosed).
func (s *Session) enqueueControl(msg codec.Encoder) error {
	return s.enqueueNetwork(context.Background(), msg, wire.QoSPriorityControl, true, false)
}

// enqueueNetwork encodes msg, wraps it in an OutboundMessage with the
// given routing metadata, and sends it on the current runtime's writer
// channel. Returns ErrSessionNotReady when the session is between
// Runtimes (reconnect in progress), ErrConnectionLost when the link
// drops mid-send, and ctx.Err() when ctx fires first.
//
// Common backbone for Put, Declare, and any other network-layer emission.
func (s *Session) enqueueNetwork(ctx context.Context, msg codec.Encoder, prio wire.QoSPriority, reliable, express bool) (err error) {
	rt := s.snapshotRuntime()
	if rt == nil {
		return ErrSessionNotReady
	}
	encoded, err := transport.EncodeNetworkMessage(msg)
	if err != nil {
		return err
	}
	// The orchestrator never closes OutQ — shutdown is signalled solely
	// through LinkClosed — so the select below cannot panic on send-to-
	// closed. The recover stays as a narrow safety net in case a future
	// regression reintroduces the close.
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(goruntime.Error); ok {
				err = ErrConnectionLost
				return
			}
			panic(r)
		}
	}()
	select {
	case rt.OutQ <- session.OutboundItem{NetworkMsg: &transport.OutboundMessage{
		Encoded:  encoded,
		Priority: prio,
		Reliable: reliable,
		Express:  express,
	}}:
		return nil
	case <-rt.LinkClosed:
		return ErrConnectionLost
	case <-ctx.Done():
		return ctx.Err()
	}
}

// resolveEncoding produces a *wire.Encoding from opts, or nil when no
// encoding is set.
func resolveEncoding(opts *PutOptions) *wire.Encoding {
	if opts == nil || !opts.HasEncoding {
		return nil
	}
	w := opts.Encoding.toWire()
	return &w
}

// pushQoS derives (priority, reliable, express) from PutOptions.
//
// Defaults (no opts or all unset): Data priority, best-effort, not express.
// HasCongestion + Block flips reliable=true (maps to D=1 on the wire QoS
// extension). Express bypasses batching.
func pushQoS(opts *PutOptions) (wire.QoSPriority, bool, bool) {
	prio := wire.QoSPriorityData
	reliable := false
	express := false
	if opts == nil {
		return prio, reliable, express
	}
	if opts.HasPriority {
		prio = wire.QoSPriority(opts.Priority)
	}
	if opts.HasCongestion && opts.CongestionControl == CongestionControlBlock {
		reliable = true
	}
	if opts.IsExpress {
		express = true
	}
	return prio, reliable, express
}
