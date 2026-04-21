package session

import "time"

// ReconnectConfig mirrors the Rust DEFAULT_CONFIG.json5 connect/retry/* keys.
type ReconnectConfig struct {
	InitialMs      uint64  // connect/retry/period_init_ms; default 1000
	MaxMs          uint64  // connect/retry/period_max_ms;  default 4000
	IncreaseFactor float64 // connect/retry/period_increase_factor; default 2.0
	ExitOnFailure  bool    // connect/exit_on_failure; default false
}

// DefaultReconnectConfig returns the Rust-compatible defaults.
func DefaultReconnectConfig() ReconnectConfig {
	return ReconnectConfig{
		InitialMs:      1000,
		MaxMs:          4000,
		IncreaseFactor: 2.0,
	}
}

// Backoff drives exponential-growth retry intervals capped at MaxMs.
type Backoff struct {
	cfg     ReconnectConfig
	current time.Duration
	maxDur  time.Duration
}

// NewBackoff constructs a Backoff seeded at cfg.InitialMs.
func NewBackoff(cfg ReconnectConfig) *Backoff {
	return &Backoff{
		cfg:     cfg,
		current: time.Duration(cfg.InitialMs) * time.Millisecond,
		maxDur:  time.Duration(cfg.MaxMs) * time.Millisecond,
	}
}

// Next returns the next wait duration and advances the backoff.
func (b *Backoff) Next() time.Duration {
	d := b.current
	next := time.Duration(float64(d) * b.cfg.IncreaseFactor)
	if next <= 0 || next > b.maxDur {
		next = b.maxDur
	}
	b.current = next
	return d
}

// Reset rolls the backoff back to the initial value after a successful
// reconnect so the next disconnection starts fresh.
func (b *Backoff) Reset() {
	b.current = time.Duration(b.cfg.InitialMs) * time.Millisecond
}
