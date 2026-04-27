package session

import (
	"log/slog"
	"sync"
)

// Session is the internal state container for a zenoh client session. It
// owns session-scoped state (state machine, ID allocators, entity
// registries) and persists across reconnects. Per-link state — reader /
// writer / keepalive / watchdog goroutines, the outbound queue, sequence
// numbers — lives on Runtime (see run.go). A single Session can host
// multiple Runtimes over its lifetime via Run() + reconnect.
type Session struct {
	state stateAtom
	ids   idAllocators

	// userClose is closed by Session.Close and signals the reconnect
	// orchestrator (in the public zenoh layer) that no further Runtime
	// should be started. Per-runtime goroutines select on Runtime.stop,
	// not on this channel.
	userClose chan struct{}
	closeOnce sync.Once

	// logger is the session-scoped logger. Tests override via OptionLogger.
	logger *slog.Logger

	// Lazily-initialised registries — survive reconnects so entity
	// re-declaration can iterate them.
	subsOnce sync.Once
	subs     *subscribers
	qblsOnce sync.Once
	qbls     *queryables
	getsOnce sync.Once
	gets     *gets

	livelinessSubsOnce    sync.Once
	livelinessSubs        *livelinessSubs
	livelinessQueriesOnce sync.Once
	livelinessQueries     *livelinessQueries

	remoteAliasesOnce sync.Once
	remoteAliases     *remoteAliases

	inboundTokensOnce sync.Once
	inboundTokens     *inboundTokens

	matchingOnce sync.Once
	matching     *matchingRegistry

	// runtimes tracks every live Runtime, keyed by the peer's ZenohID.
	// In client mode this holds at most one entry; peer mode holds one
	// entry per established remote peer. The hot path (Put/Declare) goes
	// through ForEachRuntime which read-locks once.
	runtimesMu sync.RWMutex
	runtimes   map[string]*Runtime
}

// Option configures a Session at construction time.
type Option func(*Session)

// WithLogger overrides the default logger.
func WithLogger(logger *slog.Logger) Option {
	return func(s *Session) {
		if logger != nil {
			s.logger = logger
		}
	}
}

// New constructs a Session in the Init state. It does not start any
// goroutines or perform any I/O; call BeginHandshake then Run to attach a
// live link.
func New(opts ...Option) *Session {
	s := &Session{
		userClose: make(chan struct{}),
		logger:    slog.Default(),
		runtimes:  make(map[string]*Runtime),
	}
	s.state.Store(StateInit)
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// RegisterRuntime stores rt under the given peer ZenohID. It overwrites
// any existing entry — callers that need to detect duplicates must use
// RuntimeFor first.
func (s *Session) RegisterRuntime(peerZID []byte, rt *Runtime) {
	if rt == nil {
		return
	}
	s.runtimesMu.Lock()
	defer s.runtimesMu.Unlock()
	s.runtimes[string(peerZID)] = rt
}

// UnregisterRuntime removes the entry for peerZID if rt matches. The
// matched-rt check protects against unregistering a freshly-installed
// successor when an old Runtime's teardown observer fires after the
// replacement has already been registered.
func (s *Session) UnregisterRuntime(peerZID []byte, rt *Runtime) {
	s.runtimesMu.Lock()
	defer s.runtimesMu.Unlock()
	if cur, ok := s.runtimes[string(peerZID)]; ok && (rt == nil || cur == rt) {
		delete(s.runtimes, string(peerZID))
	}
}

// RuntimeFor returns the Runtime registered for the given peer ZenohID,
// or nil if none is live.
func (s *Session) RuntimeFor(peerZID []byte) *Runtime {
	s.runtimesMu.RLock()
	defer s.runtimesMu.RUnlock()
	return s.runtimes[string(peerZID)]
}

// ForEachRuntime invokes fn for every live Runtime. The read lock is held
// for the duration so fn must not block on any operation that registers or
// unregisters runtimes (e.g. Run/Shutdown). Snapshot first if the caller
// needs to do blocking I/O.
func (s *Session) ForEachRuntime(fn func(peerZID []byte, rt *Runtime)) {
	s.runtimesMu.RLock()
	defer s.runtimesMu.RUnlock()
	for k, rt := range s.runtimes {
		fn([]byte(k), rt)
	}
}

// SnapshotRuntimes returns a copy of the current runtime set. Use when
// the caller needs to perform blocking work per-runtime without holding
// the registry lock.
func (s *Session) SnapshotRuntimes() []*Runtime {
	s.runtimesMu.RLock()
	defer s.runtimesMu.RUnlock()
	if len(s.runtimes) == 0 {
		return nil
	}
	out := make([]*Runtime, 0, len(s.runtimes))
	for _, rt := range s.runtimes {
		out = append(out, rt)
	}
	return out
}

// AnyRuntime returns one currently-live Runtime, or nil if none. For
// client-mode hot paths that only need "the" runtime; in peer mode the
// choice between multiple runtimes is non-deterministic — callers that
// care use ForEachRuntime instead.
func (s *Session) AnyRuntime() *Runtime {
	s.runtimesMu.RLock()
	defer s.runtimesMu.RUnlock()
	for _, rt := range s.runtimes {
		return rt
	}
	return nil
}

// State returns the current lifecycle state.
func (s *Session) State() State { return s.state.Load() }

// IsClosed reports whether the session has completed shutdown.
func (s *Session) IsClosed() bool {
	return s.state.Load() == StateClosed
}

// Logger returns the session logger.
func (s *Session) Logger() *slog.Logger { return s.logger }

// IDs returns the session's entity ID allocators. Used by Publisher /
// Subscriber / Queryable / LivelinessToken declaration paths.
func (s *Session) IDs() *idAllocators { return &s.ids }

// Close signals end-of-life to any reconnect orchestrator watching this
// session. It does NOT drain per-runtime goroutines — that is the current
// Runtime's responsibility; the public zenoh layer waits for both.
// Idempotent.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		for {
			old := s.state.Load()
			if old == StateClosed || s.state.CAS(old, StateClosed) {
				break
			}
		}
		close(s.userClose)
	})
	return nil
}

// UserCloseCh returns the channel that is closed when Session.Close has
// been called. The reconnect orchestrator selects on it to distinguish
// "link failure, try again" from "user requested shutdown, give up".
func (s *Session) UserCloseCh() <-chan struct{} { return s.userClose }
