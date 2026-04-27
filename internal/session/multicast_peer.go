package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/transport"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// MulticastPeerEntry records what we know about one peer discovered on
// the multicast group. Populated/refreshed by JOIN reception, evicted by
// lease expiry. Pub/sub propagation through multicast FRAME bodies is
// not yet wired — the immediate value of this table is letting the
// public API surface "who is on the group" via Session.MulticastPeers.
type MulticastPeerEntry struct {
	ZID        wire.ZenohID
	WhatAmI    wire.WhatAmI
	LeaseMs    uint64
	BatchSize  uint16
	Resolution wire.Resolution
	LaneSNs    *wire.LaneSNs // nil when JOIN omits the QoS lane SN ext
	NextSNRel  uint64
	NextSNBE   uint64
	SrcAddr    *net.UDPAddr
	// LastSeenUnixMs is the JOIN reception wallclock; eviction uses it
	// against LeaseMs.
	LastSeenUnixMs atomic.Int64
}

// MulticastPeerTable is the multicast peer registry. Keyed primarily by
// ZID; bySrcAddr is the fast path used by FRAME readers (when those
// land) to map a datagram source back to its ZID without walking byZID.
type MulticastPeerTable struct {
	mu        sync.RWMutex
	byZID     map[string]*MulticastPeerEntry
	bySrcAddr map[string]*MulticastPeerEntry
}

// NewMulticastPeerTable returns an empty MulticastPeerTable.
func NewMulticastPeerTable() *MulticastPeerTable {
	return &MulticastPeerTable{
		byZID:     make(map[string]*MulticastPeerEntry),
		bySrcAddr: make(map[string]*MulticastPeerEntry),
	}
}

// Insert refreshes an existing entry or creates a new one. Returns the
// entry plus a flag that is true when the JOIN introduced a peer not
// previously in the table.
func (t *MulticastPeerTable) Insert(j *wire.Join, src *net.UDPAddr) (*MulticastPeerEntry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := string(j.ZID.Bytes)
	now := time.Now().UnixMilli()
	if entry, ok := t.byZID[key]; ok {
		entry.LastSeenUnixMs.Store(now)
		entry.SrcAddr = src
		t.bySrcAddr[src.String()] = entry
		return entry, false
	}
	leaseMs := j.Lease
	if j.LeaseSeconds {
		leaseMs *= 1000
	}
	entry := &MulticastPeerEntry{
		ZID:        j.ZID,
		WhatAmI:    j.WhatAmI,
		LeaseMs:    leaseMs,
		BatchSize:  j.BatchSize,
		Resolution: j.Resolution,
		NextSNRel:  j.NextSNRel,
		NextSNBE:   j.NextSNBE,
		SrcAddr:    src,
	}
	if sns, present, err := j.DecodeLaneSNs(); err == nil && present {
		entry.LaneSNs = sns
	}
	entry.LastSeenUnixMs.Store(now)
	t.byZID[key] = entry
	t.bySrcAddr[src.String()] = entry
	return entry, true
}

// LookupByZID returns the entry for the given ZenohID or nil.
func (t *MulticastPeerTable) LookupByZID(zid wire.ZenohID) *MulticastPeerEntry {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.byZID[string(zid.Bytes)]
}

// LookupBySrcAddr returns the entry whose last-seen src matches addr,
// or nil. The reverse map is updated on every Insert.
func (t *MulticastPeerTable) LookupBySrcAddr(addr *net.UDPAddr) *MulticastPeerEntry {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.bySrcAddr[addr.String()]
}

// Snapshot returns a copy of every live entry.
func (t *MulticastPeerTable) Snapshot() []*MulticastPeerEntry {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]*MulticastPeerEntry, 0, len(t.byZID))
	for _, e := range t.byZID {
		out = append(out, e)
	}
	return out
}

// EvictExpired removes every entry whose LastSeenUnixMs + LeaseMs has
// elapsed. Returns the evicted entries so callers can fire any
// downstream "peer gone" hooks.
func (t *MulticastPeerTable) EvictExpired(now time.Time) []*MulticastPeerEntry {
	cut := now.UnixMilli()
	var dropped []*MulticastPeerEntry
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, e := range t.byZID {
		if e.LastSeenUnixMs.Load()+int64(e.LeaseMs) < cut {
			delete(t.byZID, k)
			delete(t.bySrcAddr, e.SrcAddr.String())
			dropped = append(dropped, e)
		}
	}
	return dropped
}

// MulticastConfig captures the inputs the Multicast lifecycle needs.
type MulticastConfig struct {
	Link                  *transport.UDPMulticastLink
	ZID                   wire.ZenohID
	WhatAmI               wire.WhatAmI
	LeaseMillis           uint64
	KeepAliveDivisor      int // JOIN cadence = lease / divisor; 0 → 4
	Resolution            wire.Resolution
	BatchSize             uint16
	EvictionInterval      time.Duration // 0 → lease/4
	OnPeerJoin, OnPeerGone func(*MulticastPeerEntry)
	Logger                *slog.Logger
}

// MulticastRuntime owns the multicast Link and the goroutines that
// service it: JOIN-send loop, JOIN-receive loop, eviction loop.
type MulticastRuntime struct {
	link    *transport.UDPMulticastLink
	table   *MulticastPeerTable
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	logger  *slog.Logger
	closed  atomic.Bool
}

// Table is the live peer table.
func (rt *MulticastRuntime) Table() *MulticastPeerTable { return rt.table }

// Close cancels the lifecycle and joins every owned goroutine. Idempotent.
func (rt *MulticastRuntime) Close() error {
	if rt.closed.Swap(true) {
		return nil
	}
	rt.cancel()
	_ = rt.link.Close()
	rt.wg.Wait()
	return nil
}

// StartMulticastPeer brings up the multicast lifecycle. The returned
// MulticastRuntime owns the goroutines; call Close to tear them down.
//
// Pub/sub propagation through multicast FRAME bodies is not yet wired
// — this MVP is discovery-only: peers see each other's JOIN messages
// and populate the peer table, but there is no bi-directional data
// path. Calling Put on a Session whose only transport is multicast
// returns ErrSessionNotReady today.
func (s *Session) StartMulticastPeer(cfg MulticastConfig) (*MulticastRuntime, error) {
	if cfg.Link == nil {
		return nil, errors.New("multicast: nil link")
	}
	if cfg.LeaseMillis == 0 {
		return nil, errors.New("multicast: LeaseMillis must be > 0")
	}
	if cfg.KeepAliveDivisor <= 0 {
		cfg.KeepAliveDivisor = 4
	}
	if cfg.EvictionInterval == 0 {
		cfg.EvictionInterval = time.Duration(cfg.LeaseMillis/4) * time.Millisecond
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = uint16(transport.MulticastBatchSize)
	}
	if cfg.Resolution == 0 {
		cfg.Resolution = wire.DefaultResolution
	}
	if cfg.Logger == nil {
		cfg.Logger = s.logger
	}

	ctx, cancel := context.WithCancel(context.Background())
	rt := &MulticastRuntime{
		link:   cfg.Link,
		table:  NewMulticastPeerTable(),
		cancel: cancel,
		logger: cfg.Logger,
	}
	joinInterval := time.Duration(cfg.LeaseMillis) * time.Millisecond / time.Duration(cfg.KeepAliveDivisor)

	rt.wg.Go(func() { rt.runJoinSendLoop(ctx, cfg, joinInterval) })
	rt.wg.Go(func() { rt.runReadLoop(ctx, cfg) })
	rt.wg.Go(func() { rt.runEvictionLoop(ctx, cfg) })
	return rt, nil
}

// runJoinSendLoop emits JOIN every joinInterval. Misses are logged at
// Debug — the next tick re-emits, so a single hiccup doesn't tear the
// session down.
func (rt *MulticastRuntime) runJoinSendLoop(ctx context.Context, cfg MulticastConfig, interval time.Duration) {
	body, err := encodeJoin(cfg)
	if err != nil {
		rt.logger.Warn("multicast: encode initial JOIN failed", "err", err)
		return
	}
	if err := rt.link.WriteBatch(body); err != nil {
		rt.logger.Debug("multicast: initial JOIN send failed", "err", err)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			body, err := encodeJoin(cfg)
			if err != nil {
				rt.logger.Warn("multicast: encode JOIN failed", "err", err)
				continue
			}
			if err := rt.link.WriteBatch(body); err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				rt.logger.Debug("multicast: JOIN send failed", "err", err)
			}
		}
	}
}

// runReadLoop reads datagrams off the listen socket, decodes the
// transport header, and either inserts into the peer table (JOIN) or
// drops the body (FRAME/FRAGMENT propagation is a follow-up).
func (rt *MulticastRuntime) runReadLoop(ctx context.Context, cfg MulticastConfig) {
	go func() {
		<-ctx.Done()
		_ = rt.link.Close()
	}()
	buf := make([]byte, transport.MulticastBatchSize)
	for {
		n, src, err := rt.link.ReadBatchFrom(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			rt.logger.Debug("multicast: read failed", "err", err)
			continue
		}
		rt.handleDatagram(cfg, src, buf[:n])
	}
}

func (rt *MulticastRuntime) handleDatagram(cfg MulticastConfig, src *net.UDPAddr, body []byte) {
	r := codec.NewReader(body)
	h, err := r.DecodeHeader()
	if err != nil {
		rt.logger.Debug("multicast: header decode failed", "err", err)
		return
	}
	switch h.ID {
	case wire.IDTransportJoin:
		j, err := wire.DecodeJoin(r, h)
		if err != nil {
			rt.logger.Debug("multicast: JOIN decode failed", "err", err)
			return
		}
		// Self-ZID filter: ignore our own JOINs so loopback (or a host with
		// IP_MULTICAST_LOOP=1) does not register us as our own peer.
		if zidEqual(j.ZID.Bytes, cfg.ZID.Bytes) {
			return
		}
		entry, isNew := rt.table.Insert(j, src)
		if isNew && cfg.OnPeerJoin != nil {
			cfg.OnPeerJoin(entry)
		}
	default:
		// FRAME/FRAGMENT propagation through multicast is a follow-up;
		// silently drop unknown bodies for now.
		return
	}
}

// runEvictionLoop drops peers whose lease has expired.
func (rt *MulticastRuntime) runEvictionLoop(ctx context.Context, cfg MulticastConfig) {
	t := time.NewTicker(cfg.EvictionInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			for _, e := range rt.table.EvictExpired(now) {
				if cfg.OnPeerGone != nil {
					cfg.OnPeerGone(e)
				}
			}
		}
	}
}

// encodeJoin builds a JOIN message body from the multicast config.
func encodeJoin(cfg MulticastConfig) ([]byte, error) {
	j := &wire.Join{
		Version:     wire.ProtoVersion,
		ZID:         cfg.ZID,
		WhatAmI:     cfg.WhatAmI,
		HasSizeInfo: true,
		Resolution:  cfg.Resolution,
		BatchSize:   cfg.BatchSize,
		Lease:       cfg.LeaseMillis,
	}
	w := codec.NewWriter(64)
	if err := j.EncodeTo(w); err != nil {
		return nil, fmt.Errorf("encode JOIN: %w", err)
	}
	return w.Bytes(), nil
}

func zidEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
