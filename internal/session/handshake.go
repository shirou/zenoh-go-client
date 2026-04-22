package session

import (
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/transport"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// HandshakeConfig captures the locally-chosen parameters the initiator
// proposes during INIT/OPEN.
type HandshakeConfig struct {
	ZID            wire.ZenohID
	WhatAmI        wire.WhatAmI
	BatchSize      uint16 // max batch size we are willing to receive
	LeaseMillis    uint64 // lease duration we propose (milliseconds)
	Resolution     wire.Resolution
	EnableQoS      bool // include QoS Unit ext in INIT SYN
	MaxMessageSize int  // cap for FRAGMENT reassembly
}

// DefaultHandshakeConfig builds a config with the project-wide defaults
// (batch_size=65535, lease=10s, resolution=0x0A, QoS enabled, 1 GiB
// reassembly cap). Caller must fill ZID and WhatAmI.
func DefaultHandshakeConfig() HandshakeConfig {
	return HandshakeConfig{
		BatchSize:      codec.MaxBatchSize,
		LeaseMillis:    10_000,
		Resolution:     wire.DefaultResolution,
		EnableQoS:      true,
		MaxMessageSize: 1 << 30,
	}
}

// HandshakeResult is the negotiated post-handshake state both peers agreed on.
type HandshakeResult struct {
	PeerZID              wire.ZenohID
	PeerWhatAmI          wire.WhatAmI
	NegotiatedBatchSize  uint16
	NegotiatedResolution wire.Resolution
	// MyLeaseMillis / PeerLeaseMillis: each side proposes its own lease.
	// Local keepalive cadence is derived from PeerLeaseMillis (we must send
	// within peer's lease); lease-watchdog uses MyLeaseMillis (peer must
	// send within ours).
	MyLeaseMillis   uint64
	PeerLeaseMillis uint64
	MyInitialSN     uint64
	PeerInitialSN   uint64
	QoSEnabled      bool
}

// DoHandshake runs the 4-message INIT/OPEN exchange as the initiator.
//
// Flow:
//
//	A → INIT SYN
//	A ← INIT ACK (version check, cookie, negotiate batch/resolution)
//	A → OPEN SYN (echo cookie, propose lease + initial_sn)
//	A ← OPEN ACK (peer's lease + initial_sn)
//
// Returns the negotiated parameters or an error on CLOSE / wire / version
// failure. CONNECTION_TO_SELF (peer ZID equals ours) is detected at INIT ACK
// and surfaced as the dedicated CLOSE reason.
func DoHandshake(link transport.Link, cfg HandshakeConfig) (*HandshakeResult, error) {
	if !cfg.ZID.IsValid() {
		return nil, fmt.Errorf("handshake: local ZID must be 1..16 bytes")
	}
	if cfg.BatchSize == 0 {
		return nil, fmt.Errorf("handshake: BatchSize must be > 0")
	}
	if cfg.LeaseMillis == 0 {
		return nil, fmt.Errorf("handshake: LeaseMillis must be > 0")
	}

	// --- INIT SYN ---
	initSyn := &wire.Init{
		Ack:         false,
		Version:     wire.ProtoVersion,
		ZID:         cfg.ZID,
		WhatAmI:     cfg.WhatAmI,
		HasSizeInfo: true,
		Resolution:  cfg.Resolution,
		BatchSize:   cfg.BatchSize,
	}
	if cfg.EnableQoS {
		initSyn.Extensions = append(initSyn.Extensions, codec.Extension{
			Header: codec.ExtHeader{ID: wire.ExtIDQoS, Encoding: codec.ExtEncUnit},
		})
	}
	if err := writeTransport(link, initSyn); err != nil {
		return nil, fmt.Errorf("send INIT SYN: %w", err)
	}

	// --- INIT ACK ---
	initAck, err := readInitAck(link)
	if err != nil {
		return nil, err
	}
	if initAck.Version != wire.ProtoVersion {
		return nil, fmt.Errorf("handshake: peer version %#x, want %#x", initAck.Version, wire.ProtoVersion)
	}
	if initAck.ZID.Equal(cfg.ZID) {
		_ = sendClose(link, CloseReasonConnectionToSelf)
		return nil, &CloseReasonErr{Reason: CloseReasonConnectionToSelf}
	}

	// Negotiate: batch_size = min(proposed, peer's), resolution may be
	// lowered by responder (we accept whatever they return).
	negBatch := cfg.BatchSize
	if initAck.HasSizeInfo && initAck.BatchSize < negBatch {
		negBatch = initAck.BatchSize
	}
	negResolution := cfg.Resolution
	if initAck.HasSizeInfo {
		negResolution = initAck.Resolution
	}

	// Compute my initial SN from (my ZID, peer ZID, fsn bits).
	fsnBits := negResolution.Bits(wire.FieldFrameSN)
	myInitialSN := ComputeInitialSN(cfg.ZID.Bytes, initAck.ZID.Bytes, fsnBits)

	// --- OPEN SYN ---
	openSyn := &wire.Open{
		Ack:          false,
		LeaseSeconds: false,
		Lease:        cfg.LeaseMillis,
		InitialSN:    myInitialSN,
		Cookie:       initAck.Cookie,
	}
	// Echo QoS in OPEN SYN if both sides agreed at INIT time
	// (spec open-accept.adoc §OpenSyn Extensions).
	if cfg.EnableQoS && hasQoSExt(initAck.Extensions) {
		openSyn.Extensions = append(openSyn.Extensions, codec.Extension{
			Header: codec.ExtHeader{ID: wire.ExtIDQoS, Encoding: codec.ExtEncUnit},
		})
	}
	if err := writeTransport(link, openSyn); err != nil {
		return nil, fmt.Errorf("send OPEN SYN: %w", err)
	}

	// --- OPEN ACK ---
	openAck, err := readOpenAck(link)
	if err != nil {
		return nil, err
	}
	peerLeaseMs := openAck.Lease
	if openAck.LeaseSeconds {
		peerLeaseMs *= 1000
	}
	peerInitialSN := openAck.InitialSN

	// QoS is negotiated at INIT: if the peer echoed the QoS ext in INIT ACK,
	// transport lanes are enabled. OPEN ACK may or may not echo it; we
	// don't require it there (matching zenoh-rust).
	qosAccepted := cfg.EnableQoS && hasQoSExt(initAck.Extensions)

	return &HandshakeResult{
		PeerZID:              initAck.ZID,
		PeerWhatAmI:          initAck.WhatAmI,
		NegotiatedBatchSize:  negBatch,
		NegotiatedResolution: negResolution,
		MyLeaseMillis:        cfg.LeaseMillis,
		PeerLeaseMillis:      peerLeaseMs,
		MyInitialSN:          myInitialSN,
		PeerInitialSN:        peerInitialSN,
		QoSEnabled:           qosAccepted,
	}, nil
}

// hasQoSExt reports whether the chain contains the QoS Unit (ID 0x01) ext.
func hasQoSExt(exts []codec.Extension) bool {
	for _, e := range exts {
		if e.Header.ID == wire.ExtIDQoS && e.Header.Encoding == codec.ExtEncUnit {
			return true
		}
	}
	return false
}

// --- helpers ---

func writeTransport(link transport.Link, msg codec.Encoder) error {
	w := codec.NewWriter(64)
	if err := msg.EncodeTo(w); err != nil {
		return err
	}
	return link.WriteBatch(w.Bytes())
}

// readHandshakeAck reads one transport message, translates a CLOSE into a
// CloseReasonErr, verifies the header ID matches wantID, and decodes via
// the supplied decoder. The returned message has its Ack flag verified by
// the caller (different messages store Ack in different fields, so we
// can't check it here).
func readHandshakeAck[T any](
	link transport.Link,
	label string,
	wantID byte,
	decode func(*codec.Reader, codec.Header) (T, error),
) (T, error) {
	var zero T
	buf := make([]byte, codec.MaxBatchSize)
	n, err := link.ReadBatch(buf)
	if err != nil {
		return zero, fmt.Errorf("read %s: %w", label, err)
	}
	r := codec.NewReader(buf[:n])
	h, err := r.DecodeHeader()
	if err != nil {
		return zero, err
	}
	if h.ID == wire.IDTransportClose {
		return zero, decodeCloseAsError(r, h)
	}
	if h.ID != wantID {
		return zero, fmt.Errorf("handshake: expected %s (id=%#x), got id=%#x", label, wantID, h.ID)
	}
	m, err := decode(r, h)
	if err != nil {
		return zero, fmt.Errorf("decode %s: %w", label, err)
	}
	return m, nil
}

func readInitAck(link transport.Link) (*wire.Init, error) {
	m, err := readHandshakeAck(link, "INIT ACK", wire.IDTransportInit, wire.DecodeInit)
	if err != nil {
		return nil, err
	}
	if !m.Ack {
		return nil, fmt.Errorf("handshake: expected INIT ACK (A=1), got INIT SYN")
	}
	return m, nil
}

func readOpenAck(link transport.Link) (*wire.Open, error) {
	m, err := readHandshakeAck(link, "OPEN ACK", wire.IDTransportOpen, wire.DecodeOpen)
	if err != nil {
		return nil, err
	}
	if !m.Ack {
		return nil, fmt.Errorf("handshake: expected OPEN ACK (A=1), got OPEN SYN")
	}
	return m, nil
}

func decodeCloseAsError(r *codec.Reader, h codec.Header) error {
	cl, err := wire.DecodeClose(r, h)
	if err != nil {
		return fmt.Errorf("decode peer CLOSE: %w", err)
	}
	return &CloseReasonErr{Reason: cl.Reason}
}

// sendClose emits a session-scoped CLOSE with the given reason. Errors are
// suppressed because we're already in a failure path.
func sendClose(link transport.Link, reason uint8) error {
	return writeTransport(link, &wire.Close{Session: true, Reason: reason})
}

// CloseReasonErr is the internal-transport equivalent of zenoh.ZError with
// a CLOSE reason code. Session-layer code wraps this into zenoh.ZError when
// surfacing it to the public API.
type CloseReasonErr struct {
	Reason uint8
}

func (e *CloseReasonErr) Error() string {
	return fmt.Sprintf("peer closed session (reason 0x%02x)", e.Reason)
}

// CLOSE reason codes. Mirrors zenoh.CloseReason* so internal code can refer
// to them without depending on the public package.
const (
	CloseReasonGeneric          uint8 = 0x00
	CloseReasonUnsupported      uint8 = 0x01
	CloseReasonInvalid          uint8 = 0x02
	CloseReasonMaxSessions      uint8 = 0x03
	CloseReasonMaxLinks         uint8 = 0x04
	CloseReasonExpired          uint8 = 0x05
	CloseReasonUnresponsive     uint8 = 0x06
	CloseReasonConnectionToSelf uint8 = 0x07
)

// GenerateZID returns a 16-byte random ZenohID backed by crypto/rand.
//
// crypto/rand.Read never returns an error on modern Go (stdlib panics
// internally instead), so we don't propagate one.
func GenerateZID() wire.ZenohID {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return wire.ZenohID{Bytes: b[:]}
}

// IsConnectionToSelf reports whether err is a CLOSE with CONNECTION_TO_SELF.
func IsConnectionToSelf(err error) bool {
	var c *CloseReasonErr
	return errors.As(err, &c) && c.Reason == CloseReasonConnectionToSelf
}
