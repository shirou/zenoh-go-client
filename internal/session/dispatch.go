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
			_, err := wire.DecodeDeclare(r, h)
			return err
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
	if req.KeyExpr.Scope != 0 {
		s.logger.Warn("dispatch REQUEST with ExprId alias; aliasing not yet supported",
			"scope", req.KeyExpr.Scope)
		return nil
	}
	ke, err := keyexpr.New(req.KeyExpr.Suffix)
	if err != nil {
		return fmt.Errorf("dispatch REQUEST: invalid key %q: %w", req.KeyExpr.Suffix, err)
	}
	qr := QueryReceived{
		RequestID:     req.RequestID,
		KeyExpr:       req.KeyExpr.Suffix,
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
	reply := InboundReply{KeyExpr: res.KeyExpr.Suffix}
	if ke, err := keyexpr.New(res.KeyExpr.Suffix); err == nil {
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

// onPush builds a PushSample from the decoded PUSH and routes it to every
// matching subscriber.
func (s *Session) onPush(p *wire.Push) error {
	// Resolve key expression. ExprId aliasing is not yet implemented, so we
	// expect scope==0 with the full suffix.
	if p.KeyExpr.Scope != 0 {
		// Peer sent an aliased ExprId we don't know. Drop silently (log).
		s.logger.Warn("dispatch PUSH with ExprId alias; aliasing not yet supported",
			"scope", p.KeyExpr.Scope)
		return nil
	}
	ke, err := keyexpr.New(p.KeyExpr.Suffix)
	if err != nil {
		return fmt.Errorf("dispatch PUSH: invalid key %q: %w", p.KeyExpr.Suffix, err)
	}

	sample := PushSample{KeyExpr: p.KeyExpr.Suffix, ParsedKeyExpr: ke}
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
