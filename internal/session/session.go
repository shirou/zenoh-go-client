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
	}
	s.state.Store(StateInit)
	for _, opt := range opts {
		opt(s)
	}
	return s
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
