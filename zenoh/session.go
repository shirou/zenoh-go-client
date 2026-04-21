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
	"github.com/shirou/zenoh-go-client/internal/wire"
)

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

	// runtime is swapped atomically by the reconnect orchestrator so the
	// hot path (every Put / Get / Declare through snapshotRuntime) stays
	// lock-free.
	runtime atomic.Pointer[session.Runtime]

	// tokens tracks LivelinessTokens for replay on reconnect. Subscribers
	// and queryables survive via the inner session's registries; tokens
	// have no inner-side dispatch registry so we keep them here.
	tokensMu sync.Mutex
	tokens   map[uint32]KeyExpr

	// userCtx is a context derived from UserCloseCh, lazily built and
	// cached. Used by dialContext to cancel in-flight dials on Close.
	userCtxOnce sync.Once
	userCtx     context.Context
}

// Open dials the first reachable endpoint in cfg.Endpoints, completes the
// INIT/OPEN handshake, and starts the session's goroutines. The returned
// Session remains usable across subsequent link failures — the reconnect
// orchestrator restores declared entities automatically.
func Open(ctx context.Context, cfg Config) (*Session, error) {
	if len(cfg.Endpoints) == 0 {
		return nil, fmt.Errorf("zenoh.Open: Config.Endpoints is empty")
	}

	zid, err := resolveZID(cfg.ZID)
	if err != nil {
		return nil, err
	}

	inner := session.New()
	s := &Session{
		cfg:     cfg,
		backoff: reconnectConfigFromUser(cfg),
		inner:   inner,
		zid:     zid,
		done:    make(chan struct{}),
		tokens:  make(map[uint32]KeyExpr),
	}

	rt, err := s.dialAndRun(ctx)
	if err != nil {
		_ = inner.Close()
		return nil, err
	}
	s.runtime.Store(rt)

	go s.runReconnectLoop()
	return s, nil
}

// Close terminates the session, signalling the reconnect orchestrator to
// stop, sending a best-effort CLOSE to the peer, and waiting for all
// goroutines to finish.
func (s *Session) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	s.sendFarewellClose()
	// Signal the inner session to stop reconnecting and shut the current
	// runtime down; the orchestrator then joins everything.
	_ = s.inner.Close()
	if rt := s.runtime.Load(); rt != nil {
		rt.Shutdown()
	}
	<-s.done
	return nil
}

// IsClosed reports whether Close has been called.
func (s *Session) IsClosed() bool { return s.closed.Load() }

// ZId returns the session's local ZenohID.
func (s *Session) ZId() Id { return s.zid }

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
	hcfg.WhatAmI = wire.WhatAmIClient
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
		rt := s.runtime.Load()
		if rt == nil {
			return
		}
		// Wait for the current runtime to finish tearing down.
		<-rt.Done()

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

		s.runtime.Store(newRt)

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

// replayEntities re-emits D_SUBSCRIBER / D_QUERYABLE / D_TOKEN for every
// entity that was live when the link dropped. Called once per successful
// reconnect, before the session is handed back to the caller.
//
// Snapshots the three registries into local slices first, then releases
// the registry locks before issuing any send. Holding a registry lock
// across a blocking enqueueNetwork would serialise DeclareSubscriber /
// UnregisterSubscriber calls for the full duration of replay.
func (s *Session) replayEntities() error {
	type subEntry struct {
		id uint32
		ke intkeyexpr.KeyExpr
	}
	type qblEntry struct {
		id       uint32
		ke       intkeyexpr.KeyExpr
		complete bool
	}
	var subs []subEntry
	var qbls []qblEntry
	s.inner.ForEachSubscriber(func(id uint32, ke intkeyexpr.KeyExpr) {
		subs = append(subs, subEntry{id, ke})
	})
	s.inner.ForEachQueryable(func(id uint32, ke intkeyexpr.KeyExpr, complete bool) {
		qbls = append(qbls, qblEntry{id, ke, complete})
	})
	s.tokensMu.Lock()
	tokens := make(map[uint32]KeyExpr, len(s.tokens))
	for id, ke := range s.tokens {
		tokens[id] = ke
	}
	s.tokensMu.Unlock()

	var errs []error
	for _, e := range subs {
		if err := s.sendDeclareSubscriber(e.id, KeyExpr{inner: e.ke}); err != nil {
			errs = append(errs, fmt.Errorf("D_SUBSCRIBER id=%d: %w", e.id, err))
		}
	}
	for _, e := range qbls {
		opts := &QueryableOptions{Complete: e.complete}
		if err := s.sendDeclareQueryable(e.id, KeyExpr{inner: e.ke}, opts); err != nil {
			errs = append(errs, fmt.Errorf("D_QUERYABLE id=%d: %w", e.id, err))
		}
	}
	for id, ke := range tokens {
		if err := s.sendDeclareToken(id, ke); err != nil {
			errs = append(errs, fmt.Errorf("D_TOKEN id=%d: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// sendFarewellClose enqueues a best-effort CLOSE frame. The outbound
// queue may race with the runtime orchestrator (which closes it once the
// link goroutines exit). Runtime.LinkClosed closes strictly before OutQ
// is closed, so observing LinkClosed as open means the send is safe; the
// recover is a narrow safety net for the "send on closed channel" race
// that only the Go runtime can surface here.
func (s *Session) sendFarewellClose() {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(runtime.Error); !ok {
				panic(r)
			}
		}
	}()
	rt := s.snapshotRuntime()
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
// the orchestrator closes LinkClosed before draining OutQ, and the
// enqueueNetwork select observes LinkClosed first.
func (s *Session) snapshotRuntime() *session.Runtime {
	return s.runtime.Load()
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
