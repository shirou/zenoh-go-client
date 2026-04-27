package session

import (
	"bytes"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/shirou/zenoh-go-client/internal/wire"
)

func TestDoHandshakeResponderRoundtrip(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	initiator := newPipeLink(a)
	responder := newPipeLink(b)

	initCfg := DefaultHandshakeConfig()
	initCfg.ZID = wire.ZenohID{Bytes: []byte{0x01, 0x02, 0x03, 0x04}}
	initCfg.WhatAmI = wire.WhatAmIPeer

	respCfg := DefaultAcceptConfig()
	respCfg.ZID = wire.ZenohID{Bytes: []byte{0xAA, 0xBB, 0xCC, 0xDD}}
	respCfg.WhatAmI = wire.WhatAmIPeer

	type pair struct {
		res *HandshakeResult
		err error
	}
	initCh := make(chan pair, 1)
	respCh := make(chan pair, 1)

	go func() {
		r, err := DoHandshake(initiator, initCfg)
		initCh <- pair{r, err}
	}()
	go func() {
		r, err := DoHandshakeResponder(responder, respCfg)
		respCh <- pair{r, err}
	}()

	deadline := time.After(3 * time.Second)
	var iRes, rRes pair
	select {
	case iRes = <-initCh:
	case <-deadline:
		t.Fatal("initiator stuck")
	}
	select {
	case rRes = <-respCh:
	case <-deadline:
		t.Fatal("responder stuck")
	}

	if iRes.err != nil {
		t.Fatalf("initiator err: %v", iRes.err)
	}
	if rRes.err != nil {
		t.Fatalf("responder err: %v", rRes.err)
	}

	// Each side identifies the other.
	if !iRes.res.PeerZID.Equal(respCfg.ZID) {
		t.Errorf("initiator peer zid = %v, want %v", iRes.res.PeerZID, respCfg.ZID)
	}
	if !rRes.res.PeerZID.Equal(initCfg.ZID) {
		t.Errorf("responder peer zid = %v, want %v", rRes.res.PeerZID, initCfg.ZID)
	}

	// initial_sn is symmetric: SHAKE128(my,peer) on each side.
	if iRes.res.MyInitialSN != rRes.res.PeerInitialSN {
		t.Errorf("initiator MyInitialSN=%d but responder PeerInitialSN=%d",
			iRes.res.MyInitialSN, rRes.res.PeerInitialSN)
	}
	if rRes.res.MyInitialSN != iRes.res.PeerInitialSN {
		t.Errorf("responder MyInitialSN=%d but initiator PeerInitialSN=%d",
			rRes.res.MyInitialSN, iRes.res.PeerInitialSN)
	}

	// Both negotiated the same QoS / batch / resolution.
	if iRes.res.QoSEnabled != rRes.res.QoSEnabled {
		t.Errorf("QoS mismatch: initiator=%v responder=%v", iRes.res.QoSEnabled, rRes.res.QoSEnabled)
	}
	if iRes.res.NegotiatedBatchSize != rRes.res.NegotiatedBatchSize {
		t.Errorf("BatchSize mismatch: %d vs %d", iRes.res.NegotiatedBatchSize, rRes.res.NegotiatedBatchSize)
	}
}

// TestDoHandshakeResponderConnectionToSelf verifies the responder side
// fires CONNECTION_TO_SELF when an INIT SYN arrives bearing the
// responder's own ZID.
func TestDoHandshakeResponderConnectionToSelf(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	initiator := newPipeLink(a)
	responder := newPipeLink(b)

	zid := wire.ZenohID{Bytes: bytes.Repeat([]byte{0x42}, 16)}

	initCfg := DefaultHandshakeConfig()
	initCfg.ZID = zid
	initCfg.WhatAmI = wire.WhatAmIPeer

	respCfg := DefaultAcceptConfig()
	respCfg.ZID = zid
	respCfg.WhatAmI = wire.WhatAmIPeer

	respCh := make(chan error, 1)
	go func() {
		_, err := DoHandshakeResponder(responder, respCfg)
		respCh <- err
	}()
	// Initiator will detect the same condition on its INIT ACK; ignore that.
	go func() { _, _ = DoHandshake(initiator, initCfg) }()

	select {
	case err := <-respCh:
		if !IsConnectionToSelf(err) {
			t.Errorf("responder err = %v, want CONNECTION_TO_SELF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("responder did not detect CONNECTION_TO_SELF")
	}
}

// TestDoHandshakeResponderCookieMismatch sanity-checks the cookie
// verification by injecting a tampered OPEN SYN. The responder should
// CLOSE rather than continue.
func TestDoHandshakeResponderCookieMismatch(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	initiator := newPipeLink(a)
	responder := newPipeLink(b)

	respCfg := DefaultAcceptConfig()
	respCfg.ZID = wire.ZenohID{Bytes: []byte{0x01}}
	respCfg.WhatAmI = wire.WhatAmIPeer

	respCh := make(chan error, 1)
	go func() {
		_, err := DoHandshakeResponder(responder, respCfg)
		respCh <- err
	}()

	// Initiator-shaped sender: send a normal INIT SYN, ignore the INIT
	// ACK's cookie, and send an OPEN SYN with garbage bytes in the cookie
	// slot.
	go func() {
		initSyn := &wire.Init{
			Ack:         false,
			Version:     wire.ProtoVersion,
			ZID:         wire.ZenohID{Bytes: []byte{0x99, 0x88}},
			WhatAmI:     wire.WhatAmIPeer,
			HasSizeInfo: true,
			Resolution:  wire.DefaultResolution,
			BatchSize:   65535,
		}
		_ = writeTransport(initiator, initSyn)
		// Drain the INIT ACK so the responder isn't blocked on its write.
		_, _ = readInitAck(initiator)
		// Send OPEN SYN with a deliberately-wrong cookie.
		openSyn := &wire.Open{
			Ack:       false,
			Lease:     1000,
			InitialSN: 0,
			Cookie:    []byte("not-the-real-cookie"),
		}
		_ = writeTransport(initiator, openSyn)
		// Drain whatever the responder writes (CLOSE) so it isn't
		// blocked on the sync pipe and can return.
		buf := make([]byte, 65535)
		_, _ = initiator.ReadBatch(buf)
	}()

	select {
	case err := <-respCh:
		if err == nil {
			t.Fatal("responder accepted bogus cookie")
		}
		if !errIsCookieMismatch(err) {
			t.Logf("responder err: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("responder did not return on bad cookie")
	}
}

func errIsCookieMismatch(err error) bool {
	if err == nil {
		return false
	}
	var ce *CloseReasonErr
	if errors.As(err, &ce) {
		return ce.Reason == CloseReasonInvalid
	}
	return true
}
