package session

import (
	"log/slog"
	"sync"
)

// Session is the internal state container for a zenoh client session.
// It owns all per-session goroutines (reader, writer, keepalive watchdog,
// inbound dispatcher, reconnect manager) and the entity registry.
type Session struct {
	state stateAtom
	ids   idAllocators

	// Coordination primitives shared by all per-session goroutines.
	closing chan struct{}  // closed by Close(); signals everyone to exit
	wg      sync.WaitGroup // waits for every goroutine launched by RunLoops

	closeOnce sync.Once

	// logger is the session-scoped logger. Tests override via OptionLogger.
	logger *slog.Logger
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

// New constructs a Session in the Init state. It does not start any goroutines
// or perform any I/O; call RunLoops to launch the session's goroutines.
func New(opts ...Option) *Session {
	s := &Session{
		closing: make(chan struct{}),
		logger:  slog.Default(),
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

// IDs returns the session's entity ID allocators. Used by Publisher/Subscriber
// declaration paths.
func (s *Session) IDs() *idAllocators { return &s.ids }

// Close requests orderly shutdown of the session.
//
// It is safe (and idempotent) to call Close concurrently with any other
// method. Close returns after every per-session goroutine has exited.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		for {
			old := s.state.Load()
			if old == StateClosed || s.state.CAS(old, StateClosed) {
				break
			}
		}
		close(s.closing)
	})
	// wg.Wait is safe to call from multiple goroutines; it blocks until the
	// goroutines launched by RunLoops have all returned.
	s.wg.Wait()
	return nil
}

// ClosingCh returns the closing channel internal goroutines select on to
// observe session shutdown. Package-internal use only.
func (s *Session) ClosingCh() <-chan struct{} { return s.closing }
