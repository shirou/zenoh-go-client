package session

import (
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/locator"
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

// LinkLocalAddress returns the local endpoint string of the underlying link,
// for surfacing through the public LinkInfo API. Empty when the transport
// has no meaningful local address.
func (rt *Runtime) LinkLocalAddress() string { return rt.link.LocalAddress() }

// LinkRemoteLocator returns the locator the link is connected to.
func (rt *Runtime) LinkRemoteLocator() locator.Locator { return rt.link.RemoteLocator() }

// PeerZIDBytes returns the remote peer's ZenohID bytes, for use as a key
// in the Session.runtimes registry.
func (rt *Runtime) PeerZIDBytes() []byte {
	if rt.Result == nil {
		return nil
	}
	return rt.Result.PeerZID.Bytes
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
//
// In multi-runtime (peer) mode the single-runtime FSM does not apply —
// concurrent handshakes against unrelated peers are valid and must not
// fail just because an earlier one already drove the state to Active.
// The only refusal is StateClosed.
func (s *Session) BeginHandshake() error {
	if s.multiRuntime {
		if s.state.Load() == StateClosed {
			return fmt.Errorf("session: cannot handshake from state Closed")
		}
		return nil
	}
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
//
// In multi-runtime (peer) mode the Handshaking→Active CAS is replaced
// with a permissive "not Closed" check so two concurrent handshakes
// against different peers can both produce Runtimes — the canonical-link
// tiebreak in the calling layer resolves duplicates without forcing one
// of them to drop its Link mid-flight (which would mutually destroy
// both directions).
func (s *Session) Run(cfg RunConfig) (*Runtime, error) {
	if s.multiRuntime {
		// Bump to Active without clobbering a concurrent Close that
		// raced past our Closed check. CAS-loop because the source state
		// can be Init (first ever Run on this Session), Handshaking
		// (only one peer ever called BeginHandshake), or Active (the
		// common steady-state once any peer is up).
		for {
			cur := s.state.Load()
			if cur == StateClosed {
				return nil, fmt.Errorf("session: cannot Run from state Closed")
			}
			if cur == StateActive || s.state.CAS(cur, StateActive) {
				break
			}
		}
	} else if !s.state.CAS(StateHandshaking, StateActive) {
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

	if cfg.Result.PeerLeaseMillis > 0 {
		wg.Go(func() {
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
	//   3. wait for all runtime goroutines to exit
	//   4. close rt.done
	//
	// outQ is intentionally never closed. The writer exits on rt.stop, and
	// external senders (Put / sendFarewellClose) bail on LinkClosed in their
	// own selects. Closing outQ would race against in-flight sends — the Go
	// race detector flags a close concurrent with a chan-send even when the
	// send arms a LinkClosed branch, so leaving outQ open keeps the contract
	// race-free without losing any wakeup signal.
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
		wg.Wait()
		// Cancel any in-flight Gets so their public-side translator
		// goroutines exit. A subsequent Run() on this Session starts
		// fresh — Gets do not survive across Runtimes.
		s.cancelAllGets()
		s.cancelAllLivelinessQueries()
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
