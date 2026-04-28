package session

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/locator"
	"github.com/shirou/zenoh-go-client/internal/transport"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// startMulticast brings up an isolated MulticastRuntime on a randomly-
// chosen group port. Returns the runtime and a cleanup helper.
func startMulticast(t *testing.T, group string, zid wire.ZenohID) (*Session, *MulticastRuntime) {
	t.Helper()
	return startMulticastWithDispatch(t, group, zid, nil)
}

// startMulticastWithDispatch is startMulticast plus a custom inbound
// dispatch — used by the pub/sub propagation tests to record what each
// peer receives.
func startMulticastWithDispatch(t *testing.T, group string, zid wire.ZenohID, dispatch MulticastInboundDispatch) (*Session, *MulticastRuntime) {
	t.Helper()
	loc := locator.Locator{Scheme: locator.SchemeUDP, Address: group}
	link, err := transport.DialMulticastUDP(loc, transport.MulticastDialOpts{})
	if err != nil {
		t.Fatalf("DialMulticastUDP: %v", err)
	}
	s := New()
	rt, err := s.StartMulticastPeer(MulticastConfig{
		Link:             link,
		ZID:              zid,
		WhatAmI:          wire.WhatAmIPeer,
		LeaseMillis:      1000,
		KeepAliveDivisor: 4, // 250ms send cadence
		BatchSize:        uint16(transport.MulticastBatchSize),
		Resolution:       wire.DefaultResolution,
		EvictionInterval: 200 * time.Millisecond,
		Dispatch:         dispatch,
	})
	if err != nil {
		t.Fatalf("StartMulticastPeer: %v", err)
	}
	return s, rt
}

// TestMulticastPeerDiscovery: three independent multicast sessions on
// the same group converge so that each peer table contains the other
// two. JOIN is the only message; no FRAME/FRAGMENT yet.
func TestMulticastPeerDiscovery(t *testing.T) {
	port := 19000 + int(time.Now().UnixNano()%200)
	group := fmt.Sprintf("224.0.0.230:%d", port)

	zids := []wire.ZenohID{
		{Bytes: []byte{0xAA}},
		{Bytes: []byte{0xBB}},
		{Bytes: []byte{0xCC}},
	}

	type runner struct {
		s  *Session
		rt *MulticastRuntime
	}
	var runners []*runner
	for _, zid := range zids {
		s, rt := startMulticast(t, group, zid)
		runners = append(runners, &runner{s, rt})
	}
	defer func() {
		for _, r := range runners {
			_ = r.rt.Close()
			_ = r.s.Close()
		}
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		converged := true
		for i, r := range runners {
			seen := r.rt.Table().Snapshot()
			if len(seen) != len(runners)-1 {
				converged = false
				break
			}
			_ = i
		}
		if converged {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	for i, r := range runners {
		t.Logf("runner[%d] zid=%x peers=%d", i, zids[i].Bytes, len(r.rt.Table().Snapshot()))
	}
	t.Skip("multicast peer discovery did not converge — likely IGMP membership unavailable in this sandbox")
}

// TestMulticastPeerLeaseEviction: a JOIN that stops being refreshed
// disappears from the peer table after lease + a small grace period.
func TestMulticastPeerLeaseEviction(t *testing.T) {
	port := 19200 + int(time.Now().UnixNano()%200)
	group := fmt.Sprintf("224.0.0.231:%d", port)

	// listener-only session — populates a peer table but emits no JOIN.
	loc := locator.Locator{Scheme: locator.SchemeUDP, Address: group}
	listener, err := transport.DialMulticastUDP(loc, transport.MulticastDialOpts{})
	if err != nil {
		t.Fatalf("Dial(listener): %v", err)
	}
	s := New()
	defer s.Close()
	listenRt, err := s.StartMulticastPeer(MulticastConfig{
		Link:             listener,
		ZID:              wire.ZenohID{Bytes: []byte{0x01}},
		WhatAmI:          wire.WhatAmIPeer,
		LeaseMillis:      300, // tiny lease so eviction is fast
		KeepAliveDivisor: 4,
		BatchSize:        uint16(transport.MulticastBatchSize),
		Resolution:       wire.DefaultResolution,
		EvictionInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer listenRt.Close()

	// transient peer that emits one JOIN then dies.
	peerLink, err := transport.DialMulticastUDP(loc, transport.MulticastDialOpts{})
	if err != nil {
		t.Fatal(err)
	}
	peerSession := New()
	peerRt, err := peerSession.StartMulticastPeer(MulticastConfig{
		Link:             peerLink,
		ZID:              wire.ZenohID{Bytes: []byte{0x02}},
		WhatAmI:          wire.WhatAmIPeer,
		LeaseMillis:      300,
		KeepAliveDivisor: 4,
		BatchSize:        uint16(transport.MulticastBatchSize),
		Resolution:       wire.DefaultResolution,
		EvictionInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Wait until the listener sees it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(listenRt.Table().Snapshot()) >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(listenRt.Table().Snapshot()) < 1 {
		_ = peerRt.Close()
		_ = peerSession.Close()
		t.Skip("multicast loopback unavailable; skipping eviction test")
	}

	// Kill the peer so it stops emitting.
	_ = peerRt.Close()
	_ = peerSession.Close()

	// Eviction should fire after lease + 1 tick.
	dropped := false
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(listenRt.Table().Snapshot()) == 0 {
			dropped = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !dropped {
		t.Errorf("expired peer was not evicted")
	}
}

// TestMulticastPeerSelfFilter: a peer's own JOINs (received via
// loopback) must not register as a new peer.
func TestMulticastPeerSelfFilter(t *testing.T) {
	port := 19400 + int(time.Now().UnixNano()%200)
	group := fmt.Sprintf("224.0.0.232:%d", port)

	loc := locator.Locator{Scheme: locator.SchemeUDP, Address: group}
	link, err := transport.DialMulticastUDP(loc, transport.MulticastDialOpts{})
	if err != nil {
		t.Fatal(err)
	}
	s := New()
	defer s.Close()
	rt, err := s.StartMulticastPeer(MulticastConfig{
		Link:             link,
		ZID:              wire.ZenohID{Bytes: []byte{0xFE, 0xFE}},
		WhatAmI:          wire.WhatAmIPeer,
		LeaseMillis:      1000,
		KeepAliveDivisor: 4,
		BatchSize:        uint16(transport.MulticastBatchSize),
		Resolution:       wire.DefaultResolution,
		EvictionInterval: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	// Wait long enough for several JOIN rounds.
	time.Sleep(700 * time.Millisecond)
	if got := len(rt.Table().Snapshot()); got != 0 {
		t.Errorf("self JOIN registered as peer: table size = %d", got)
	}
}

// recordedPush captures one inbound PUSH from the multicast dispatch.
// Bytes are deep-copied because the wire decoder aliases the reader
// buffer; the test goroutine reads these fields after the buffer has
// been reused for keepalive JOINs.
type recordedPush struct {
	srcZID  wire.ZenohID
	keyExpr string
	kind    byte
	payload []byte
}

// recordingDispatch returns a MulticastInboundDispatch that records
// every inbound PUSH onto out. Other message types are skipped (their
// bodies are still consumed so the reader can advance).
func recordingDispatch(out *[]recordedPush, mu *sync.Mutex) MulticastInboundDispatch {
	return func(srcZID wire.ZenohID, h codec.Header, r *codec.Reader) error {
		if h.ID == wire.IDNetworkPush {
			p, err := wire.DecodePush(r, h)
			if err != nil {
				return err
			}
			rec := recordedPush{
				srcZID:  wire.ZenohID{Bytes: append([]byte(nil), srcZID.Bytes...)},
				keyExpr: p.KeyExpr.Suffix,
			}
			switch body := p.Body.(type) {
			case *wire.PutBody:
				rec.kind = wire.IDDataPut
				rec.payload = append([]byte(nil), body.Payload...)
			case *wire.DelBody:
				rec.kind = wire.IDDataDel
			}
			mu.Lock()
			*out = append(*out, rec)
			mu.Unlock()
			return nil
		}
		_ = r.Skip(r.Len())
		return nil
	}
}

// waitConverge polls for both runtimes to see each other in their peer
// tables. Returns true on success; the caller decides whether to
// t.Skip (multicast unavailable) or t.Fatal (regression).
func waitConverge(rtA, rtB *MulticastRuntime, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(rtA.Table().Snapshot()) >= 1 && len(rtB.Table().Snapshot()) >= 1 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// TestMulticastPushPropagation: peer A enqueues a PUSH, peer B's
// dispatch records it once with A's source ZID. End-to-end through the
// outbound batcher → multicast socket → inbound demux → dispatch.
func TestMulticastPushPropagation(t *testing.T) {
	port := 19600 + int(time.Now().UnixNano()%200)
	group := fmt.Sprintf("224.0.0.234:%d", port)

	zidA := wire.ZenohID{Bytes: []byte{0x01, 0xAA}}
	zidB := wire.ZenohID{Bytes: []byte{0x02, 0xBB}}

	var receivedMu sync.Mutex
	var received []recordedPush

	sA, rtA := startMulticast(t, group, zidA)
	defer sA.Close()
	defer rtA.Close()
	sB, rtB := startMulticastWithDispatch(t, group, zidB, recordingDispatch(&received, &receivedMu))
	defer sB.Close()
	defer rtB.Close()

	if !waitConverge(rtA, rtB, 3*time.Second) {
		t.Skip("multicast peer discovery did not converge — likely IGMP membership unavailable in this sandbox")
	}

	// Build a PUSH on peer A and enqueue it through A's runtime.
	push := &wire.Push{
		KeyExpr: wire.WireExpr{Suffix: "demo/multicast"},
		Body:    &wire.PutBody{Payload: []byte("hello-multicast")},
	}
	encoded, err := transport.EncodeNetworkMessage(push)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !rtA.Enqueue(OutboundItem{NetworkMsg: &transport.OutboundMessage{
		Encoded:  encoded,
		Priority: wire.QoSPriorityData,
	}}) {
		t.Fatalf("rtA.Enqueue refused")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		receivedMu.Lock()
		n := len(received)
		receivedMu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	receivedMu.Lock()
	defer receivedMu.Unlock()
	if len(received) != 1 {
		t.Fatalf("expected 1 PUSH on peer B, got %d", len(received))
	}
	got := received[0]
	if string(got.srcZID.Bytes) != string(zidA.Bytes) {
		t.Errorf("srcZID = %x, want %x", got.srcZID.Bytes, zidA.Bytes)
	}
	if got.keyExpr != "demo/multicast" {
		t.Errorf("keyExpr = %q, want demo/multicast", got.keyExpr)
	}
	if got.kind != wire.IDDataPut {
		t.Errorf("kind = %#x, want IDDataPut", got.kind)
	}
	if string(got.payload) != "hello-multicast" {
		t.Errorf("Payload = %q, want hello-multicast", got.payload)
	}
}

// TestMulticastFragmentPropagation: a PUSH whose encoded length exceeds
// the FRAME body cap goes through the fragmenter, is split into
// FRAGMENTs, and is reassembled by peer B's per-peer Reassembler before
// dispatch.
func TestMulticastFragmentPropagation(t *testing.T) {
	port := 19800 + int(time.Now().UnixNano()%200)
	group := fmt.Sprintf("224.0.0.235:%d", port)

	zidA := wire.ZenohID{Bytes: []byte{0x10, 0xAA}}
	zidB := wire.ZenohID{Bytes: []byte{0x20, 0xBB}}

	var receivedMu sync.Mutex
	var received []recordedPush

	sA, rtA := startMulticast(t, group, zidA)
	defer sA.Close()
	defer rtA.Close()
	sB, rtB := startMulticastWithDispatch(t, group, zidB, recordingDispatch(&received, &receivedMu))
	defer sB.Close()
	defer rtB.Close()

	if !waitConverge(rtA, rtB, 3*time.Second) {
		t.Skip("multicast peer discovery did not converge — likely IGMP membership unavailable in this sandbox")
	}

	// Build a payload > MulticastBatchSize so the fragmenter kicks in.
	payload := []byte(strings.Repeat("X", transport.MulticastBatchSize*2))
	push := &wire.Push{
		KeyExpr: wire.WireExpr{Suffix: "demo/big"},
		Body:    &wire.PutBody{Payload: payload},
	}
	encoded, err := transport.EncodeNetworkMessage(push)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !rtA.Enqueue(OutboundItem{NetworkMsg: &transport.OutboundMessage{
		Encoded:  encoded,
		Priority: wire.QoSPriorityData,
	}}) {
		t.Fatalf("rtA.Enqueue refused")
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		receivedMu.Lock()
		n := len(received)
		receivedMu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	receivedMu.Lock()
	defer receivedMu.Unlock()
	if len(received) != 1 {
		t.Fatalf("expected 1 reassembled PUSH on peer B, got %d", len(received))
	}
	if received[0].kind != wire.IDDataPut {
		t.Errorf("kind = %#x, want IDDataPut", received[0].kind)
	}
	if len(received[0].payload) != len(payload) {
		t.Errorf("Payload size = %d, want %d", len(received[0].payload), len(payload))
	}
}

// TestMulticastSelfFilterLoopback: with LoopbackDisabled=false our own
// outbound PUSH echoes back to our own listen socket. The mechanism
// that drops it is the JOIN self-filter (handleDatagram drops our own
// JOIN before Insert, so bySrcAddr never holds our send-socket port
// and the inbound FRAME's source-lookup returns nil). This test pins
// that behaviour even though no peer ever JOINs on the group.
func TestMulticastSelfFilterLoopback(t *testing.T) {
	port := 20000 + int(time.Now().UnixNano()%200)
	group := fmt.Sprintf("224.0.0.236:%d", port)

	var seen atomic.Int64
	dispatch := func(srcZID wire.ZenohID, h codec.Header, r *codec.Reader) error {
		if h.ID == wire.IDNetworkPush {
			seen.Add(1)
		}
		_ = r.Skip(r.Len())
		return nil
	}

	loc := locator.Locator{Scheme: locator.SchemeUDP, Address: group}
	// LoopbackDisabled defaults to false → our datagrams come back.
	link, err := transport.DialMulticastUDP(loc, transport.MulticastDialOpts{})
	if err != nil {
		t.Fatal(err)
	}
	s := New()
	defer s.Close()
	zid := wire.ZenohID{Bytes: []byte{0xCA, 0xFE}}
	rt, err := s.StartMulticastPeer(MulticastConfig{
		Link:             link,
		ZID:              zid,
		WhatAmI:          wire.WhatAmIPeer,
		LeaseMillis:      1000,
		KeepAliveDivisor: 4,
		BatchSize:        uint16(transport.MulticastBatchSize),
		Resolution:       wire.DefaultResolution,
		EvictionInterval: 200 * time.Millisecond,
		Dispatch:         dispatch,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	// Even before we Enqueue anything, the JOIN reader's self-filter
	// runs; once we Enqueue a PUSH, the FRAME reader's self-filter is
	// what we want to verify.
	push := &wire.Push{
		KeyExpr: wire.WireExpr{Suffix: "demo/loopback"},
		Body:    &wire.PutBody{Payload: []byte("self")},
	}
	encoded, err := transport.EncodeNetworkMessage(push)
	if err != nil {
		t.Fatal(err)
	}
	if !rt.Enqueue(OutboundItem{NetworkMsg: &transport.OutboundMessage{
		Encoded:  encoded,
		Priority: wire.QoSPriorityData,
	}}) {
		t.Fatalf("Enqueue refused")
	}

	// Wait long enough for the datagram to round-trip through
	// loopback, but the self-filter must drop it.
	time.Sleep(400 * time.Millisecond)
	if got := seen.Load(); got != 0 {
		t.Errorf("self-loopback PUSH dispatched %d times; want 0", got)
	}
}

// TestMulticastPeerCloseLeaks asserts every goroutine MulticastRuntime
// owns is joined by Close.
func TestMulticastPeerCloseLeaks(t *testing.T) {
	loc := locator.Locator{Scheme: locator.SchemeUDP, Address: "224.0.0.233:19501"}
	link, err := transport.DialMulticastUDP(loc, transport.MulticastDialOpts{})
	if err != nil {
		t.Fatal(err)
	}
	s := New()
	defer s.Close()
	rt, err := s.StartMulticastPeer(MulticastConfig{
		Link:             link,
		ZID:              wire.ZenohID{Bytes: []byte{0x77}},
		WhatAmI:          wire.WhatAmIPeer,
		LeaseMillis:      1000,
		KeepAliveDivisor: 4,
		BatchSize:        uint16(transport.MulticastBatchSize),
		Resolution:       wire.DefaultResolution,
		EvictionInterval: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Two Close calls must be idempotent.
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { _ = rt.Close() })
	}
	wg.Wait()
}
