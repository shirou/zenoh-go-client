package session

import (
	"log/slog"
	"runtime/debug"
	"sync/atomic"
	"time"
)

// leaseWatchdog monitors the timestamp of the last inbound batch and fires
// onExpire if the peer is silent for longer than myLeaseMs.
//
// The reader loop updates lastRecvUnixMs on every batch received. When the
// watchdog observes (now - lastRecv) > lease, it calls onExpire exactly
// once and exits; the reconnect manager is expected to tear down the
// current link and restart.
func leaseWatchdog(
	lastRecvUnixMs *atomic.Int64,
	myLeaseMs uint64,
	closing <-chan struct{},
	onExpire func(),
	logger *slog.Logger,
) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("watchdog panicked", "panic", r, "stack", string(debug.Stack()))
		}
	}()

	if myLeaseMs == 0 {
		logger.Warn("watchdog: myLeaseMs=0, disabling")
		return
	}

	// Wake at a fraction of the lease so we detect expiry within one
	// check-interval. lease/8 keeps detection latency below an eighth of
	// the lease window.
	checkInterval := max(time.Duration(myLeaseMs)*time.Millisecond/8, 50*time.Millisecond)
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	leaseDur := time.Duration(myLeaseMs) * time.Millisecond

	// Seed lastRecv to "now" so we don't immediately fire at startup.
	lastRecvUnixMs.Store(time.Now().UnixMilli())

	for {
		select {
		case <-closing:
			return
		case <-ticker.C:
			last := lastRecvUnixMs.Load()
			if last == 0 {
				continue
			}
			if time.Since(time.UnixMilli(last)) > leaseDur {
				logger.Warn("watchdog: peer silent beyond lease", "lease_ms", myLeaseMs)
				onExpire()
				return
			}
		}
	}
}
