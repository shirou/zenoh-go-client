package session

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/transport"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// InboundDispatch is the callback the reader invokes for each decoded
// network message header. The Reader is positioned immediately after the
// header byte; Dispatch MUST consume the complete message body (via the
// appropriate wire.Decode*) so the next iteration can advance.
//
// Returning an error stops batch processing; the reader treats it as a
// link-level error and exits.
type InboundDispatch func(h codec.Header, r *codec.Reader) error

// ReaderConfig bundles the stateful dependencies a reader loop needs.
type ReaderConfig struct {
	Link           transport.Link
	Reassembler    *transport.Reassembler
	Dispatch       InboundDispatch
	Logger         *slog.Logger
	LastRecvUnixMs *atomic.Int64 // set on every inbound batch for lease watchdog
	// OnClose is invoked if the peer sends a CLOSE. Receives the reason code.
	OnClose func(reason uint8)
}

// readerLoop reads batches from the link, decodes transport messages, and
// extracts network messages from FRAME / FRAGMENT bodies. Each completed
// network message is passed to Dispatch.
//
// Exits on: io.EOF (clean peer close), read error (triggers reconnect via
// link-level failure), or CLOSE from peer (triggers OnClose + clean exit).
func readerLoop(cfg ReaderConfig, closing <-chan struct{}) {
	defer func() {
		if r := recover(); r != nil {
			cfg.Logger.Error("reader panicked",
				"panic", r,
				"stack", string(debug.Stack()))
		}
	}()

	buf := make([]byte, codec.MaxBatchSize)
	for {
		select {
		case <-closing:
			return
		default:
		}

		n, err := cfg.Link.ReadBatch(buf)
		if err != nil {
			// If shutdown was already initiated, the link was closed under
			// us on purpose — don't WARN.
			shuttingDown := false
			select {
			case <-closing:
				shuttingDown = true
			default:
			}
			switch {
			case errors.Is(err, io.EOF):
				cfg.Logger.Debug("reader: peer closed link (EOF)")
			case shuttingDown || errors.Is(err, net.ErrClosed):
				cfg.Logger.Debug("reader: link closed during shutdown", "err", err)
			default:
				cfg.Logger.Warn("reader: link read failed", "err", err)
			}
			return
		}
		cfg.LastRecvUnixMs.Store(time.Now().UnixMilli())

		if err := processBatch(cfg, buf[:n]); err != nil {
			cfg.Logger.Warn("reader: batch decode failed", "err", err)
			// Malformed batch → close the link; the session-level
			// reconnect manager will re-establish.
			return
		}
	}
}

// processBatch decodes one stream batch: iterate transport messages, handle
// FRAME/FRAGMENT bodies and signalling messages.
func processBatch(cfg ReaderConfig, batch []byte) error {
	r := codec.NewReader(batch)
	for r.Len() > 0 {
		h, err := r.DecodeHeader()
		if err != nil {
			return fmt.Errorf("header: %w", err)
		}
		switch h.ID {
		case wire.IDTransportKeepAlive:
			if _, err := wire.DecodeKeepAlive(r, h); err != nil {
				return err
			}
		case wire.IDTransportClose:
			cl, err := wire.DecodeClose(r, h)
			if err != nil {
				return err
			}
			if cfg.OnClose != nil {
				cfg.OnClose(cl.Reason)
			}
			return nil
		case wire.IDTransportFrame:
			frame, err := wire.DecodeFrame(r, h)
			if err != nil {
				return fmt.Errorf("FRAME: %w", err)
			}
			if err := dispatchFrameBody(cfg, frame.Body); err != nil {
				return err
			}
			// FRAME consumed the rest of the batch by contract.
			return nil
		case wire.IDTransportFragment:
			frag, err := wire.DecodeFragment(r, h)
			if err != nil {
				return fmt.Errorf("FRAGMENT: %w", err)
			}
			lane := transport.LaneKey{
				Priority: laneKeyPriority(frag.Extensions),
				Reliable: frag.Reliable,
			}
			// Reassembler owns the body copy on completion.
			complete, err := cfg.Reassembler.Push(lane, frag.SeqNum, frag.More, frag.Body)
			if err != nil {
				cfg.Logger.Warn("reader: fragment reassembly reset", "err", err, "lane", lane)
				// Non-fatal: keep reading.
				return nil
			}
			if complete != nil {
				if err := dispatchFrameBody(cfg, complete); err != nil {
					return err
				}
			}
			return nil
		case wire.IDTransportOAM:
			// Transport OAM not yet decoded. Without a length we can't
			// reliably find the next message in the batch, so treat as a
			// protocol violation and close the link.
			return fmt.Errorf("transport OAM not supported (id=%#x)", h.ID)
		default:
			return fmt.Errorf("unexpected transport msg id=%#x inside active session", h.ID)
		}
	}
	return nil
}

// dispatchFrameBody iterates self-delimiting NetworkMessages inside a FRAME
// body and hands each off to the inbound Dispatch callback.
func dispatchFrameBody(cfg ReaderConfig, body []byte) error {
	r := codec.NewReader(body)
	for r.Len() > 0 {
		h, err := r.DecodeHeader()
		if err != nil {
			return fmt.Errorf("network header: %w", err)
		}
		before := r.Len()
		if err := cfg.Dispatch(h, r); err != nil {
			return err
		}
		// Safety net: if bytes remain but Dispatch didn't consume any,
		// we'd loop forever on the same header. Empty-body messages
		// legitimately leave r.Len() == 0 here, so only error when there
		// are unread bytes.
		if r.Len() > 0 && r.Len() == before {
			return fmt.Errorf("dispatch did not consume network message id=%#x", h.ID)
		}
	}
	return nil
}

// laneKeyPriority extracts the QoS priority from a FRAME/FRAGMENT extension
// chain. Falls back to the default Data priority if no QoS ext is present.
func laneKeyPriority(exts []codec.Extension) uint8 {
	if e := codec.FindExt(exts, wire.ExtIDQoS); e != nil && e.Header.Encoding == codec.ExtEncZ64 {
		return uint8(wire.DecodeQoSZ64(e.Z64).Priority)
	}
	return uint8(wire.QoSPriorityData)
}
