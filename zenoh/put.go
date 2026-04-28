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
	// Local self-delivery, matching rust's default Locality::Any: the
	// source session dispatches to its own matching subscribers before
	// (or instead of) any wire echo. zenohd's egress_filter prevents
	// the wire roundtrip from re-firing them; in peer-multicast our
	// LoopbackDisabled+self-ZID guards do the same. The source is our
	// own ZID — used by the matching dedup if a remote subscriber path
	// later observes a same-(srcZID,id) U_SUBSCRIBER from us.
	dispatchLocalPush(s, keyExpr, body)
	return s.enqueueNetwork(ctx, push, prio, reliable, express)
}

// dispatchLocalPush builds a PushSample from the same body bytes the
// wire side carries and feeds it to the inner session's local
// subscriber registry.
func dispatchLocalPush(s *Session, keyExpr KeyExpr, body wire.PushBody) {
	sample := session.PushSample{
		KeyExpr:       keyExpr.String(),
		ParsedKeyExpr: keyExpr.internalKeyExpr(),
		SourceZID:     s.zid.ToWireID(),
	}
	switch b := body.(type) {
	case *wire.PutBody:
		sample.Kind = wire.IDDataPut
		sample.Payload = b.Payload
		sample.Encoding = b.Encoding
		sample.Timestamp = b.Timestamp
	case *wire.DelBody:
		sample.Kind = wire.IDDataDel
		sample.Timestamp = b.Timestamp
	default:
		return
	}
	s.inner.DispatchLocalPush(keyExpr.internalKeyExpr(), sample)
}

// enqueueControl emits a control-plane message on the Control priority
// lane, reliable, non-express. Always uses a background ctx because
// declare / tear-down / INTEREST sends must not be abandoned mid-wire;
// cancellation is via Session.Close (propagated through LinkClosed).
//
// In peer mode the message is broadcast to every live Runtime so all
// connected peers see the declaration; in client mode there is exactly
// one Runtime and the behaviour matches the pre-peer-mode hot path.
func (s *Session) enqueueControl(msg codec.Encoder) error {
	return s.enqueueNetwork(context.Background(), msg, wire.QoSPriorityControl, true, false)
}

// enqueueControlOn is the per-target variant of enqueueControl, used by
// the reconnect-replay path so only the freshly-installed Link (or a
// freshly-discovered multicast peer) receives the catch-up
// declarations. The target is any session.EntityReplayTarget — both
// the unicast Runtime and the MulticastRuntime implement it.
func (s *Session) enqueueControlOn(target session.EntityReplayTarget, msg codec.Encoder) error {
	return s.enqueueNetworkOn(target, msg, wire.QoSPriorityControl, true, false)
}

// enqueueNetwork encodes msg, wraps it in an OutboundMessage with the
// given routing metadata, and broadcasts it to every reachable
// transport — every live unicast Runtime plus every multicast Runtime
// whose peer table contains at least one ZID we are not already
// reaching via unicast.
//
// Returns:
//   - ErrSessionNotReady when there is no transport at all (no unicast
//     Runtime AND no multicast Runtime configured).
//   - ErrConnectionLost when transports existed but every one of them
//     refused the send (link drops + every multicast OutQ full).
//   - ctx.Err() when the caller's context fired first.
//
// Multicast is best-effort: a full-OutQ drop is recorded against the
// runtime's Dropped() counter and surfaces as a missed delivery, but is
// never counted toward `delivered` and never causes this function to
// return an error on its own.
func (s *Session) enqueueNetwork(ctx context.Context, msg codec.Encoder, prio wire.QoSPriority, reliable, express bool) error {
	unicastRuntimes := s.snapshotAllRuntimes()
	multicastRuntimes := s.snapshotMulticastRuntimes()
	if len(unicastRuntimes) == 0 && len(multicastRuntimes) == 0 {
		return ErrSessionNotReady
	}
	encoded, err := transport.EncodeNetworkMessage(msg)
	if err != nil {
		return err
	}

	// Unicast fan-out, plus build the peer-ZID set the multicast skip
	// uses to detect "every multicast peer is also a unicast peer".
	unicastPeers := make(map[string]struct{}, len(unicastRuntimes))
	var firstErr error
	unicastDelivered := 0
	for _, rt := range unicastRuntimes {
		unicastPeers[string(rt.PeerZIDBytes())] = struct{}{}
		if err := s.deliverEncoded(rt, ctx, encoded, prio, reliable, express); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		unicastDelivered++
	}

	// Multicast fan-out with prefer-unicast skip. The skip suppresses
	// emissions where every group member is already reachable via a
	// dedicated unicast Runtime — that case the multicast datagram
	// would only produce duplicates. When the group has at least one
	// peer we don't reach over unicast, the emission goes out and the
	// unicast-reachable peers get a duplicate (acknowledged
	// limitation; receiver-side dedup is a follow-up).
	multicastDelivered := 0
	multicastAttempted := 0
	item := session.OutboundItem{NetworkMsg: &transport.OutboundMessage{
		Encoded:  encoded,
		Priority: prio,
		Reliable: reliable,
		Express:  express,
	}}
	for _, mrt := range multicastRuntimes {
		if multicastFullyOverlapsUnicast(mrt, unicastPeers) {
			continue
		}
		multicastAttempted++
		if mrt.Enqueue(item) {
			multicastDelivered++
		}
	}

	delivered := unicastDelivered + multicastDelivered
	if delivered > 0 {
		// Don't silently swallow a unicast send failure just because a
		// multicast emission covered for it: callers debugging "why
		// didn't peer X get my Put" still need to see this. Best-effort
		// log only — the caller sees nil because at least one transport
		// accepted the bytes.
		if firstErr != nil {
			s.inner.Logger().Warn("enqueueNetwork: unicast send failed; multicast may cover",
				"err", firstErr,
				"unicast_runtimes", len(unicastRuntimes),
				"unicast_delivered", unicastDelivered,
				"multicast_delivered", multicastDelivered)
		}
		return nil
	}
	// No bytes made it onto any wire. Prefer surfacing the unicast
	// error (more actionable for the caller); fall back to a generic
	// ErrConnectionLost in the multicast-only-and-everything-dropped
	// case so we don't lie about delivery.
	if firstErr != nil {
		return firstErr
	}
	if multicastAttempted > 0 {
		return ErrConnectionLost
	}
	// Every multicast runtime was skipped because it overlapped unicast,
	// AND every unicast send failed (firstErr is nil only when no
	// unicast was attempted). The remaining case is "no unicast +
	// multicast all-skipped", which means there were peers but
	// reaching them via multicast would have been pure duplicates of
	// successful unicast sends — but unicastDelivered==0 here, so
	// nothing actually reached the wire. Surface ErrConnectionLost.
	return ErrConnectionLost
}

// multicastFullyOverlapsUnicast reports whether every peer currently
// known to mrt is also reachable through a unicast Runtime — in which
// case multicast emission would be a pure duplicate. An empty peer
// table is treated as "no overlap" (we still want to emit so freshly
// joining peers see DECLAREs).
func multicastFullyOverlapsUnicast(mrt *session.MulticastRuntime, unicastPeers map[string]struct{}) bool {
	peers := mrt.Table().Snapshot()
	if len(peers) == 0 {
		return false
	}
	for _, p := range peers {
		if _, ok := unicastPeers[string(p.ZID.Bytes)]; !ok {
			return false
		}
	}
	return true
}

// enqueueNetworkOn sends msg to a specific replay target. Used by the
// reconnect replay path and peer-mode "new (multicast or unicast) peer
// just appeared, catch it up". Returns ErrConnectionLost when the
// target's queue refused the item (link drop, or a full multicast
// OutQ).
func (s *Session) enqueueNetworkOn(target session.EntityReplayTarget, msg codec.Encoder, prio wire.QoSPriority, reliable, express bool) error {
	if target == nil {
		return ErrSessionNotReady
	}
	encoded, err := transport.EncodeNetworkMessage(msg)
	if err != nil {
		return err
	}
	item := session.OutboundItem{NetworkMsg: &transport.OutboundMessage{
		Encoded:  encoded,
		Priority: prio,
		Reliable: reliable,
		Express:  express,
	}}
	if !target.Enqueue(item) {
		return ErrConnectionLost
	}
	return nil
}

// deliverEncoded performs the actual OutQ send. Encoded is shared across
// every runtime in a broadcast — the batcher reads but does not mutate it.
func (s *Session) deliverEncoded(rt *session.Runtime, ctx context.Context, encoded []byte, prio wire.QoSPriority, reliable, express bool) (err error) {
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
