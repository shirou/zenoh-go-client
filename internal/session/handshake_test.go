package session

import (
	"crypto/rand"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/transport"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// mockPeer responds to an INIT SYN / OPEN SYN with matching ACKs. Used to
// exercise DoHandshake from the initiator side over a pipe link.
type mockPeer struct {
	ZID     wire.ZenohID
	WhatAmI wire.WhatAmI
	// Overrides for negotiation. Zero value = accept initiator's proposal.
	BatchSize  uint16
	Resolution wire.Resolution
	Lease      uint64 // ms
	EchoQoS    bool   // include QoS ext in INIT/OPEN ACK
	CookieLen  int    // 0 → 8-byte cookie
}

// RunOnce reads INIT SYN then OPEN SYN on link and writes the matching ACKs.
// Returns after OPEN ACK is written.
func (p *mockPeer) RunOnce(link transport.Link) error {
	// ---- INIT SYN ----
	initSyn, err := readTransportMsg[*wire.Init](link, wire.IDTransportInit, wire.DecodeInit)
	if err != nil {
		return err
	}
	if initSyn.Ack {
		return io.ErrUnexpectedEOF
	}

	// cookie
	cookieLen := p.CookieLen
	if cookieLen == 0 {
		cookieLen = 8
	}
	cookie := make([]byte, cookieLen)
	if _, err := rand.Read(cookie); err != nil {
		return err
	}

	ack := &wire.Init{
		Ack:         true,
		Version:     wire.ProtoVersion,
		ZID:         p.ZID,
		WhatAmI:     p.WhatAmI,
		HasSizeInfo: true,
		Resolution:  p.Resolution,
		BatchSize:   p.BatchSize,
		Cookie:      cookie,
	}
	if ack.BatchSize == 0 {
		ack.BatchSize = initSyn.BatchSize
	}
	if ack.Resolution == 0 {
		ack.Resolution = initSyn.Resolution
	}
	if p.EchoQoS {
		ack.Extensions = append(ack.Extensions, codec.Extension{
			Header: codec.ExtHeader{ID: wire.ExtIDQoS, Encoding: codec.ExtEncUnit},
		})
	}
	if err := writeTransport(link, ack); err != nil {
		return err
	}

	// ---- OPEN SYN ----
	openSyn, err := readTransportMsg[*wire.Open](link, wire.IDTransportOpen, wire.DecodeOpen)
	if err != nil {
		return err
	}
	// Echo lease + our own initial_sn.
	ourInitialSN := ComputeInitialSN(p.ZID.Bytes, initSyn.ZID.Bytes, ack.Resolution.Bits(wire.FieldFrameSN))
	lease := p.Lease
	if lease == 0 {
		lease = openSyn.Lease
	}
	openAck := &wire.Open{
		Ack:       true,
		Lease:     lease,
		InitialSN: ourInitialSN,
	}
	if p.EchoQoS {
		openAck.Extensions = append(openAck.Extensions, codec.Extension{
			Header: codec.ExtHeader{ID: wire.ExtIDQoS, Encoding: codec.ExtEncUnit},
		})
	}
	return writeTransport(link, openAck)
}

// readTransportMsg reads one batch, expects header id, and decodes via the
// provided decoder. Type parameter T is the concrete message pointer type.
func readTransportMsg[T any](link transport.Link, wantID byte, dec func(*codec.Reader, codec.Header) (T, error)) (T, error) {
	var zero T
	buf := make([]byte, codec.MaxBatchSize)
	n, err := link.ReadBatch(buf)
	if err != nil {
		return zero, err
	}
	r := codec.NewReader(buf[:n])
	h, err := r.DecodeHeader()
	if err != nil {
		return zero, err
	}
	if h.ID != wantID {
		return zero, io.ErrUnexpectedEOF
	}
	return dec(r, h)
}

// ----------------------------------------------------------------------------
// Tests
// ----------------------------------------------------------------------------

func TestHandshakeHappyPath(t *testing.T) {
	clientLink, serverLink := newPipeLinks()
	defer clientLink.Close()
	defer serverLink.Close()

	peer := &mockPeer{
		ZID:     wire.ZenohID{Bytes: []byte{0xA1, 0xA2, 0xA3, 0xA4, 0xA5, 0xA6, 0xA7, 0xA8}},
		WhatAmI: wire.WhatAmIRouter,
		EchoQoS: true,
	}
	peerDone := make(chan error, 1)
	go func() { peerDone <- peer.RunOnce(serverLink) }()

	cfg := DefaultHandshakeConfig()
	cfg.ZID = GenerateZID()
	cfg.WhatAmI = wire.WhatAmIClient
	res, err := DoHandshake(clientLink, cfg)
	if err != nil {
		t.Fatalf("DoHandshake: %v", err)
	}
	if !res.PeerZID.Equal(peer.ZID) {
		t.Errorf("PeerZID mismatch: got % x, want % x", res.PeerZID.Bytes, peer.ZID.Bytes)
	}
	if res.PeerWhatAmI != wire.WhatAmIRouter {
		t.Errorf("PeerWhatAmI = %v, want Router", res.PeerWhatAmI)
	}
	if res.NegotiatedBatchSize != cfg.BatchSize {
		t.Errorf("BatchSize = %d, want %d", res.NegotiatedBatchSize, cfg.BatchSize)
	}
	if !res.QoSEnabled {
		t.Error("QoS should be enabled when both sides agree")
	}
	// initial_sn is deterministic from (my ZID, peer ZID, fsn bits).
	expected := ComputeInitialSN(cfg.ZID.Bytes, peer.ZID.Bytes, res.NegotiatedResolution.Bits(wire.FieldFrameSN))
	if res.MyInitialSN != expected {
		t.Errorf("MyInitialSN = %#x, want %#x", res.MyInitialSN, expected)
	}

	if err := <-peerDone; err != nil {
		t.Errorf("peer: %v", err)
	}
}

func TestHandshakeBatchSizeNegotiation(t *testing.T) {
	clientLink, serverLink := newPipeLinks()
	defer clientLink.Close()
	defer serverLink.Close()

	// Peer returns a smaller batch size; negotiation picks the min.
	peer := &mockPeer{
		ZID:       wire.ZenohID{Bytes: []byte{0xBB}},
		WhatAmI:   wire.WhatAmIRouter,
		BatchSize: 32768, // smaller than our default
	}
	go func() { _ = peer.RunOnce(serverLink) }()

	cfg := DefaultHandshakeConfig()
	cfg.ZID = GenerateZID()
	cfg.WhatAmI = wire.WhatAmIClient
	res, err := DoHandshake(clientLink, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.NegotiatedBatchSize != 32768 {
		t.Errorf("expected min batch_size 32768, got %d", res.NegotiatedBatchSize)
	}
}

func TestHandshakeConnectionToSelf(t *testing.T) {
	clientLink, serverLink := newPipeLinks()
	defer clientLink.Close()
	defer serverLink.Close()

	cfg := DefaultHandshakeConfig()
	cfg.ZID = GenerateZID()
	cfg.WhatAmI = wire.WhatAmIClient

	// Peer echoes OUR ZID back.
	peer := &mockPeer{
		ZID:     cfg.ZID,
		WhatAmI: wire.WhatAmIRouter,
	}
	go func() { _ = peer.RunOnce(serverLink) }()

	_, err := DoHandshake(clientLink, cfg)
	if err == nil {
		t.Fatal("expected CONNECTION_TO_SELF error")
	}
	if !IsConnectionToSelf(err) {
		t.Errorf("IsConnectionToSelf(err) should be true; got %v (%T)", err, err)
	}
}

func TestHandshakeVersionMismatch(t *testing.T) {
	clientLink, serverLink := newPipeLinks()
	defer clientLink.Close()
	defer serverLink.Close()

	// Peer writes an INIT ACK with wrong version.
	go func() {
		initSyn, err := readTransportMsg[*wire.Init](serverLink, wire.IDTransportInit, wire.DecodeInit)
		if err != nil {
			return
		}
		_ = initSyn
		ack := &wire.Init{
			Ack:         true,
			Version:     0xFF, // bad
			ZID:         wire.ZenohID{Bytes: []byte{1, 2}},
			WhatAmI:     wire.WhatAmIRouter,
			HasSizeInfo: true,
			BatchSize:   1024,
			Resolution:  wire.DefaultResolution,
			Cookie:      []byte{0, 0},
		}
		_ = writeTransport(serverLink, ack)
	}()

	cfg := DefaultHandshakeConfig()
	cfg.ZID = GenerateZID()
	cfg.WhatAmI = wire.WhatAmIClient
	_, err := DoHandshake(clientLink, cfg)
	if err == nil || !errorContains(err, "peer version") {
		t.Errorf("expected version mismatch error, got %v", err)
	}
}

func TestHandshakePeerClosedOnInit(t *testing.T) {
	clientLink, serverLink := newPipeLinks()
	defer clientLink.Close()
	defer serverLink.Close()

	// Peer reads INIT SYN then responds with CLOSE.
	go func() {
		_, _ = readTransportMsg[*wire.Init](serverLink, wire.IDTransportInit, wire.DecodeInit)
		_ = writeTransport(serverLink, &wire.Close{Session: true, Reason: CloseReasonUnsupported})
	}()

	cfg := DefaultHandshakeConfig()
	cfg.ZID = GenerateZID()
	cfg.WhatAmI = wire.WhatAmIClient
	_, err := DoHandshake(clientLink, cfg)
	if err == nil {
		t.Fatal("expected CloseReasonErr")
	}
	var ce *CloseReasonErr
	if !errors.As(err, &ce) {
		t.Fatalf("expected *CloseReasonErr, got %v (%T)", err, err)
	}
	if ce.Reason != CloseReasonUnsupported {
		t.Errorf("reason = %#x, want %#x", ce.Reason, CloseReasonUnsupported)
	}
}

func errorContains(err error, substr string) bool {
	return err != nil && strings.Contains(err.Error(), substr)
}
