package session

import (
	"errors"
	"log/slog"
	"net"
	"runtime/debug"

	"github.com/shirou/zenoh-go-client/internal/transport"
)

// logWriteErr downgrades expected-during-shutdown write errors to Debug.
// A "use of closed network connection" here means triggerShutdown already
// ran — WARNing about it is just noise on the normal close path.
func logWriteErr(logger *slog.Logger, msg string, err error) {
	if errors.Is(err, net.ErrClosed) {
		logger.Debug(msg+" (link closed during shutdown)", "err", err)
		return
	}
	logger.Warn(msg, "err", err)
}

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
// when closing fires; outQ is never closed (the orchestrator coordinates
// shutdown through closing alone to avoid a close-vs-send race against
// external Put senders).
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
		case item := <-outQ:
			if writeDead {
				continue
			}
			if err := handleItem(item, batcher, link); err != nil {
				logWriteErr(logger, "writer: link write failed, draining outQ", err)
				writeDead = true
				continue
			}
			// Coalesce: opportunistically drain items already queued, then
			// flush. Bursts batch together; idle single messages still
			// reach the wire promptly.
			writeDead = drainAvailable(outQ, batcher, link, logger, writeDead)
		case <-closing:
			return
		}
	}
}

// drainAvailable non-blockingly consumes items already sitting in outQ,
// forwarding each through handleItem. Returns the propagated "link write
// failed" latch.
func drainAvailable(
	outQ <-chan OutboundItem,
	batcher *transport.Batcher,
	link transport.Link,
	logger *slog.Logger,
	writeDead bool,
) bool {
	for {
		select {
		case item := <-outQ:
			if writeDead {
				continue
			}
			if err := handleItem(item, batcher, link); err != nil {
				logWriteErr(logger, "writer: link write failed, draining outQ", err)
				writeDead = true
			}
		default:
			if !writeDead {
				if err := batcher.FlushAll(); err != nil {
					logWriteErr(logger, "writer: link flush failed", err)
					writeDead = true
				}
			}
			return writeDead
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
