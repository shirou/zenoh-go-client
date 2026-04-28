package session

import (
	"fmt"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// MulticastDispatcher returns the inbound dispatcher the multicast
// runtime installs. It mirrors NetworkDispatcher for the message
// types that make sense on a best-effort multicast group:
//
//   - PUSH and DECLARE are routed through the normal session
//     handlers, with the originating peer's ZID stamped onto the
//     PushSample so future per-peer accounting work can use it.
//   - REQUEST / RESPONSE / RESPONSE_FINAL / INTEREST are explicitly
//     out of scope for multicast pub/sub propagation; they are
//     dropped with a Debug log so a misconfigured peer (or a future
//     extension) is visible in the trace but does not crash dispatch.
//   - Network OAM is consumed and ignored, matching unicast.
func (s *Session) MulticastDispatcher() MulticastInboundDispatch {
	return func(srcZID wire.ZenohID, h codec.Header, r *codec.Reader) error {
		switch h.ID {
		case wire.IDNetworkPush:
			push, err := wire.DecodePush(r, h)
			if err != nil {
				return fmt.Errorf("multicast dispatch PUSH: %w", err)
			}
			return s.onPushFromPeer(push, srcZID)
		case wire.IDNetworkDeclare:
			d, err := wire.DecodeDeclare(r, h)
			if err != nil {
				return fmt.Errorf("multicast dispatch DECLARE: %w", err)
			}
			return s.onDeclareFromPeer(d, srcZID)
		case wire.IDNetworkRequest, wire.IDNetworkResponse,
			wire.IDNetworkResponseFinal, wire.IDNetworkInterest:
			// Multicast queries are not in the supported feature set.
			// Consume the body so the reader can advance to the next
			// network message in the same FRAME.
			_ = r.Skip(r.Len())
			s.logger.Debug("multicast: dropping query/interest message",
				"src", srcZID, "msg_id", h.ID)
			return nil
		case wire.IDNetworkOAM:
			_ = r.Skip(r.Len())
			return nil
		default:
			return fmt.Errorf("multicast dispatch: unexpected msg id=%#x", h.ID)
		}
	}
}
