package zenoh

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	intkeyexpr "github.com/shirou/zenoh-go-client/internal/keyexpr"
	"github.com/shirou/zenoh-go-client/internal/session"
	"github.com/shirou/zenoh-go-client/internal/transport"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// sessionModeToWire converts the public SessionMode to the wire-level
// WhatAmI used in INIT/OPEN/JOIN handshakes.
func sessionModeToWire(m SessionMode) wire.WhatAmI {
	switch m {
	case ModePeer:
		return wire.WhatAmIPeer
	case ModeRouter:
		return wire.WhatAmIRouter
	default:
		return wire.WhatAmIClient
	}
}

// Session is the client's connection to a zenoh router or peer. Session
// values are safe for concurrent use.
//
// A Session survives transient link failures: when the current link drops
// without Close having been called, an internal orchestrator re-dials the
// configured endpoints with exponential backoff and, on success, replays
// every live declaration (subscribers, queryables, liveliness tokens) on
// the fresh link. Outbound calls made during the gap return
// ErrSessionNotReady rather than blocking.
type Session struct {
	cfg     Config
	backoff session.ReconnectConfig
	inner   *session.Session
	zid     Id
	closed  atomic.Bool
	done    chan struct{} // closed when reconnect goroutine has exited

	// curRuntime is the reconnect orchestrator's view of the live client
	// runtime, swapped atomically so the hot path stays lock-free. The
	// inner session.runtimes registry is the authoritative multi-Runtime
	// store for peer mode; in client mode this pointer mirrors the
	// singleton entry of that map.
	curRuntime atomic.Pointer[session.Runtime]

	// tokens tracks LivelinessTokens for replay on reconnect. Subscribers
	// and queryables survive via the inner session's registries; tokens
	// have no inner-side dispatch registry so we keep them here.
	tokensMu sync.Mutex
	tokens   map[uint32]KeyExpr

	// queriers mirrors tokens: outbound INTEREST is not tracked inside
	// inner/session so the zenoh layer owns the replay registry.
	queriersMu sync.Mutex
	queriers   map[uint32]*querierState

	// publishers tracks Publisher INTEREST state for reconnect replay.
	// Matching state itself lives on inner.matchingRegistry; this map
	// only remembers enough to re-emit the INTEREST on a fresh link.
	publishersMu sync.Mutex
	publishers   map[uint32]*publisherState

	// livelinessSubsReplay keeps enough state per liveliness subscriber
	// to re-emit its INTEREST on a fresh link (keyExpr + history flag).
	// The inner session owns the actual deliver callback.
	livelinessSubsMu     sync.Mutex
	livelinessSubsReplay map[uint32]livelinessSubReplayState

	// userCtx is a context derived from UserCloseCh, lazily built and
	// cached. Used by dialContext to cancel in-flight dials on Close.
	userCtxOnce sync.Once
	userCtx     context.Context

	// peerWG tracks per-endpoint dial loops + accept loops + per-runtime
	// watcher goroutines spawned in peer mode. Close awaits them.
	peerWG sync.WaitGroup
	// listeners is the list of bound transport.Listener instances in peer
	// mode. Close iterates them.
	listenersMu sync.Mutex
	listeners   []transport.Listener
}

// Open dials the first reachable endpoint in cfg.Endpoints, completes the
// INIT/OPEN handshake, and starts the session's goroutines. The returned
// Session remains usable across subsequent link failures — the reconnect
// orchestrator restores declared entities automatically.
//
// In peer mode (cfg.Mode == ModePeer) Open additionally binds every
// listen endpoint and starts an outbound dial loop per connect endpoint,
// returning once the listeners are bound. Outbound dials retry in the
// background.
func Open(ctx context.Context, cfg Config) (*Session, error) {
	if cfg.Mode == ModeRouter {
		return nil, fmt.Errorf("zenoh.Open: router mode is not yet implemented")
	}
	if cfg.Mode == ModeClient && len(cfg.Endpoints) == 0 {
		return nil, fmt.Errorf("zenoh.Open: Config.Endpoints is empty")
	}
	if cfg.Mode == ModePeer && len(cfg.Endpoints) == 0 && len(cfg.ListenEndpoints) == 0 {
		return nil, fmt.Errorf("zenoh.Open: peer mode requires connect or listen endpoints")
	}

	zid, err := resolveZID(cfg.ZID)
	if err != nil {
		return nil, err
	}

	inner := session.New()
	s := &Session{
		cfg:                  cfg,
		backoff:              reconnectConfigFromUser(cfg),
		inner:                inner,
		zid:                  zid,
		done:                 make(chan struct{}),
		tokens:               make(map[uint32]KeyExpr),
		queriers:             make(map[uint32]*querierState),
		publishers:           make(map[uint32]*publisherState),
		livelinessSubsReplay: make(map[uint32]livelinessSubReplayState),
	}

	if cfg.Mode == ModePeer {
		if err := s.openPeer(ctx); err != nil {
			_ = inner.Close()
			return nil, err
		}
		go s.runPeerSupervisor()
		return s, nil
	}

	rt, err := s.dialAndRun(ctx)
	if err != nil {
		_ = inner.Close()
		return nil, err
	}
	s.installRuntime(rt)

	go s.runReconnectLoop()
	return s, nil
}

// installRuntime registers rt in the inner Session's runtime registry and
// updates curRuntime so the hot path picks it up.
func (s *Session) installRuntime(rt *session.Runtime) {
	if rt == nil {
		return
	}
	s.inner.RegisterRuntime(rt.PeerZIDBytes(), rt)
	s.curRuntime.Store(rt)
}

// uninstallRuntime is the inverse of installRuntime; called when a runtime
// finishes tearing down so the registry no longer references it.
func (s *Session) uninstallRuntime(rt *session.Runtime) {
	if rt == nil {
		return
	}
	s.inner.UnregisterRuntime(rt.PeerZIDBytes(), rt)
	s.curRuntime.CompareAndSwap(rt, nil)
}

// Close terminates the session, signalling the reconnect orchestrator to
// stop, sending a best-effort CLOSE to every peer, closing every listener,
// and waiting for all goroutines to finish.
func (s *Session) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	s.broadcastFarewellClose()
	// Signal the inner session to stop reconnecting and shut every
	// runtime down; per-mode supervisor then joins everything.
	_ = s.inner.Close()
	s.inner.ForEachRuntime(func(_ []byte, rt *session.Runtime) {
		rt.Shutdown()
	})
	s.closeAllListeners()
	<-s.done
	return nil
}

// closeAllListeners closes every transport.Listener registered by the
// peer-mode supervisor. Idempotent.
func (s *Session) closeAllListeners() {
	s.listenersMu.Lock()
	lns := s.listeners
	s.listeners = nil
	s.listenersMu.Unlock()
	for _, ln := range lns {
		_ = ln.Close()
	}
}

// IsClosed reports whether Close has been called.
func (s *Session) IsClosed() bool { return s.closed.Load() }

// ZId returns the session's local ZenohID.
func (s *Session) ZId() Id { return s.zid }

// PeersZId returns the ZenohIDs of every currently-connected peer
// session — i.e. remote peers whose handshake advertised Peer. Empty in
// client mode and whenever no live peer Runtime is registered.
func (s *Session) PeersZId() []Id { return s.peerIDsByRole(wire.WhatAmIPeer) }

// RoutersZId returns the ZenohIDs of every currently-connected router.
// In client mode this is at most one router; in peer mode it lists all
// routers a peer has dialled.
func (s *Session) RoutersZId() []Id { return s.peerIDsByRole(wire.WhatAmIRouter) }

func (s *Session) peerIDsByRole(role wire.WhatAmI) []Id {
	var out []Id
	s.inner.ForEachRuntime(func(_ []byte, rt *session.Runtime) {
		if rt.Result == nil {
			return
		}
		if rt.Result.PeerWhatAmI == role {
			out = append(out, IdFromWireID(rt.Result.PeerZID))
		}
	})
	return out
}

// dialAndRun dials, handshakes, and starts a fresh Runtime. Used both for
// Open and for each reconnect attempt.
func (s *Session) dialAndRun(ctx context.Context) (*session.Runtime, error) {
	link, err := session.DialFirst(ctx, s.cfg.Endpoints)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	if err := s.inner.BeginHandshake(); err != nil {
		link.Close()
		return nil, err
	}
	hcfg := session.DefaultHandshakeConfig()
	hcfg.ZID = s.zid.ToWireID()
	hcfg.WhatAmI = sessionModeToWire(s.cfg.Mode)
	result, err := session.DoHandshake(link, hcfg)
	if err != nil {
		link.Close()
		return nil, fmt.Errorf("handshake: %w", err)
	}
	rt, err := s.inner.Run(session.RunConfig{
		Link:     link,
		Result:   result,
		Dispatch: s.inner.NetworkDispatcher(),
	})
	if err != nil {
		link.Close()
		return nil, fmt.Errorf("run: %w", err)
	}
	return rt, nil
}

// runReconnectLoop watches the current Runtime for completion. On
// user-initiated close it exits; otherwise it enters Reconnecting, dials
// with backoff, and, on success, swaps in a fresh Runtime and replays
// every live entity.
func (s *Session) runReconnectLoop() {
	defer close(s.done)
	logger := s.inner.Logger()

	for {
		rt := s.curRuntime.Load()
		if rt == nil {
			return
		}
		// Wait for the current runtime to finish tearing down.
		<-rt.Done()
		s.uninstallRuntime(rt)

		// User Close? Stop here.
		select {
		case <-s.inner.UserCloseCh():
			return
		default:
		}

		if err := s.inner.EnterReconnecting(); err != nil {
			// State is Closed already — exit.
			return
		}
		logger.Info("link lost, reconnecting", "endpoints", s.cfg.Endpoints)

		newRt, ok := s.reconnectWithBackoff()
		if !ok {
			return
		}

		s.installRuntime(newRt)

		if err := s.replayEntities(); err != nil {
			logger.Warn("reconnect: entity replay failed", "err", err)
		}
		logger.Info("reconnected")
	}
}

// reconnectWithBackoff retries dialAndRun until success, user close, or
// the state is no longer Reconnecting. Returns (nil, false) on give-up.
func (s *Session) reconnectWithBackoff() (*session.Runtime, bool) {
	b := session.NewBackoff(s.backoff)
	logger := s.inner.Logger()
	for {
		select {
		case <-s.inner.UserCloseCh():
			return nil, false
		default:
		}

		// Each dial attempt is bounded by the backoff MaxMs window AND
		// cancelled immediately if the user calls Close — otherwise a
		// blocking dial could delay shutdown up to MaxMs.
		attemptCtx, cancel := s.dialContext(time.Duration(s.backoff.MaxMs) * time.Millisecond)
		rt, err := s.dialAndRun(attemptCtx)
		cancel()
		if err == nil {
			return rt, true
		}
		logger.Debug("reconnect attempt failed", "err", err)

		wait := b.Next()
		select {
		case <-time.After(wait):
		case <-s.inner.UserCloseCh():
			return nil, false
		}
	}
}

// dialContext returns a context bounded by timeout and cancelled on user
// Close. The returned cancel must be called on success to release the
// AfterFunc watcher.
func (s *Session) dialContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	// context.AfterFunc fires cancel when userCloseCtx is Done OR
	// produces a stop func to cancel the wait before it fires. Cheaper
	// and simpler than a dedicated watcher goroutine.
	stop := context.AfterFunc(s.userCloseCtx(), cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// userCloseCtx returns a context cancelled when Session.Close is called.
// Lazily constructed once and cached for the session's lifetime.
func (s *Session) userCloseCtx() context.Context {
	s.userCtxOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		s.userCtx = ctx
		go func() {
			<-s.inner.UserCloseCh()
			cancel()
		}()
	})
	return s.userCtx
}

// replayEntities re-emits D_SUBSCRIBER / D_QUERYABLE / D_TOKEN /
// INTEREST for every entity that was live when the link dropped. Called
// once per successful reconnect, before the session is handed back to
// the caller.
//
// In client mode there is exactly one Runtime, so this is equivalent to
// replayToRuntime against that singleton. The thin wrapper exists so
// peer-mode lifecycle code can target a specific newly-installed Link.
func (s *Session) replayEntities() error {
	s.inner.ResetRemoteAliases()
	s.inner.ResetInboundTokens()
	// Matching counts/flags are tied to the old router's INTEREST snapshot;
	// clear them so the fresh link's snapshot-complete re-delivers the
	// correct state. Listeners survive the reset.
	s.inner.ResetMatching()

	rt := s.snapshotRuntime()
	if rt == nil {
		return ErrSessionNotReady
	}
	return s.replayToRuntime(rt)
}

// replayToRuntime emits the catch-up declarations to the given Runtime.
// Each registry is snapshotted into a slice of replayEntry closures with
// its own lock released before any send fires — holding a registry lock
// across a blocking enqueue would serialise declare / undeclare calls
// for the full duration of replay.
func (s *Session) replayToRuntime(rt *session.Runtime) error {
	if rt == nil {
		return ErrSessionNotReady
	}
	var plan []replayEntry

	s.inner.ForEachSubscriber(func(id uint32, ke intkeyexpr.KeyExpr) {
		k := KeyExpr{inner: ke}
		plan = append(plan, replayEntry{id, "D_SUBSCRIBER", func() error {
			return s.sendDeclareSubscriberOn(rt, id, k)
		}})
	})
	s.inner.ForEachQueryable(func(id uint32, ke intkeyexpr.KeyExpr, complete bool) {
		k := KeyExpr{inner: ke}
		opts := &QueryableOptions{Complete: complete}
		plan = append(plan, replayEntry{id, "D_QUERYABLE", func() error {
			return s.sendDeclareQueryableOn(rt, id, k, opts)
		}})
	})
	s.tokensMu.Lock()
	for id, ke := range s.tokens {
		plan = append(plan, replayEntry{id, "D_TOKEN", func() error {
			return s.sendDeclareTokenOn(rt, id, ke)
		}})
	}
	s.tokensMu.Unlock()
	s.queriersMu.Lock()
	for id, q := range s.queriers {
		ke := q.keyExpr
		plan = append(plan, replayEntry{id, "INTEREST", func() error {
			return s.sendQuerierInterestOn(rt, id, ke)
		}})
	}
	s.queriersMu.Unlock()
	s.publishersMu.Lock()
	for id, p := range s.publishers {
		ke := p.keyExpr
		plan = append(plan, replayEntry{id, "INTEREST[publisher]", func() error {
			return s.sendPublisherInterestOn(rt, id, ke)
		}})
	}
	s.publishersMu.Unlock()
	s.livelinessSubsMu.Lock()
	for id, st := range s.livelinessSubsReplay {
		ke := st.keyExpr
		history := st.history
		plan = append(plan, replayEntry{id, "INTEREST[liveliness]", func() error {
			return s.sendLivelinessSubscriberInterestOn(rt, id, ke, history)
		}})
	}
	s.livelinessSubsMu.Unlock()

	var errs []error
	for _, e := range plan {
		if err := e.send(); err != nil {
			errs = append(errs, fmt.Errorf("%s id=%d: %w", e.label, e.id, err))
		}
	}
	return errors.Join(errs...)
}

// replayEntry is one item in the reconnect replay plan. The closure owns
// whatever state (keyExpr, options) the specific send needs; the loop in
// replayEntities stays uniform.
type replayEntry struct {
	id    uint32
	label string
	send  func() error
}

// broadcastFarewellClose enqueues a best-effort CLOSE frame to every
// live Runtime. The orchestrator never closes OutQ (shutdown is
// signalled solely through LinkClosed), so the sends below cannot panic
// on send-to-closed; the recover in farewellTo stays as a narrow safety
// net in case a future regression reintroduces the close.
func (s *Session) broadcastFarewellClose() {
	for _, rt := range s.inner.SnapshotRuntimes() {
		s.farewellTo(rt)
	}
}

func (s *Session) farewellTo(rt *session.Runtime) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(runtime.Error); !ok {
				panic(r)
			}
		}
	}()
	if rt == nil {
		return
	}
	select {
	case <-rt.LinkClosed:
		return
	default:
	}
	closeBytes, err := session.EncodeCloseMessage(session.CloseReasonGeneric)
	if err != nil {
		return
	}
	select {
	case rt.OutQ <- session.OutboundItem{RawBatch: closeBytes}:
	case <-rt.LinkClosed:
	default:
	}
}

// snapshotRuntime returns the currently-live Runtime or nil if there is
// no active link (session closed, or the reconnect loop between Runtimes).
// Reads are atomic; a Runtime returned here remains safe to use because
// OutQ is never closed and the enqueueNetwork select observes LinkClosed
// to bail out cleanly when the link goes away.
//
// In peer mode there may be multiple live Runtimes — this returns the
// reconnect orchestrator's "current" pointer, which is non-deterministic.
// Callers that need to fan out to every peer should use
// snapshotAllRuntimes instead.
func (s *Session) snapshotRuntime() *session.Runtime {
	return s.curRuntime.Load()
}

// snapshotAllRuntimes returns every currently-live Runtime. Empty in
// client mode while the link is down; in peer mode includes every
// established remote peer.
func (s *Session) snapshotAllRuntimes() []*session.Runtime {
	return s.inner.SnapshotRuntimes()
}

// resolveZID parses hex or falls back to a fresh random ID.
func resolveZID(hexStr string) (Id, error) {
	if hexStr == "" {
		return IdFromWireID(session.GenerateZID()), nil
	}
	return NewIdFromHex(hexStr)
}

// internalKeyExpr returns the internal/keyexpr value for wire encoding / matching.
func (k KeyExpr) internalKeyExpr() intkeyexpr.KeyExpr { return k.inner }

// reconnectConfigFromUser overlays any non-zero user-supplied Config
// timing onto the Rust-compatible defaults.
func reconnectConfigFromUser(cfg Config) session.ReconnectConfig {
	c := session.DefaultReconnectConfig()
	if cfg.ReconnectInitial > 0 {
		c.InitialMs = uint64(cfg.ReconnectInitial / time.Millisecond)
	}
	if cfg.ReconnectMax > 0 {
		c.MaxMs = uint64(cfg.ReconnectMax / time.Millisecond)
	}
	if cfg.ReconnectFactor > 0 {
		c.IncreaseFactor = cfg.ReconnectFactor
	}
	return c
}
