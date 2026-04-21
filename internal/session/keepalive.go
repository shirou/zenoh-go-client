package session

import (
	"bytes"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// keepaliveLoop sends a KEEPALIVE at the negotiated cadence
// (peerLeaseMs / divisor) so the peer's lease-watchdog doesn't expire us.
//
// KEEPALIVE is a transport-layer message and is emitted as its own framed
// batch (not inside a FRAME) via the RawBatch path of the outbound queue.
func keepaliveLoop(
	outQ chan<- OutboundItem,
	peerLeaseMs uint64,
	divisor int,
	closing <-chan struct{},
	logger *slog.Logger,
) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("keepalive panicked", "panic", r, "stack", string(debug.Stack()))
		}
	}()

	if divisor <= 0 {
		divisor = 4
	}
	// Clamp to 50 ms so a small peer lease (e.g. 100 ms with divisor 4 =
	// 25 ms) doesn't pin a CPU ticking.
	interval := max(time.Duration(peerLeaseMs)*time.Millisecond/time.Duration(divisor), 50*time.Millisecond)
	if peerLeaseMs == 0 {
		logger.Warn("keepalive: peerLeaseMs=0, disabling")
		return
	}

	encoded, err := encodeKeepAlive()
	if err != nil {
		logger.Error("keepalive: encode failed at startup", "err", err)
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-closing:
			return
		case <-ticker.C:
			select {
			case outQ <- OutboundItem{RawBatch: encoded}:
			case <-closing:
				return
			}
		}
	}
}

// encodeKeepAlive produces the KEEPALIVE transport message bytes, ready to
// be written as a stream batch. The encoding is deterministic (empty body,
// no extensions) so we compute it once and reuse.
func encodeKeepAlive() ([]byte, error) {
	w := codec.NewWriter(4)
	if err := (&wire.KeepAlive{}).EncodeTo(w); err != nil {
		return nil, err
	}
	return bytes.Clone(w.Bytes()), nil
}
