package session

import (
	"bytes"
	"crypto/rand"
	"fmt"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/transport"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// AcceptConfig captures the locally-chosen parameters the responder is
// prepared to accept during INIT/OPEN. The responder picks min(local,
// proposed) for negotiable fields.
type AcceptConfig struct {
	ZID            wire.ZenohID
	WhatAmI        wire.WhatAmI
	BatchSize      uint16
	LeaseMillis    uint64
	Resolution     wire.Resolution
	EnableQoS      bool
	MaxMessageSize int
}

// DefaultAcceptConfig returns an AcceptConfig using the same project
// defaults as DefaultHandshakeConfig. Caller fills ZID and WhatAmI.
func DefaultAcceptConfig() AcceptConfig {
	return AcceptConfig{
		BatchSize:      codec.MaxBatchSize,
		LeaseMillis:    10_000,
		Resolution:     wire.DefaultResolution,
		EnableQoS:      true,
		MaxMessageSize: 1 << 30,
	}
}

// DoHandshakeResponder runs the 4-message INIT/OPEN exchange as the
// responder.
//
// Flow:
//
//	B ← INIT SYN
//	B → INIT ACK (issue 16-byte random cookie, negotiate batch/resolution)
//	B ← OPEN SYN (verify cookie matches the one we issued)
//	B → OPEN ACK (our lease + initial_sn)
//
// Returns the negotiated parameters or an error on CLOSE / wire / version
// failure. CONNECTION_TO_SELF (peer ZID equals ours) is detected at INIT
// SYN and surfaced as the dedicated CLOSE reason — symmetric with
// DoHandshake.
func DoHandshakeResponder(link transport.Link, cfg AcceptConfig) (*HandshakeResult, error) {
	if !cfg.ZID.IsValid() {
		return nil, fmt.Errorf("handshake-responder: local ZID must be 1..16 bytes")
	}
	if cfg.BatchSize == 0 {
		return nil, fmt.Errorf("handshake-responder: BatchSize must be > 0")
	}
	if cfg.LeaseMillis == 0 {
		return nil, fmt.Errorf("handshake-responder: LeaseMillis must be > 0")
	}

	// --- INIT SYN ---
	initSyn, err := readInitSyn(link)
	if err != nil {
		return nil, err
	}
	if initSyn.Version != wire.ProtoVersion {
		_ = sendClose(link, CloseReasonInvalid)
		return nil, fmt.Errorf("handshake-responder: peer version %#x, want %#x", initSyn.Version, wire.ProtoVersion)
	}
	if initSyn.ZID.Equal(cfg.ZID) {
		_ = sendClose(link, CloseReasonConnectionToSelf)
		return nil, &CloseReasonErr{Reason: CloseReasonConnectionToSelf}
	}

	// Negotiate: take the minimum so neither side ends up with a value it
	// cannot handle.
	negBatch := cfg.BatchSize
	if initSyn.HasSizeInfo && initSyn.BatchSize < negBatch {
		negBatch = initSyn.BatchSize
	}
	negResolution := cfg.Resolution
	if initSyn.HasSizeInfo && initSyn.Resolution.Bits(wire.FieldFrameSN) < cfg.Resolution.Bits(wire.FieldFrameSN) {
		negResolution = initSyn.Resolution
	}

	// QoS is bidirectional: we echo the QoS Unit ext only when both sides
	// asked for it (matching Rust's open-accept exchange).
	qosAccepted := cfg.EnableQoS && hasQoSExt(initSyn.Extensions)

	// 16-byte cookie. The verification is in-process — DoHandshakeResponder
	// owns the responder side of one Link, so a per-call random value
	// suffices. Stateless cookies (HMAC-bound to peer addr) would be a
	// hardening follow-up but bring no correctness benefit for the in-
	// process case.
	cookie := make([]byte, 16)
	if _, err := rand.Read(cookie); err != nil {
		return nil, fmt.Errorf("handshake-responder: cookie rand: %w", err)
	}

	// --- INIT ACK ---
	initAck := &wire.Init{
		Ack:         true,
		Version:     wire.ProtoVersion,
		ZID:         cfg.ZID,
		WhatAmI:     cfg.WhatAmI,
		HasSizeInfo: true,
		Resolution:  negResolution,
		BatchSize:   negBatch,
		Cookie:      cookie,
	}
	if qosAccepted {
		initAck.Extensions = append(initAck.Extensions, codec.Extension{
			Header: codec.ExtHeader{ID: wire.ExtIDQoS, Encoding: codec.ExtEncUnit},
		})
	}
	if err := writeTransport(link, initAck); err != nil {
		return nil, fmt.Errorf("send INIT ACK: %w", err)
	}

	// --- OPEN SYN ---
	openSyn, err := readOpenSyn(link)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(openSyn.Cookie, cookie) {
		_ = sendClose(link, CloseReasonInvalid)
		return nil, fmt.Errorf("handshake-responder: OPEN SYN cookie mismatch")
	}

	peerLeaseMs := openSyn.Lease
	if openSyn.LeaseSeconds {
		peerLeaseMs *= 1000
	}
	peerInitialSN := openSyn.InitialSN

	fsnBits := negResolution.Bits(wire.FieldFrameSN)
	myInitialSN := ComputeInitialSN(cfg.ZID.Bytes, initSyn.ZID.Bytes, fsnBits)

	// --- OPEN ACK ---
	openAck := &wire.Open{
		Ack:          true,
		LeaseSeconds: false,
		Lease:        cfg.LeaseMillis,
		InitialSN:    myInitialSN,
	}
	if qosAccepted {
		openAck.Extensions = append(openAck.Extensions, codec.Extension{
			Header: codec.ExtHeader{ID: wire.ExtIDQoS, Encoding: codec.ExtEncUnit},
		})
	}
	if err := writeTransport(link, openAck); err != nil {
		return nil, fmt.Errorf("send OPEN ACK: %w", err)
	}

	return &HandshakeResult{
		PeerZID:              initSyn.ZID,
		PeerWhatAmI:          initSyn.WhatAmI,
		NegotiatedBatchSize:  negBatch,
		NegotiatedResolution: negResolution,
		MyLeaseMillis:        cfg.LeaseMillis,
		PeerLeaseMillis:      peerLeaseMs,
		MyInitialSN:          myInitialSN,
		PeerInitialSN:        peerInitialSN,
		QoSEnabled:           qosAccepted,
	}, nil
}

func readInitSyn(link transport.Link) (*wire.Init, error) {
	m, err := readHandshakeAck(link, "INIT SYN", wire.IDTransportInit, wire.DecodeInit)
	if err != nil {
		return nil, err
	}
	if m.Ack {
		return nil, fmt.Errorf("handshake-responder: expected INIT SYN (A=0), got INIT ACK")
	}
	return m, nil
}

func readOpenSyn(link transport.Link) (*wire.Open, error) {
	m, err := readHandshakeAck(link, "OPEN SYN", wire.IDTransportOpen, wire.DecodeOpen)
	if err != nil {
		return nil, err
	}
	if m.Ack {
		return nil, fmt.Errorf("handshake-responder: expected OPEN SYN (A=0), got OPEN ACK")
	}
	return m, nil
}
