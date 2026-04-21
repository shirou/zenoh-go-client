package session

import (
	"log/slog"
	"runtime/debug"

	"github.com/shirou/zenoh-go-client/internal/transport"
)

// OutboundItem is the unit passed through the session's outbound queue.
// Exactly one of NetworkMsg / RawBatch is set:
//
//   - NetworkMsg: a NetworkMessage (PUSH/REQUEST/RESPONSE/DECLARE/INTEREST)
//     to be routed through the QoS batcher.
//   - RawBatch: a fully-framed transport-layer batch (KEEPALIVE, CLOSE,
//     OAM) that bypasses batching and is written directly to the link.
//     Writing a RawBatch first flushes any pending FRAME batches to
//     preserve ordering.
type OutboundItem struct {
	NetworkMsg *transport.OutboundMessage
	RawBatch   []byte
}

// writerLoop drains outQ, routes each item, and writes to the link. It exits
// when outQ is closed or closing fires.
func writerLoop(
	outQ <-chan OutboundItem,
	batcher *transport.Batcher,
	link transport.Link,
	closing <-chan struct{},
	logger *slog.Logger,
) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("writer panicked",
				"panic", r,
				"stack", string(debug.Stack()))
		}
		if err := batcher.FlushAll(); err != nil {
			logger.Debug("writer: final flush failed", "err", err)
		}
	}()

	// writeDead flips after the first write error. Subsequent items are
	// drained from outQ (so senders don't deadlock) but dropped silently
	// rather than re-attempting writes that will spin-log.
	writeDead := false

	for {
		select {
		case item, ok := <-outQ:
			if !ok {
				return
			}
			if writeDead {
				continue
			}
			if err := handleItem(item, batcher, link); err != nil {
				logger.Warn("writer: link write failed, draining outQ", "err", err)
				writeDead = true
				continue
			}
			// Coalesce: opportunistically drain items already queued, then
			// flush. Bursts batch together; idle single messages still
			// reach the wire promptly.
			var closed bool
			writeDead, closed = drainAvailable(outQ, batcher, link, logger, writeDead)
			if closed {
				return
			}
		case <-closing:
			return
		}
	}
}

// drainAvailable non-blockingly consumes items already sitting in outQ,
// forwarding each through handleItem. Returns (writeDead, queueClosed):
// queueClosed=true means the caller should exit; writeDead propagates the
// "link write failed" latch.
func drainAvailable(
	outQ <-chan OutboundItem,
	batcher *transport.Batcher,
	link transport.Link,
	logger *slog.Logger,
	writeDead bool,
) (dead, queueClosed bool) {
	for {
		select {
		case item, ok := <-outQ:
			if !ok {
				_ = batcher.FlushAll()
				return writeDead, true
			}
			if writeDead {
				continue
			}
			if err := handleItem(item, batcher, link); err != nil {
				logger.Warn("writer: link write failed, draining outQ", "err", err)
				writeDead = true
			}
		default:
			if !writeDead {
				if err := batcher.FlushAll(); err != nil {
					logger.Warn("writer: link flush failed", "err", err)
					writeDead = true
				}
			}
			return writeDead, false
		}
	}
}

func handleItem(item OutboundItem, batcher *transport.Batcher, link transport.Link) error {
	switch {
	case item.NetworkMsg != nil:
		return batcher.Enqueue(item.NetworkMsg)
	case item.RawBatch != nil:
		if err := batcher.FlushAll(); err != nil {
			return err
		}
		return link.WriteBatch(item.RawBatch)
	default:
		return nil // empty item
	}
}
