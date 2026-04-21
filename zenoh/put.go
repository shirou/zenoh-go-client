package zenoh

import (
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
// is PriorityControl, which is a legitimate value — not "default"). Phase
// 6+ migrates these to option.Option[T] per the design doc.
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
// be nil to use defaults.
func (s *Session) Put(keyExpr KeyExpr, payload ZBytes, opts *PutOptions) error {
	if s.closed.Load() {
		return ErrSessionClosed
	}
	if keyExpr.IsZero() {
		return ErrInvalidKeyExpr
	}
	return s.enqueuePush(keyExpr, &wire.PutBody{
		Payload:  payload.unsafeBytes(),
		Encoding: resolveEncoding(opts),
	}, opts)
}

// Delete emits a DEL sample on keyExpr.
func (s *Session) Delete(keyExpr KeyExpr, opts *PutOptions) error {
	if s.closed.Load() {
		return ErrSessionClosed
	}
	if keyExpr.IsZero() {
		return ErrInvalidKeyExpr
	}
	return s.enqueuePush(keyExpr, &wire.DelBody{}, opts)
}

func (s *Session) enqueuePush(keyExpr KeyExpr, body wire.PushBody, opts *PutOptions) error {
	push := &wire.Push{KeyExpr: keyExpr.toWire(), Body: body}
	prio, reliable, express := pushQoS(opts)
	return s.enqueueNetwork(push, prio, reliable, express)
}

// enqueueNetwork encodes msg, wraps it in an OutboundMessage with the
// given routing metadata, and sends it on the current runtime's writer
// channel. Returns ErrSessionNotReady when the session is between
// Runtimes (reconnect in progress) and ErrConnectionLost when the link
// drops mid-send.
//
// Common backbone for Put, Declare, and any other network-layer emission.
func (s *Session) enqueueNetwork(msg codec.Encoder, prio wire.QoSPriority, reliable, express bool) (err error) {
	rt := s.snapshotRuntime()
	if rt == nil {
		return ErrSessionNotReady
	}
	encoded, err := transport.EncodeNetworkMessage(msg)
	if err != nil {
		return err
	}
	// The runtime orchestrator closes OutQ strictly after LinkClosed
	// closes, so in the select below we observe LinkClosed first and
	// bail — almost always. The tight window where LinkClosed is already
	// closed AND the orchestrator has started close(OutQ) can still
	// surface as "send on closed channel". We convert that to
	// ErrConnectionLost rather than propagating the panic.
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
