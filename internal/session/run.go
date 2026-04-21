package session

import (
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/transport"
)

// RunConfig bundles everything Session.Run needs to start the per-runtime
// goroutines once a link is up and a handshake has completed.
type RunConfig struct {
	Link      transport.Link
	Result    *HandshakeResult
	OutQSize  int             // buffered size of the outbound item queue
	MaxMsgSz  int             // cap for FRAGMENT reassembly
	KeepAlive int             // peer lease divisor (defaults to 4)
	Dispatch  InboundDispatch // where incoming network messages go; nil = drop
}

// Runtime is a per-link bundle: reader, writer, keepalive, watchdog, and
// the outbound queue that carries their traffic. It lives from Run() until
// Shutdown() drains everything; a single Session can create multiple
// Runtimes over its lifetime when the link is re-established after a drop.
type Runtime struct {
	OutQ       chan<- OutboundItem
	Result     *HandshakeResult
	LinkClosed <-chan struct{} // closed when the reader goroutine exits
	PeerClose  <-chan uint8    // fires with reason code on peer CLOSE
	LastRecvMs *atomic.Int64

	// Private runtime state: stop/done are closed by the runtime
	// orchestrator; link is held so Shutdown can poke the reader into exit
	// via link.Close().
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
	link     transport.Link
}

// Shutdown initiates orderly teardown of this runtime. Idempotent and safe
// to call concurrently. Returns immediately; wait on Done() for completion.
func (rt *Runtime) Shutdown() {
	rt.stopOnce.Do(func() {
		_ = rt.link.Close()
		close(rt.stop)
	})
}

// Done returns a channel that closes once every goroutine owned by this
// runtime has exited and the outbound queue has drained. A fresh Runtime
// for the same Session can be started only after Done has fired.
func (rt *Runtime) Done() <-chan struct{} { return rt.done }

// BeginHandshake moves the session into Handshaking, either from the
// initial Init state (fresh Open) or from Reconnecting (after a link drop
// followed by an EnterReconnecting call). It is a no-op for a state that
// is already Handshaking so that retries of the same handshake attempt are
// idempotent.
func (s *Session) BeginHandshake() error {
	for {
		old := s.state.Load()
		switch old {
		case StateHandshaking:
			return nil
		case StateInit, StateReconnecting:
			if !s.state.CAS(old, StateDialing) {
				continue // someone raced us; reload
			}
			if !s.state.CAS(StateDialing, StateHandshaking) {
				return fmt.Errorf("session: state raced during handshake transition (now %s)", s.state.Load())
			}
			return nil
		default:
			return fmt.Errorf("session: cannot handshake from state %s", old)
		}
	}
}

// EnterReconnecting transitions Active → Reconnecting. Called by the
// public zenoh-layer reconnect orchestrator once a Runtime's Done channel
// closes without user intent.
func (s *Session) EnterReconnecting() error {
	if s.state.CAS(StateActive, StateReconnecting) {
		return nil
	}
	return fmt.Errorf("session: cannot enter reconnecting from state %s", s.state.Load())
}

// Run wires up reader / writer / keepalive / watchdog goroutines for an
// active link. Returns a Runtime whose Done channel closes once all of
// those goroutines have exited. Run may be called multiple times on the
// same Session (for reconnect) — each invocation requires Handshaking
// state and produces an independent Runtime.
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
	lastRecv := new(atomic.Int64)
	reasm := transport.NewReassembler(cfg.MaxMsgSz)

	batcher := transport.NewBatcher(
		int(cfg.Result.NegotiatedBatchSize),
		cfg.Result.QoSEnabled,
		cfg.Result.MyInitialSN,
		cfg.Link.WriteBatch,
	)

	rt := &Runtime{
		OutQ:       outQ,
		Result:     cfg.Result,
		LinkClosed: linkClosed,
		PeerClose:  peerClose,
		LastRecvMs: lastRecv,
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
		link:       cfg.Link,
	}

	// wg tracks runtime-scoped goroutines (reader, writer, keepalive,
	// watchdog). The orchestrator joins them all before closing rt.done.
	var wg sync.WaitGroup

	wg.Go(func() {
		defer close(linkClosed)
		readerLoop(ReaderConfig{
			Link:           cfg.Link,
			Reassembler:    reasm,
			Dispatch:       cfg.Dispatch,
			Logger:         s.logger,
			LastRecvUnixMs: lastRecv,
			OnClose: func(reason uint8) {
				select {
				case peerClose <- reason:
				default:
				}
				rt.Shutdown()
			},
		}, rt.stop)
	})

	wg.Go(func() {
		writerLoop(outQ, batcher, cfg.Link, rt.stop, s.logger)
	})

	// outQSenders tracks goroutines that may write to outQ (keepalive),
	// so the orchestrator can close outQ only after they have stopped.
	var outQSenders sync.WaitGroup

	if cfg.Result.PeerLeaseMillis > 0 {
		outQSenders.Add(1)
		wg.Go(func() {
			defer outQSenders.Done()
			keepaliveLoop(outQ, cfg.Result.PeerLeaseMillis, cfg.KeepAlive, rt.stop, s.logger)
		})
	}

	if cfg.Result.MyLeaseMillis > 0 {
		wg.Go(func() {
			leaseWatchdog(lastRecv, cfg.Result.MyLeaseMillis, rt.stop, rt.Shutdown, s.logger)
		})
	}

	// Orchestrator:
	//   1. wait for the reader to exit (link EOF, peer CLOSE, or explicit Shutdown)
	//   2. fire Shutdown (idempotent) to propagate the stop signal to other loops
	//   3. wait for every outQ sender to stop, then close outQ so the writer drains
	//   4. wait for all runtime goroutines to exit
	//   5. close rt.done
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.logger.Error("runtime orchestrator panicked",
					"panic", r, "stack", string(debug.Stack()))
			}
			close(rt.done)
		}()
		<-linkClosed
		rt.Shutdown()
		outQSenders.Wait()
		close(outQ)
		wg.Wait()
		// Cancel any in-flight Gets so their public-side translator
		// goroutines exit. A subsequent Run() on this Session starts
		// fresh — Gets do not survive across Runtimes.
		s.cancelAllGets()
	}()

	return rt, nil
}

// noopDispatch consumes and discards any network message, so the reader's
// "did you consume your body?" guard is satisfied even when no higher layer
// is wired up. Used as the default in RunConfig.
func noopDispatch(_ codec.Header, r *codec.Reader) error {
	_ = r.Skip(r.Len())
	return nil
}
