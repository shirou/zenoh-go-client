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
	t          *testing.T
	ln         net.Listener
	conn       net.Conn
	declared   chan declaredSubscriber
	declaredQ  chan declaredQueryable
	declaredT  chan declaredToken
	undeclared chan undeclaredEntity
	requests   chan inboundRequest
	responses  chan inboundResponse
	injectCh   chan []byte // framed network-msg bytes to send (wrapped in FRAME)
	closed     chan struct{}
}

type declaredSubscriber struct {
	id  uint32
	key string
}

type declaredQueryable struct {
	id  uint32
	key string
}

type declaredToken struct {
	id  uint32
	key string
}

type undeclaredEntity struct {
	kind byte // wire.IDUndeclareSubscriber / Queryable / Token
	id   uint32
	key  string
}

type inboundRequest struct {
	requestID  uint32
	key        string
	parameters string
	extensions []codec.Extension
}

type inboundResponse struct {
	requestID uint32
	key       string
	payload   []byte
	isErr     bool
}

func newMockRouter(t *testing.T) *mockRouter {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := &mockRouter{
		t:          t,
		ln:         ln,
		declared:   make(chan declaredSubscriber, 8),
		declaredQ:  make(chan declaredQueryable, 8),
		declaredT:  make(chan declaredToken, 8),
		undeclared: make(chan undeclaredEntity, 8),
		requests:   make(chan inboundRequest, 8),
		responses:  make(chan inboundResponse, 8),
		injectCh:   make(chan []byte, 8),
		closed:     make(chan struct{}),
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
		switch h.ID {
		case wire.IDNetworkDeclare:
			d, err := wire.DecodeDeclare(r, h)
			if err != nil {
				return
			}
			switch body := d.Body.(type) {
			case *wire.DeclareEntity:
				switch body.Kind {
				case wire.IDDeclareSubscriber:
					m.declared <- declaredSubscriber{id: body.EntityID, key: body.KeyExpr.Suffix}
				case wire.IDDeclareQueryable:
					m.declaredQ <- declaredQueryable{id: body.EntityID, key: body.KeyExpr.Suffix}
				case wire.IDDeclareToken:
					m.declaredT <- declaredToken{id: body.EntityID, key: body.KeyExpr.Suffix}
				}
			case *wire.UndeclareEntity:
				m.undeclared <- undeclaredEntity{kind: body.Kind, id: body.EntityID, key: body.WireExpr.Suffix}
			}
		case wire.IDNetworkRequest:
			req, err := wire.DecodeRequest(r, h)
			if err != nil {
				return
			}
			m.requests <- inboundRequest{
				requestID:  req.RequestID,
				key:        req.KeyExpr.Suffix,
				parameters: paramsOf(req.Body),
				extensions: req.Extensions,
			}
		case wire.IDNetworkResponse:
			res, err := wire.DecodeResponse(r, h)
			if err != nil {
				return
			}
			ir := inboundResponse{requestID: res.RequestID, key: res.KeyExpr.Suffix}
			switch body := res.Body.(type) {
			case *wire.ReplyBody:
				if put, ok := body.Inner.(*wire.PutBody); ok {
					ir.payload = put.Payload
				}
			case *wire.ErrBody:
				ir.isErr = true
				ir.payload = body.Payload
			}
			m.responses <- ir
		case wire.IDNetworkResponseFinal:
			_, _ = wire.DecodeResponseFinal(r, h)
		case wire.IDNetworkPush:
			_, _ = wire.DecodePush(r, h)
			return // PUSH consumes rest of body
		default:
			return
		}
	}
}

func paramsOf(q *wire.QueryBody) string {
	if q == nil {
		return ""
	}
	return q.Parameters
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
	m.inject(t, &wire.Push{
		KeyExpr: wire.WireExpr{Scope: 0, Suffix: keyExpr},
		Body:    &wire.PutBody{Payload: []byte(payload)},
	})
}

// injectRequest simulates a REQUEST arriving from a remote querier.
func (m *mockRouter) injectRequest(t *testing.T, requestID uint32, keyExpr, params string) {
	t.Helper()
	body := &wire.QueryBody{}
	if params != "" {
		body.Parameters = params
	}
	m.inject(t, &wire.Request{
		RequestID: requestID,
		KeyExpr:   wire.WireExpr{Scope: 0, Suffix: keyExpr},
		Body:      body,
	})
}

// injectReply simulates a RESPONSE with a PUT body.
func (m *mockRouter) injectReply(t *testing.T, requestID uint32, keyExpr, payload string) {
	t.Helper()
	m.inject(t, &wire.Response{
		RequestID: requestID,
		KeyExpr:   wire.WireExpr{Scope: 0, Suffix: keyExpr},
		Body:      &wire.ReplyBody{Inner: &wire.PutBody{Payload: []byte(payload)}},
	})
}

// injectResponseFinal signals end-of-replies for a request_id.
func (m *mockRouter) injectResponseFinal(t *testing.T, requestID uint32) {
	t.Helper()
	m.inject(t, &wire.ResponseFinal{RequestID: requestID})
}

func (m *mockRouter) inject(t *testing.T, msg codec.Encoder) {
	t.Helper()
	w := codec.NewWriter(64)
	if err := msg.EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	bs := make([]byte, w.Len())
	copy(bs, w.Bytes())
	select {
	case m.injectCh <- bs:
	case <-time.After(time.Second):
		t.Fatal("inject: router busy")
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

// TestSessionGetReceivesReplies: client sends Get → router captures
// REQUEST → router injects two REPLYs + RESPONSE_FINAL → client receives
// all replies and channel closes.
func TestSessionGetReceivesReplies(t *testing.T) {
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

	ke, _ := NewKeyExpr("demo/get")
	replyCh, err := sess.Get(ke, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Wait for the router to see the REQUEST.
	var req inboundRequest
	select {
	case req = <-router.requests:
	case <-time.After(2 * time.Second):
		t.Fatal("router did not see REQUEST within 2s")
	}
	if req.key != "demo/get" {
		t.Errorf("REQUEST key = %q, want demo/get", req.key)
	}

	// Inject two replies then RESPONSE_FINAL.
	router.injectReply(t, req.requestID, "demo/get", "first")
	router.injectReply(t, req.requestID, "demo/get", "second")
	router.injectResponseFinal(t, req.requestID)

	got := []string{}
	for r := range replyCh {
		if !r.IsOk() {
			t.Errorf("unexpected error reply")
			continue
		}
		s, _ := r.Sample()
		got = append(got, s.Payload().String())
	}
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Errorf("got replies %v, want [first second]", got)
	}
}

// TestDeclareQueryableRespondsToRequest: router sends REQUEST → our
// queryable handler replies → router observes RESPONSE + RESPONSE_FINAL.
func TestDeclareQueryableRespondsToRequest(t *testing.T) {
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
	qbl, err := sess.DeclareQueryable(ke, func(q *Query) {
		replyKE, _ := NewKeyExpr(q.KeyExpr().String())
		_ = q.Reply(replyKE, NewZBytesFromString("pong"), nil)
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer qbl.Drop()

	// Wait for D_QUERYABLE.
	select {
	case d := <-router.declaredQ:
		if d.key != "demo/**" {
			t.Errorf("router saw D_QUERYABLE key=%q", d.key)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("router did not observe D_QUERYABLE within 2s")
	}

	// Inject a REQUEST addressed at a key intersecting demo/**.
	router.injectRequest(t, 42, "demo/ping", "")

	// Expect one RESPONSE (the reply) from the client.
	select {
	case res := <-router.responses:
		if res.requestID != 42 {
			t.Errorf("RESPONSE request_id = %d, want 42", res.requestID)
		}
		if string(res.payload) != "pong" {
			t.Errorf("RESPONSE payload = %q, want pong", res.payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("router did not see RESPONSE within 2s")
	}
}

func TestSessionGetEmitsRequestExtensions(t *testing.T) {
	router := newMockRouter(t)
	defer router.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sess, err := Open(ctx, NewConfig().WithEndpoint("tcp/"+router.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	ke, _ := NewKeyExpr("demo/test/**")
	replies, err := sess.Get(ke, &GetOptions{
		Target:    QueryTargetAll,
		HasTarget: true,
		Budget:    32,
		Timeout:   2500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	var reqID uint32
	select {
	case req := <-router.requests:
		reqID = req.requestID
		target := codec.FindExt(req.extensions, wire.ReqExtIDQueryTarget)
		if target == nil || wire.QueryTarget(target.Z64) != wire.QueryTargetAll {
			t.Errorf("QueryTarget ext = %+v, want QueryTargetAll", target)
		}
		budget := codec.FindExt(req.extensions, wire.ReqExtIDBudget)
		if budget == nil || uint32(budget.Z64) != 32 {
			t.Errorf("Budget ext = %+v, want 32", budget)
		}
		timeout := codec.FindExt(req.extensions, wire.ReqExtIDTimeout)
		if timeout == nil || timeout.Z64 != 2500 {
			t.Errorf("Timeout ext = %+v, want 2500 (ms)", timeout)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("router did not observe REQUEST within 2s")
	}

	// Terminate the get cleanly: inject RESPONSE_FINAL so the translator
	// goroutine spawned by Session.Get exits via its normal range-over-
	// channel path and the reply channel closes on its own.
	router.injectResponseFinal(t, reqID)
	select {
	case _, ok := <-replies:
		if ok {
			t.Error("expected closed replies channel, got a value")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("replies channel not closed after RESPONSE_FINAL")
	}
}

func TestGetWithContextCancelClosesReplies(t *testing.T) {
	router := newMockRouter(t)
	defer router.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sess, err := Open(ctx, NewConfig().WithEndpoint("tcp/"+router.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	getCtx, cancelGet := context.WithCancel(context.Background())
	ke, _ := NewKeyExpr("demo/test/**")
	replies, err := sess.GetWithContext(getCtx, ke, nil)
	if err != nil {
		t.Fatalf("GetWithContext: %v", err)
	}

	// Wait for the REQUEST to hit the router (but don't reply).
	select {
	case <-router.requests:
	case <-time.After(2 * time.Second):
		t.Fatal("router did not observe REQUEST")
	}

	cancelGet()
	select {
	case _, ok := <-replies:
		if ok {
			t.Error("expected closed replies channel after ctx cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("replies channel not closed after ctx cancel")
	}
}

func TestGetTimeoutClosesReplies(t *testing.T) {
	router := newMockRouter(t)
	defer router.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sess, err := Open(ctx, NewConfig().WithEndpoint("tcp/"+router.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	ke, _ := NewKeyExpr("demo/test/**")
	replies, err := sess.Get(ke, &GetOptions{Timeout: 150 * time.Millisecond})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	select {
	case <-router.requests:
	case <-time.After(2 * time.Second):
		t.Fatal("router did not observe REQUEST")
	}

	select {
	case _, ok := <-replies:
		if ok {
			t.Error("expected closed replies channel after timeout")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("replies channel not closed after Timeout expired")
	}
}

func TestDeclareLivelinessTokenEmitsDAndU(t *testing.T) {
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

	ke, _ := NewKeyExpr("group1/alpha")
	tok, err := sess.Liveliness().DeclareToken(ke, nil)
	if err != nil {
		t.Fatalf("DeclareToken: %v", err)
	}

	select {
	case d := <-router.declaredT:
		if d.key != "group1/alpha" {
			t.Errorf("D_TOKEN key=%q, want group1/alpha", d.key)
		}
		if d.id == 0 {
			t.Errorf("D_TOKEN id=0; expected a freshly allocated id")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("router did not observe D_TOKEN within 2s")
	}

	tok.Drop()
	select {
	case u := <-router.undeclared:
		if u.kind != wire.IDUndeclareToken {
			t.Errorf("undeclared kind=%#x, want IDUndeclareToken=%#x", u.kind, wire.IDUndeclareToken)
		}
		if u.key != "group1/alpha" {
			t.Errorf("U_TOKEN key=%q, want group1/alpha", u.key)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("router did not observe U_TOKEN within 2s")
	}

	// Idempotency: a second Drop must not emit another U_TOKEN. 500ms is
	// well beyond the sub-millisecond enqueue path but short enough to keep
	// the test fast; the atomic guard inside Drop means the window is
	// really "did any goroutine mis-handle the CAS," not wall-clock.
	tok.Drop()
	select {
	case u := <-router.undeclared:
		t.Errorf("unexpected second U_* after Drop: %+v", u)
	case <-time.After(500 * time.Millisecond):
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
