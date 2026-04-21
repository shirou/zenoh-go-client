package zenoh

import (
	"context"
	"crypto/rand"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/session"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// mockRouter accepts one TCP connection, completes the INIT/OPEN handshake
// as a responder, then exposes a loop that reads DECLARE messages, records
// them, and can inject PUSHes back to the client.
type mockRouter struct {
	t        *testing.T
	ln       net.Listener
	conn     net.Conn
	declared chan declaredSubscriber
	injectCh chan []byte // framed network-msg bytes to send (wrapped in FRAME)
	closed   chan struct{}
}

type declaredSubscriber struct {
	id  uint32
	key string
}

func newMockRouter(t *testing.T) *mockRouter {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := &mockRouter{
		t:        t,
		ln:       ln,
		declared: make(chan declaredSubscriber, 8),
		injectCh: make(chan []byte, 8),
		closed:   make(chan struct{}),
	}
	go m.serve()
	return m
}

func (m *mockRouter) Addr() string { return m.ln.Addr().String() }

func (m *mockRouter) Close() {
	select {
	case <-m.closed:
		return
	default:
	}
	if m.conn != nil {
		m.conn.Close()
	}
	m.ln.Close()
}

func (m *mockRouter) serve() {
	defer close(m.closed)
	conn, err := m.ln.Accept()
	if err != nil {
		return
	}
	m.conn = conn
	defer conn.Close()

	// --- Handshake as responder ---
	if err := m.handshake(conn); err != nil {
		m.t.Logf("mockRouter handshake: %v", err)
		return
	}

	// --- Post-handshake loop ---
	buf := make([]byte, codec.MaxBatchSize)
	for {
		select {
		case payload := <-m.injectCh:
			if err := m.sendFrame(conn, payload); err != nil {
				return
			}
		default:
		}

		_ = conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		n, err := codec.ReadStreamBatch(conn, buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		m.processBatch(conn, buf[:n])
	}
}

func (m *mockRouter) handshake(conn net.Conn) error {
	buf := make([]byte, codec.MaxBatchSize)

	// INIT SYN
	n, err := codec.ReadStreamBatch(conn, buf)
	if err != nil {
		return err
	}
	r := codec.NewReader(buf[:n])
	h, _ := r.DecodeHeader()
	initSyn, err := wire.DecodeInit(r, h)
	if err != nil {
		return err
	}

	// INIT ACK
	cookie := make([]byte, 8)
	_, _ = rand.Read(cookie)
	initAck := &wire.Init{
		Ack:         true,
		Version:     wire.ProtoVersion,
		ZID:         wire.ZenohID{Bytes: []byte{0xAB, 0xCD}},
		WhatAmI:     wire.WhatAmIRouter,
		HasSizeInfo: true,
		Resolution:  initSyn.Resolution,
		BatchSize:   initSyn.BatchSize,
		Cookie:      cookie,
	}
	// Echo QoS ext so the client enables QoS lanes.
	for _, e := range initSyn.Extensions {
		if e.Header.ID == wire.ExtIDQoS && e.Header.Encoding == codec.ExtEncUnit {
			initAck.Extensions = append(initAck.Extensions, e)
		}
	}
	w := codec.NewWriter(64)
	if err := initAck.EncodeTo(w); err != nil {
		return err
	}
	if err := codec.EncodeStreamBatch(conn, w.Bytes()); err != nil {
		return err
	}

	// OPEN SYN
	n, err = codec.ReadStreamBatch(conn, buf)
	if err != nil {
		return err
	}
	r = codec.NewReader(buf[:n])
	h, _ = r.DecodeHeader()
	openSyn, err := wire.DecodeOpen(r, h)
	if err != nil {
		return err
	}

	// OPEN ACK
	ourInitialSN := session.ComputeInitialSN(
		initAck.ZID.Bytes, initSyn.ZID.Bytes, initAck.Resolution.Bits(wire.FieldFrameSN))
	openAck := &wire.Open{
		Ack:       true,
		Lease:     openSyn.Lease,
		InitialSN: ourInitialSN,
	}
	w2 := codec.NewWriter(32)
	if err := openAck.EncodeTo(w2); err != nil {
		return err
	}
	return codec.EncodeStreamBatch(conn, w2.Bytes())
}

// processBatch walks transport messages in one batch and extracts DECLAREs
// carrying D_SUBSCRIBER.
func (m *mockRouter) processBatch(conn net.Conn, batch []byte) {
	r := codec.NewReader(batch)
	for r.Len() > 0 {
		h, err := r.DecodeHeader()
		if err != nil {
			return
		}
		switch h.ID {
		case wire.IDTransportFrame:
			frame, err := wire.DecodeFrame(r, h)
			if err != nil {
				return
			}
			m.processFrameBody(frame.Body)
			return
		case wire.IDTransportKeepAlive:
			_, _ = wire.DecodeKeepAlive(r, h)
		case wire.IDTransportClose:
			return
		default:
			return
		}
	}
}

func (m *mockRouter) processFrameBody(body []byte) {
	r := codec.NewReader(body)
	for r.Len() > 0 {
		h, err := r.DecodeHeader()
		if err != nil {
			return
		}
		if h.ID != wire.IDNetworkDeclare {
			// Consume and skip other messages (keep test simple).
			return
		}
		d, err := wire.DecodeDeclare(r, h)
		if err != nil {
			return
		}
		if sub, ok := d.Body.(*wire.DeclareEntity); ok && sub.Kind == wire.IDDeclareSubscriber {
			m.declared <- declaredSubscriber{id: sub.EntityID, key: sub.KeyExpr.Suffix}
		}
	}
}

// sendFrame wraps a pre-encoded NetworkMessage in a FRAME + stream batch
// and writes it. SeqNum starts at 0 for simplicity (the client's reader
// doesn't verify monotonicity in MVP).
func (m *mockRouter) sendFrame(conn net.Conn, networkMsg []byte) error {
	w := codec.NewWriter(len(networkMsg) + 8)
	w.EncodeHeader(codec.Header{
		ID: wire.IDTransportFrame,
		F1: true, // reliable
	})
	w.EncodeZ64(0)
	w.AppendBytes(networkMsg)
	return codec.EncodeStreamBatch(conn, w.Bytes())
}

// injectPush makes the mock router deliver a PUSH/PUT to the client.
func (m *mockRouter) injectPush(t *testing.T, keyExpr, payload string) {
	t.Helper()
	push := &wire.Push{
		KeyExpr: wire.WireExpr{Scope: 0, Suffix: keyExpr},
		Body:    &wire.PutBody{Payload: []byte(payload)},
	}
	w := codec.NewWriter(64)
	if err := push.EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	bs := make([]byte, w.Len())
	copy(bs, w.Bytes())
	select {
	case m.injectCh <- bs:
	case <-time.After(time.Second):
		t.Fatal("injectPush: router busy")
	}
}

// ----------------------------------------------------------------------------
// Tests
// ----------------------------------------------------------------------------

func TestOpenSessionAndPut(t *testing.T) {
	router := newMockRouter(t)
	defer router.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	cfg := NewConfig().WithEndpoint("tcp/" + router.Addr())
	sess, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	ke, _ := NewKeyExpr("demo/test")
	if err := sess.Put(ke, NewZBytesFromString("hello"), nil); err != nil {
		t.Fatal(err)
	}
}

func TestDeclareSubscriberReceivesInjectedPush(t *testing.T) {
	router := newMockRouter(t)
	defer router.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cfg := NewConfig().WithEndpoint("tcp/" + router.Addr())
	sess, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	ke, _ := NewKeyExpr("demo/**")
	fifo := NewFifoChannel[Sample](16)
	sub, err := sess.DeclareSubscriber(ke, fifo)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Drop()

	// Wait for the router to see our D_SUBSCRIBER.
	select {
	case d := <-router.declared:
		if d.key != "demo/**" {
			t.Errorf("router saw D_SUBSCRIBER key=%q, want demo/**", d.key)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("router did not observe D_SUBSCRIBER within 2s")
	}

	// Inject a PUSH that matches the subscriber's key.
	router.injectPush(t, "demo/example/hello", "world")

	ch := sub.Handler().(<-chan Sample)
	select {
	case sample := <-ch:
		if sample.KeyExpr().String() != "demo/example/hello" {
			t.Errorf("sample key = %q", sample.KeyExpr().String())
		}
		if sample.Payload().String() != "world" {
			t.Errorf("sample payload = %q, want world", sample.Payload().String())
		}
		if sample.Kind() != SampleKindPut {
			t.Errorf("kind = %v, want Put", sample.Kind())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sample not delivered within 2s")
	}
}

func TestOpenFailsOnClosedEndpoint(t *testing.T) {
	// Pick an ephemeral port, close immediately → dial will refuse.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cfg := NewConfig().WithEndpoint("tcp/" + addr)
	_, err = Open(ctx, cfg)
	if err == nil {
		t.Fatal("expected dial failure")
	}
	if !strings.Contains(err.Error(), "dial") {
		t.Errorf("expected dial error mention, got %v", err)
	}
}
