package session

import (
	"bytes"
	"fmt"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/keyexpr"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// NetworkDispatcherForPeer is the InboundDispatch a Session uses when
// wired into the reader loop. The peerZID identifies the remote face
// the link is bound to (zenohd's ZID in client mode, the remote peer's
// ZID in unicast peer mode); it's threaded into the matching dedup
// path so D_SUBSCRIBER / D_QUERYABLE entries from this peer don't
// collide with same-id entries from a different peer.
func (s *Session) NetworkDispatcherForPeer(peerZID wire.ZenohID) InboundDispatch {
	return func(h codec.Header, r *codec.Reader) error {
		switch h.ID {
		case wire.IDNetworkPush:
			push, err := wire.DecodePush(r, h)
			if err != nil {
				return fmt.Errorf("dispatch PUSH: %w", err)
			}
			return s.onPushFromPeer(push, peerZID)
		case wire.IDNetworkRequest:
			req, err := wire.DecodeRequest(r, h)
			if err != nil {
				return fmt.Errorf("dispatch REQUEST: %w", err)
			}
			return s.onRequest(req)
		case wire.IDNetworkResponse:
			res, err := wire.DecodeResponse(r, h)
			if err != nil {
				return fmt.Errorf("dispatch RESPONSE: %w", err)
			}
			return s.onResponse(res)
		case wire.IDNetworkResponseFinal:
			rf, err := wire.DecodeResponseFinal(r, h)
			if err != nil {
				return fmt.Errorf("dispatch RESPONSE_FINAL: %w", err)
			}
			s.finaliseGet(rf.RequestID)
			return nil
		case wire.IDNetworkDeclare:
			d, err := wire.DecodeDeclare(r, h)
			if err != nil {
				return fmt.Errorf("dispatch DECLARE: %w", err)
			}
			return s.onDeclareFromPeer(d, peerZID)
		case wire.IDNetworkInterest:
			_, err := wire.DecodeInterest(r, h)
			return err
		case wire.IDNetworkOAM:
			_ = r.Skip(r.Len())
			return nil
		default:
			return fmt.Errorf("dispatch: unexpected network msg id=%#x", h.ID)
		}
	}
}

// onRequest routes an inbound REQUEST to every matching queryable.
func (s *Session) onRequest(req *wire.Request) error {
	full, ok := s.resolveRemoteKey(req.KeyExpr)
	if !ok {
		s.logger.Warn("dispatch REQUEST with unknown ExprId alias",
			"scope", req.KeyExpr.Scope)
		return nil
	}
	ke, err := keyexpr.New(full)
	if err != nil {
		return fmt.Errorf("dispatch REQUEST: invalid key %q: %w", full, err)
	}
	qr := QueryReceived{
		RequestID:     req.RequestID,
		KeyExpr:       full,
		ParsedKeyExpr: ke,
	}
	if req.Body != nil {
		qr.Parameters = req.Body.Parameters
		if req.Body.Consolidation != nil {
			qr.Consolidation = *req.Body.Consolidation
		}
	}
	s.dispatchQuery(ke, qr)
	return nil
}

// onResponse routes a RESPONSE to the in-flight Get identified by RequestID.
//
// Payloads and encodings from wire.PutBody/DelBody/ErrBody alias the
// reader buffer, which is reused on the next batch read. We deep-copy
// everything here because the Get collector channel crosses goroutines
// (the in-flight Get's translator runs asynchronously).
func (s *Session) onResponse(res *wire.Response) error {
	full, _ := s.resolveRemoteKey(res.KeyExpr)
	reply := InboundReply{KeyExpr: full}
	if ke, err := keyexpr.New(full); err == nil {
		reply.ParsedKeyExpr = ke
	}
	switch body := res.Body.(type) {
	case *wire.ReplyBody:
		switch inner := body.Inner.(type) {
		case *wire.PutBody:
			reply.Put = clonePutBody(inner)
		case *wire.DelBody:
			reply.Del = cloneDelBody(inner)
		default:
			return fmt.Errorf("dispatch RESPONSE: unknown reply inner type %T", body.Inner)
		}
	case *wire.ErrBody:
		reply.Err = cloneErrBody(body)
	default:
		return fmt.Errorf("dispatch RESPONSE: unknown body type %T", res.Body)
	}
	s.deliverReply(res.RequestID, reply)
	return nil
}

// clonePutBody deep-copies the payload (which aliases the reader buffer)
// plus any Timestamp / Encoding pointers.
func clonePutBody(p *wire.PutBody) *wire.PutBody {
	return &wire.PutBody{
		Payload:   bytes.Clone(p.Payload),
		Encoding:  wire.CloneEncoding(p.Encoding),
		Timestamp: wire.CloneTimestamp(p.Timestamp),
	}
}

func cloneDelBody(d *wire.DelBody) *wire.DelBody {
	return &wire.DelBody{Timestamp: wire.CloneTimestamp(d.Timestamp)}
}

func cloneErrBody(e *wire.ErrBody) *wire.ErrBody {
	return &wire.ErrBody{
		Payload:  bytes.Clone(e.Payload),
		Encoding: wire.CloneEncoding(e.Encoding),
	}
}

// onDeclareFromPeer routes an inbound DECLARE message, threading the
// originating peer's ZID into the matching dedup path. Handled
// sub-messages:
//   - D_KEYEXPR / U_KEYEXPR maintain the receive-side alias table so any
//     later WireExpr with Scope != 0 can be resolved.
//   - D_SUBSCRIBER / U_SUBSCRIBER and D_QUERYABLE / U_QUERYABLE drive the
//     matching registry (Publisher / Querier MatchingListener).
//   - D_TOKEN / U_TOKEN fan out to matching liveliness subscribers and
//     (when the DECLARE carries an interest_id from a live liveliness Get)
//     to that query's reply channel.
//   - D_FINAL with an interest_id completes either a matching INTEREST's
//     initial snapshot or a liveliness Get's reply stream.
func (s *Session) onDeclareFromPeer(d *wire.Declare, srcZID wire.ZenohID) error {
	switch body := d.Body.(type) {
	case *wire.DeclareKeyExpr:
		s.declareRemoteAlias(body.ExprID, body.Scope, body.Suffix)
	case *wire.UndeclareKeyExpr:
		s.undeclareRemoteAlias(body.ExprID)
	case *wire.DeclareEntity:
		switch body.Kind {
		case wire.IDDeclareToken:
			return s.onTokenDeclare(d, body)
		case wire.IDDeclareSubscriber:
			return s.onMatchingEntityDeclare(MatchingSubscribers, srcZID, body)
		case wire.IDDeclareQueryable:
			return s.onMatchingEntityDeclare(MatchingQueryables, srcZID, body)
		}
	case *wire.UndeclareEntity:
		switch body.Kind {
		case wire.IDUndeclareToken:
			return s.onTokenUndeclare(body)
		case wire.IDUndeclareSubscriber:
			return s.onMatchingEntityUndeclare(MatchingSubscribers, srcZID, body)
		case wire.IDUndeclareQueryable:
			return s.onMatchingEntityUndeclare(MatchingQueryables, srcZID, body)
		}
	case *wire.DeclareFinal:
		if d.HasInterestID {
			// A DeclareFinal carrying an interest_id terminates exactly
			// one outstanding INTEREST. Try matching first (Publisher /
			// Querier); fall back to liveliness Get.
			if !s.onMatchingSnapshotDone(d.InterestID) {
				s.finaliseLivelinessQuery(d.InterestID)
			}
		}
	}
	return nil
}

// onMatchingEntityDeclare resolves the incoming key expression (honouring
// receive-side aliases) and feeds it into the matching registry.
func (s *Session) onMatchingEntityDeclare(kind MatchingKind, srcZID wire.ZenohID, body *wire.DeclareEntity) error {
	ke, err := s.resolveMatchingKey(body.KeyExpr, kind, "declare")
	if err != nil || ke.IsZero() {
		return err
	}
	s.onMatchingDeclare(kind, srcZID, body.EntityID, ke)
	return nil
}

func (s *Session) onMatchingEntityUndeclare(kind MatchingKind, srcZID wire.ZenohID, body *wire.UndeclareEntity) error {
	// The undeclare path is keyed by (srcZID, entityID); zenohd
	// commonly sends U_SUBSCRIBER / U_QUERYABLE with an empty
	// WireExpr because receivers are expected to look the entity up
	// by id. Don't bail when the WireExpr fails to resolve — pass a
	// zero KeyExpr through so the dedup-set walk in applyMatchingDelta
	// can still remove the right entries.
	ke, _ := s.resolveMatchingKey(body.WireExpr, kind, "undeclare")
	s.onMatchingUndeclare(kind, srcZID, body.EntityID, ke)
	return nil
}

func (s *Session) resolveMatchingKey(w wire.WireExpr, kind MatchingKind, op string) (keyexpr.KeyExpr, error) {
	full, ok := s.resolveRemoteKey(w)
	if !ok || full == "" {
		// Router sent an unresolvable alias. Log once and ignore; the next
		// D_KEYEXPR should repair the table.
		s.logger.Warn("dispatch: matching entity with unknown alias",
			"kind", kind, "op", op, "scope", w.Scope)
		return keyexpr.KeyExpr{}, nil
	}
	ke, err := keyexpr.New(full)
	if err != nil {
		return keyexpr.KeyExpr{}, fmt.Errorf("dispatch matching %s: invalid key %q: %w", op, full, err)
	}
	return ke, nil
}

func (s *Session) onTokenDeclare(d *wire.Declare, body *wire.DeclareEntity) error {
	full, ok := s.resolveRemoteKey(body.KeyExpr)
	if !ok {
		s.logger.Warn("dispatch D_TOKEN with unknown ExprId alias",
			"scope", body.KeyExpr.Scope)
		return nil
	}
	ke, err := keyexpr.New(full)
	if err != nil {
		return fmt.Errorf("dispatch D_TOKEN: invalid key %q: %w", full, err)
	}
	s.RememberInboundToken(body.EntityID, full)
	ev := LivelinessEvent{
		KeyExpr:       full,
		ParsedKeyExpr: ke,
		IsAlive:       true,
	}
	s.dispatchLiveliness(ke, ev)
	if d.HasInterestID {
		s.deliverLivelinessReply(d.InterestID, ev)
	}
	return nil
}

func (s *Session) onTokenUndeclare(body *wire.UndeclareEntity) error {
	// zenohd typically sends U_TOKEN with an empty WireExpr and relies
	// on the client having cached the D_TOKEN's entity_id → key mapping.
	// Try the local cache first; fall back to the WireExpr (possibly
	// aliased) if the entity wasn't seen.
	full, fromCache := s.ForgetInboundToken(body.EntityID)
	if !fromCache {
		resolved, ok := s.resolveRemoteKey(body.WireExpr)
		if !ok || resolved == "" {
			s.logger.Warn("dispatch U_TOKEN with no resolvable key",
				"entityID", body.EntityID, "scope", body.WireExpr.Scope)
			return nil
		}
		full = resolved
	}
	ke, err := keyexpr.New(full)
	if err != nil {
		return fmt.Errorf("dispatch U_TOKEN: invalid key %q: %w", full, err)
	}
	s.dispatchLiveliness(ke, LivelinessEvent{
		KeyExpr:       full,
		ParsedKeyExpr: ke,
		IsAlive:       false,
	})
	return nil
}

// onPushFromPeer builds a PushSample from the decoded PUSH and routes
// it to every matching subscriber. srcZID identifies the originating
// peer; populated for both unicast (the Runtime's peer ZID) and
// multicast (decoded from the datagram source).
func (s *Session) onPushFromPeer(p *wire.Push, srcZID wire.ZenohID) error {
	full, ok := s.resolveRemoteKey(p.KeyExpr)
	if !ok {
		s.logger.Warn("dispatch PUSH with unknown ExprId alias",
			"scope", p.KeyExpr.Scope)
		return nil
	}
	ke, err := keyexpr.New(full)
	if err != nil {
		return fmt.Errorf("dispatch PUSH: invalid key %q: %w", full, err)
	}

	sample := PushSample{KeyExpr: full, ParsedKeyExpr: ke, SourceZID: srcZID}
	switch body := p.Body.(type) {
	case *wire.PutBody:
		sample.Kind = wire.IDDataPut
		sample.Payload = body.Payload
		sample.Encoding = body.Encoding
		sample.Timestamp = body.Timestamp
	case *wire.DelBody:
		sample.Kind = wire.IDDataDel
		sample.Timestamp = body.Timestamp
	default:
		return fmt.Errorf("dispatch PUSH: unknown body type %T", p.Body)
	}
	s.dispatchPush(ke, sample)
	return nil
}
