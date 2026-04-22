package session

import (
	"bytes"
	"fmt"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/keyexpr"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// NetworkDispatcher is the InboundDispatch a Session uses when wired into
// the reader loop. It decodes network messages and routes them to the
// appropriate registry (subscribers, queryables, in-flight gets, ...).
func (s *Session) NetworkDispatcher() InboundDispatch {
	return func(h codec.Header, r *codec.Reader) error {
		switch h.ID {
		case wire.IDNetworkPush:
			push, err := wire.DecodePush(r, h)
			if err != nil {
				return fmt.Errorf("dispatch PUSH: %w", err)
			}
			return s.onPush(push)
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
			return s.onDeclare(d)
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

// onDeclare routes an inbound DECLARE message. Handled sub-messages:
//   - D_KEYEXPR / U_KEYEXPR maintain the receive-side alias table so any
//     later WireExpr with Scope != 0 can be resolved.
//   - D_TOKEN / U_TOKEN fan out to matching liveliness subscribers and
//     (when the DECLARE carries an interest_id from a live liveliness Get)
//     to that query's reply channel.
//   - D_FINAL with an interest_id closes the matching liveliness Get's
//     reply channel.
//
// Intentionally dropped without logging:
//   - D_SUBSCRIBER / U_SUBSCRIBER
//   - D_QUERYABLE / U_QUERYABLE
//   - D_INTEREST / D_ENTITY for kinds other than Token
//
// A router advertising its own subscribers / queryables back to a
// client-mode session is valid wire traffic but has no effect on us;
// logging each one would be noise on a busy network. Extending any of
// these to routed / peer mode is a separate feature task.
func (s *Session) onDeclare(d *wire.Declare) error {
	switch body := d.Body.(type) {
	case *wire.DeclareKeyExpr:
		s.declareRemoteAlias(body.ExprID, body.Scope, body.Suffix)
	case *wire.UndeclareKeyExpr:
		s.undeclareRemoteAlias(body.ExprID)
	case *wire.DeclareEntity:
		if body.Kind == wire.IDDeclareToken {
			return s.onTokenDeclare(d, body)
		}
	case *wire.UndeclareEntity:
		if body.Kind == wire.IDUndeclareToken {
			return s.onTokenUndeclare(body)
		}
	case *wire.DeclareFinal:
		if d.HasInterestID {
			s.finaliseLivelinessQuery(d.InterestID)
		}
	}
	return nil
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

// onPush builds a PushSample from the decoded PUSH and routes it to every
// matching subscriber.
func (s *Session) onPush(p *wire.Push) error {
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

	sample := PushSample{KeyExpr: full, ParsedKeyExpr: ke}
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
