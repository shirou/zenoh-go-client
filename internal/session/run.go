package session

import (
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/transport"
)

// RunConfig bundles everything Session.Run needs to start the per-session
// goroutines once a link is up and a handshake has completed.
type RunConfig struct {
	Link      transport.Link
	Result    *HandshakeResult
	OutQSize  int             // buffered size of the outbound item queue
	MaxMsgSz  int             // cap for FRAGMENT reassembly
	KeepAlive int             // peer lease divisor (defaults to 4)
	Dispatch  InboundDispatch // where incoming network messages go; nil = drop
}

// Runtime is the bundle of primitives Session.Run installs so the session
// (and the public API built on top) can send messages, observe shutdown,
// and react to peer events.
type Runtime struct {
	OutQ       chan<- OutboundItem
	Result     *HandshakeResult
	LinkClosed <-chan struct{} // closed after reader+writer have exited
	PeerClose  <-chan uint8    // fires with reason code on peer CLOSE
	LastRecvMs *atomic.Int64
}

// BeginHandshake walks the state machine from Init → Dialing → Handshaking.
// Called by the orchestrator before DoHandshake. Must be a no-op for a
// freshly constructed Session; returns an error otherwise.
func (s *Session) BeginHandshake() error {
	if !s.state.CAS(StateInit, StateDialing) {
		return fmt.Errorf("session: cannot dial from state %s", s.state.Load())
	}
	if !s.state.CAS(StateDialing, StateHandshaking) {
		return fmt.Errorf("session: cannot handshake from state %s", s.state.Load())
	}
	return nil
}

// Run wires up reader/writer/keepalive/watchdog goroutines for an active
// link. Returns a Runtime used by higher layers to publish messages and
// observe link-level events.
//
// Run does not block; goroutines run until Session.Close or link EOF. Every
// goroutine registers with s.wg so Session.Close joins them all.
func (s *Session) Run(cfg RunConfig) (*Runtime, error) {
	if !s.state.CAS(StateHandshaking, StateActive) {
		return nil, fmt.Errorf("session: cannot Run from state %s", s.state.Load())
	}

	if cfg.OutQSize <= 0 {
		cfg.OutQSize = 256
	}
	if cfg.MaxMsgSz <= 0 {
		cfg.MaxMsgSz = 1 << 30
	}
	if cfg.KeepAlive <= 0 {
		cfg.KeepAlive = 4
	}
	if cfg.Dispatch == nil {
		cfg.Dispatch = noopDispatch
	}

	outQ := make(chan OutboundItem, cfg.OutQSize)
	peerClose := make(chan uint8, 1)
	linkClosed := make(chan struct{})
	var lastRecv atomic.Int64
	reasm := transport.NewReassembler(cfg.MaxMsgSz)

	batcher := transport.NewBatcher(
		int(cfg.Result.NegotiatedBatchSize),
		cfg.Result.QoSEnabled,
		cfg.Result.MyInitialSN,
		cfg.Link.WriteBatch,
	)

	// triggerShutdown is idempotent; invoked by watchdog expiry, peer CLOSE,
	// or explicit session close. The reader exits on the resulting read
	// error, which closes linkClosed and cascades the teardown.
	var shutdownOnce sync.Once
	triggerShutdown := func() {
		shutdownOnce.Do(func() { _ = cfg.Link.Close() })
	}

	s.wg.Go(func() {
		defer close(linkClosed)
		readerLoop(ReaderConfig{
			Link:           cfg.Link,
			Reassembler:    reasm,
			Dispatch:       cfg.Dispatch,
			Logger:         s.logger,
			LastRecvUnixMs: &lastRecv,
			OnClose: func(reason uint8) {
				select {
				case peerClose <- reason:
				default:
				}
				triggerShutdown()
			},
		}, s.closing)
	})

	s.wg.Go(func() {
		writerLoop(outQ, batcher, cfg.Link, s.closing, s.logger)
	})

	// outQSenders tracks every goroutine that may write to outQ. The
	// orchestrator waits on it before closing outQ, so keepalive etc.
	// can never send to a closed channel.
	var outQSenders sync.WaitGroup

	if cfg.Result.PeerLeaseMillis > 0 {
		outQSenders.Add(1)
		s.wg.Go(func() {
			defer outQSenders.Done()
			keepaliveLoop(outQ, cfg.Result.PeerLeaseMillis, cfg.KeepAlive, s.closing, s.logger)
		})
	}

	if cfg.Result.MyLeaseMillis > 0 {
		s.wg.Go(func() {
			leaseWatchdog(&lastRecv, cfg.Result.MyLeaseMillis, s.closing, triggerShutdown, s.logger)
		})
	}

	// Shutdown orchestrator:
	//  1. wait for either Session.Close (s.closing) or link-level failure
	//     (linkClosed from the reader).
	//  2. ensure the link is closed so the reader returns.
	//  3. ensure s.closing is closed so outQ senders (keepalive) exit.
	//  4. wait for every outQ sender to stop.
	//  5. close outQ so the writer drains and exits.
	s.wg.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				s.logger.Error("shutdown orchestrator panicked",
					"panic", r, "stack", string(debug.Stack()))
			}
		}()

		select {
		case <-linkClosed:
		case <-s.closing:
		}
		triggerShutdown()
		<-linkClosed
		s.closeOnce.Do(func() { close(s.closing) })
		outQSenders.Wait()
		close(outQ)
		// Close out any in-flight Gets so their public-side translator
		// goroutines wake up and exit.
		s.cancelAllGets()
	})

	return &Runtime{
		OutQ:       outQ,
		Result:     cfg.Result,
		LinkClosed: linkClosed,
		PeerClose:  peerClose,
		LastRecvMs: &lastRecv,
	}, nil
}

// noopDispatch consumes and discards any network message, so the reader's
// "did you consume your body?" guard is satisfied even when no higher layer
// is wired up. Used as the default in RunConfig.
func noopDispatch(_ codec.Header, r *codec.Reader) error {
	// Consume the rest of the FRAME body. Simpler than decoding each
	// message type; acceptable for the "nothing is listening" case.
	_ = r.Skip(r.Len())
	return nil
}
