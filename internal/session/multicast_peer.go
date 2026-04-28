package session

import (
	"bytes"
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
// lease expiry.
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

	// reasm reassembles per-peer FRAGMENT chains. Lazy — multicast
	// peers that only ever send FRAMEs (small payloads) never allocate
	// one. Per-peer because each peer has its own per-lane SN sequence
	// space; sharing one reassembler across peers would gap-detect
	// across unrelated streams.
	reasmMu sync.Mutex
	reasm   *transport.Reassembler
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
//
// j.ZID.Bytes aliases the reader buffer; the next ReadBatchFrom will
// overwrite that backing storage. We clone the ZID bytes when building
// a fresh entry so the registry — and every consumer that copies
// entry.ZID by value (Snapshot, dispatchers, MulticastPeers API) —
// keeps a stable view that does not mutate as new datagrams arrive.
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
		ZID:        wire.ZenohID{Bytes: bytes.Clone(j.ZID.Bytes)},
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

// MulticastInboundDispatch is the receive-side callback the multicast
// runtime invokes for each NetworkMessage extracted from a peer's FRAME
// or reassembled FRAGMENT chain. srcZID identifies the originating peer
// (looked up from the datagram's source address against the JOIN-built
// peer table); the Reader is positioned right after the header byte and
// must be fully consumed by the dispatch.
type MulticastInboundDispatch func(srcZID wire.ZenohID, h codec.Header, r *codec.Reader) error

// MulticastConfig captures the inputs the Multicast lifecycle needs.
type MulticastConfig struct {
	Link             *transport.UDPMulticastLink
	ZID              wire.ZenohID
	WhatAmI          wire.WhatAmI
	LeaseMillis      uint64
	KeepAliveDivisor int // JOIN cadence = lease / divisor; 0 → 4
	Resolution       wire.Resolution
	BatchSize        uint16
	// InitialSNs are advertised in JOIN and seeded into the outbound
	// batcher. Why both: receivers validate FRAME SNs against what
	// JOIN announced, mirroring rust's tct.sync(*sn). Zero-valued =
	// auto-generate via GenerateMulticastInitialSNs.
	InitialSNs wire.LaneSNs
	MaxMsgSz   int // per-peer FRAGMENT reassembly cap; 0 → 1 GiB
	OutQSize   int // outbound buffer depth; 0 → 1024
	Dispatch   MulticastInboundDispatch
	EvictionInterval time.Duration // 0 → lease/4
	// OnPeerJoin / OnPeerGone receive the runtime alongside the entry
	// so the callback can target it without a separate registration
	// step — avoids the "first JOIN arrives before we've stored the
	// runtime pointer externally" race.
	OnPeerJoin, OnPeerGone func(*MulticastRuntime, *MulticastPeerEntry)
	Logger                 *slog.Logger
}

// MulticastRuntime owns the multicast Link and the goroutines that
// service it: JOIN-send loop, JOIN-receive loop, outbound writer, and
// eviction loop. The outbound writer is the same writerLoop the unicast
// Runtime uses; multicast just feeds it a UDPMulticastLink.
type MulticastRuntime struct {
	link       *transport.UDPMulticastLink
	table      *MulticastPeerTable
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	logger     *slog.Logger
	closed     atomic.Bool
	outQ       chan OutboundItem
	batcher    *transport.Batcher
	selfZID    wire.ZenohID
	dispatch   MulticastInboundDispatch
	maxMsgSz   int
	dropped    atomic.Uint64
	linkClosed chan struct{}
	writerDone chan struct{}
}

// Table is the live peer table.
func (rt *MulticastRuntime) Table() *MulticastPeerTable { return rt.table }

// Dropped returns the cumulative count of best-effort outbound items
// the multicast OutQ refused because it was full.
func (rt *MulticastRuntime) Dropped() uint64 { return rt.dropped.Load() }

// Enqueue queues item for transmission on the multicast group.
// Non-blocking: returns true on accept, false on full-queue or
// torn-down runtime. Multicast is best-effort by design — the caller
// has no useful retry, so the policy is "drop and move on."
func (rt *MulticastRuntime) Enqueue(item OutboundItem) bool {
	select {
	case rt.outQ <- item:
		return true
	case <-rt.linkClosed:
		return false
	default:
		// Sample drop logging at powers of two so a sustained drop
		// storm doesn't flood the log; ops still gets a heartbeat.
		n := rt.dropped.Add(1)
		if n&(n-1) == 0 {
			rt.logger.Debug("multicast: outQ full, dropping",
				"total_drops", n)
		}
		return false
	}
}

// Close cancels the lifecycle and joins every owned goroutine. Idempotent.
func (rt *MulticastRuntime) Close() error {
	if rt.closed.Swap(true) {
		return nil
	}
	rt.cancel()
	close(rt.linkClosed)
	_ = rt.link.Close()
	rt.wg.Wait()
	return nil
}

// StartMulticastPeer brings up the multicast lifecycle: the JOIN
// send/receive loops, an outbound writer driving the same writerLoop
// the unicast Runtime uses, per-peer FRAGMENT reassembly, and the
// eviction loop. The returned MulticastRuntime owns the goroutines;
// call Close to tear them down.
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
	if cfg.InitialSNs == (wire.LaneSNs{}) {
		cfg.InitialSNs = wire.GenerateMulticastInitialSNs(cfg.Resolution)
	}
	if cfg.MaxMsgSz <= 0 {
		cfg.MaxMsgSz = 1 << 30
	}
	if cfg.OutQSize <= 0 {
		cfg.OutQSize = 1024
	}
	if cfg.Logger == nil {
		cfg.Logger = s.logger
	}

	ctx, cancel := context.WithCancel(context.Background())
	seeds := laneSeedsFromSNs(cfg.InitialSNs)
	batcher := transport.NewBatcherWithLaneSeeds(int(cfg.BatchSize), true /* attachQoS */, seeds, cfg.Link.WriteBatch)
	rt := &MulticastRuntime{
		link:       cfg.Link,
		table:      NewMulticastPeerTable(),
		cancel:     cancel,
		logger:     cfg.Logger,
		outQ:       make(chan OutboundItem, cfg.OutQSize),
		batcher:    batcher,
		selfZID:    cfg.ZID,
		dispatch:   cfg.Dispatch,
		maxMsgSz:   cfg.MaxMsgSz,
		linkClosed: make(chan struct{}),
		writerDone: make(chan struct{}),
	}
	joinInterval := time.Duration(cfg.LeaseMillis) * time.Millisecond / time.Duration(cfg.KeepAliveDivisor)

	rt.wg.Go(func() { rt.runJoinSendLoop(ctx, cfg, joinInterval) })
	rt.wg.Go(func() { rt.runReadLoop(ctx, cfg) })
	rt.wg.Go(func() { rt.runEvictionLoop(ctx, cfg) })
	// writerLoop's drain channel is never closed — multicast is
	// fire-and-forget, no graceful flush semantics. linkClosed acts as
	// the stop signal.
	neverDrain := make(chan struct{})
	rt.wg.Go(func() {
		writerLoop(rt.outQ, rt.batcher, rt.link, rt.linkClosed, neverDrain, rt.writerDone, rt.logger)
	})
	return rt, nil
}

// laneSeedsFromSNs converts the wire.LaneSNs (priority-major, two
// arrays for reliable / best-effort) into the [8][2]uint64 layout the
// transport.Batcher uses ([priority][reliable]).
func laneSeedsFromSNs(sns wire.LaneSNs) [8][2]uint64 {
	var seeds [8][2]uint64
	for p := range 8 {
		seeds[p][0] = sns.BestEffort[p]
		seeds[p][1] = sns.Reliable[p]
	}
	return seeds
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

// runReadLoop reads datagrams off the listen socket and dispatches
// them through handleDatagram: JOINs update the peer table, FRAMEs and
// FRAGMENTs route through the per-peer Reassembler and the configured
// MulticastInboundDispatch.
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
			cfg.OnPeerJoin(rt, entry)
		}
	case wire.IDTransportFrame:
		entry := rt.peerForDatagram(src)
		if entry == nil {
			return
		}
		frame, err := wire.DecodeFrame(r, h)
		if err != nil {
			rt.logger.Debug("multicast: FRAME decode failed", "err", err, "peer", entry.ZID)
			return
		}
		rt.dispatchMulticastFrameBody(entry, frame.Body)
	case wire.IDTransportFragment:
		entry := rt.peerForDatagram(src)
		if entry == nil {
			return
		}
		frag, err := wire.DecodeFragment(r, h)
		if err != nil {
			rt.logger.Debug("multicast: FRAGMENT decode failed", "err", err, "peer", entry.ZID)
			return
		}
		lane := transport.LaneKey{
			Priority: transport.LanePriorityFromExts(frag.Extensions),
			Reliable: frag.Reliable,
		}
		complete := rt.pushFragment(entry, lane, frag.SeqNum, frag.More, frag.Body)
		if complete != nil {
			rt.dispatchMulticastFrameBody(entry, complete)
		}
	default:
		// Transport OAM and other transport messages aren't part of
		// the multicast pub/sub data path; drop quietly.
		return
	}
}

// peerForDatagram looks up the JOIN-built entry for a datagram's
// source address. Returns nil for unknown sources (the JOIN flood will
// repair the table on the next interval). Loopback safety is upstream:
// the JOIN handler drops our own JOIN before Insert, so bySrcAddr
// never contains our own send-socket port — a self-loop FRAME from a
// multi-NIC / SSM rebound exits here via the nil entry, not via a
// per-FRAME ZID compare.
func (rt *MulticastRuntime) peerForDatagram(src *net.UDPAddr) *MulticastPeerEntry {
	entry := rt.table.LookupBySrcAddr(src)
	if entry == nil {
		rt.logger.Debug("multicast: data from unknown peer; dropping until JOIN seen",
			"src", src.String())
		return nil
	}
	return entry
}

// pushFragment lazily allocates the per-peer Reassembler under the
// entry's mutex, pushes the fragment, and returns the assembled body
// (or nil when more fragments are pending). SN gaps and oversize
// errors clear the buffer and return nil so the next valid fragment
// chain can start fresh.
func (rt *MulticastRuntime) pushFragment(entry *MulticastPeerEntry, lane transport.LaneKey, sn uint64, more bool, body []byte) []byte {
	entry.reasmMu.Lock()
	defer entry.reasmMu.Unlock()
	if entry.reasm == nil {
		entry.reasm = transport.NewReassembler(rt.maxMsgSz)
	}
	complete, err := entry.reasm.Push(lane, sn, more, body)
	if err != nil {
		rt.logger.Debug("multicast: fragment reassembly reset",
			"err", err, "peer", entry.ZID, "lane", lane)
		return nil
	}
	return complete
}

// dispatchMulticastFrameBody walks the self-delimiting NetworkMessage
// chain inside a FRAME body (or fully-reassembled FRAGMENT chain) and
// hands each one to the configured Dispatch with the source peer's
// ZID. The unicast equivalent is reader.go:dispatchFrameBody; we keep
// a parallel implementation here so the multicast Dispatch signature
// can carry srcZID without changing the unicast one.
func (rt *MulticastRuntime) dispatchMulticastFrameBody(entry *MulticastPeerEntry, body []byte) {
	if rt.dispatch == nil {
		return
	}
	r := codec.NewReader(body)
	for r.Len() > 0 {
		h, err := r.DecodeHeader()
		if err != nil {
			rt.logger.Debug("multicast: network header decode failed",
				"err", err, "peer", entry.ZID)
			return
		}
		before := r.Len()
		if err := rt.dispatch(entry.ZID, h, r); err != nil {
			rt.logger.Debug("multicast: dispatch failed",
				"err", err, "peer", entry.ZID, "msg_id", h.ID)
			return
		}
		if r.Len() > 0 && r.Len() == before {
			rt.logger.Warn("multicast: dispatch did not consume body bytes; dropping rest",
				"peer", entry.ZID, "msg_id", h.ID)
			return
		}
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
					cfg.OnPeerGone(rt, e)
				}
			}
		}
	}
}

// encodeJoin builds a JOIN message body from the multicast config. The
// per-lane initial SNs go in the QoS-ZBuf extension via AttachLaneSNs;
// the legacy aggregate NextSNRel/NextSNBE fields are filled from the
// Data-priority lane so non-QoS receivers still have a usable starting
// SN.
func encodeJoin(cfg MulticastConfig) ([]byte, error) {
	j := &wire.Join{
		Version:     wire.ProtoVersion,
		ZID:         cfg.ZID,
		WhatAmI:     cfg.WhatAmI,
		HasSizeInfo: true,
		Resolution:  cfg.Resolution,
		BatchSize:   cfg.BatchSize,
		Lease:       cfg.LeaseMillis,
		NextSNRel:   cfg.InitialSNs.Reliable[wire.QoSPriorityData],
		NextSNBE:    cfg.InitialSNs.BestEffort[wire.QoSPriorityData],
	}
	if err := j.AttachLaneSNs(cfg.InitialSNs); err != nil {
		return nil, fmt.Errorf("encode JOIN: attach lane SNs: %w", err)
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
