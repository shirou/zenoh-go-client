package session

import (
	"fmt"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/keyexpr"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// NetworkDispatcher is the InboundDispatch a Session uses when wired into
// the reader loop. It decodes network messages and routes them to the
// appropriate registry (subscribers, in-flight gets, etc.).
//
// For Phase 4 only PUSH is handled; REQUEST/RESPONSE/DECLARE arrive but
// are currently acknowledged (consumed) and dropped, so the reader's
// "did you consume the body?" safety net doesn't trigger.
func (s *Session) NetworkDispatcher() InboundDispatch {
	return func(h codec.Header, r *codec.Reader) error {
		switch h.ID {
		case wire.IDNetworkPush:
			push, err := wire.DecodePush(r, h)
			if err != nil {
				return fmt.Errorf("dispatch PUSH: %w", err)
			}
			return s.onPush(push)
		case wire.IDNetworkDeclare:
			_, err := wire.DecodeDeclare(r, h)
			return err
		case wire.IDNetworkInterest:
			_, err := wire.DecodeInterest(r, h)
			return err
		case wire.IDNetworkRequest,
			wire.IDNetworkResponse,
			wire.IDNetworkResponseFinal,
			wire.IDNetworkOAM:
			// Not yet implemented; skip the rest of the FRAME body to
			// satisfy the reader's "consume your body" contract.
			_ = r.Skip(r.Len())
			return nil
		default:
			return fmt.Errorf("dispatch: unexpected network msg id=%#x", h.ID)
		}
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
