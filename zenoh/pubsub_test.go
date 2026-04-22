package zenoh

import (
	"context"
	"crypto/rand"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/session"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// mockRouter listens on a TCP port, completes the INIT/OPEN handshake as
// a responder, then exposes a loop that reads DECLARE messages, records
// them, and can inject PUSHes back to the client. The accept loop restarts
// after a connection is torn down so reconnect tests can observe multiple
// handshakes in sequence.
type mockRouter struct {
	t          *testing.T
	ln         net.Listener
	connMu     sync.Mutex // guards conn swap between serve and tests
	conn       net.Conn
	declared   chan declaredSubscriber
	declaredQ  chan declaredQueryable
	declaredT  chan declaredToken
	undeclared chan undeclaredEntity
	interests  chan observedInterest
	requests   chan inboundRequest
	responses  chan inboundResponse
	injectCh   chan []byte // framed network-msg bytes to send (wrapped in FRAME)
	closed     chan struct{}
}

// setConn swaps in a freshly-accepted connection.
func (m *mockRouter) setConn(c net.Conn) {
	m.connMu.Lock()
	m.conn = c
	m.connMu.Unlock()
}

// currentConn returns the most recently accepted connection, or nil.
func (m *mockRouter) currentConn() net.Conn {
	m.connMu.Lock()
	defer m.connMu.Unlock()
	return m.conn
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

type observedInterest struct {
	id     uint32
	final  bool
	mode   wire.InterestMode
	filter wire.InterestFilter
	key    string // empty when unrestricted
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
		interests:  make(chan observedInterest, 8),
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
	if c := m.currentConn(); c != nil {
		c.Close()
	}
	m.ln.Close()
}

func (m *mockRouter) serve() {
	defer close(m.closed)
	for {
		conn, err := m.ln.Accept()
		if err != nil {
			return
		}
		m.setConn(conn)

		// --- Handshake as responder ---
		if err := m.handshake(conn); err != nil {
			m.t.Logf("mockRouter handshake: %v", err)
			conn.Close()
			continue
		}

		m.serveConn(conn)
		conn.Close()
	}
}

// serveConn drives post-handshake traffic for one accepted connection.
// Returns when the peer hangs up so the outer loop can accept a reconnect.
func (m *mockRouter) serveConn(conn net.Conn) {
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
		case wire.IDNetworkInterest:
			msg, err := wire.DecodeInterest(r, h)
			if err != nil {
				return
			}
			obs := observedInterest{
				id:     msg.InterestID,
				final:  msg.Mode == wire.InterestModeFinal,
				mode:   msg.Mode,
				filter: msg.Filter,
			}
			if msg.KeyExpr != nil {
				obs.key = msg.KeyExpr.Suffix
			}
			m.interests <- obs
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

// injectTokenDeclare simulates a router-side D_TOKEN. If interestID > 0
// the DECLARE carries the interest_id so the client can route it to a
// matching in-flight liveliness Get.
func (m *mockRouter) injectTokenDeclare(t *testing.T, interestID, tokenID uint32, keyExpr string) {
	t.Helper()
	m.inject(t, &wire.Declare{
		InterestID:    interestID,
		HasInterestID: interestID != 0,
		Body: &wire.DeclareEntity{
			Kind:     wire.IDDeclareToken,
			EntityID: tokenID,
			KeyExpr:  wire.WireExpr{Scope: 0, Suffix: keyExpr},
		},
	})
}

// injectTokenUndeclare simulates a router-side U_TOKEN.
func (m *mockRouter) injectTokenUndeclare(t *testing.T, tokenID uint32, keyExpr string) {
	t.Helper()
	m.inject(t, &wire.Declare{
		Body: &wire.UndeclareEntity{
			Kind:     wire.IDUndeclareToken,
			EntityID: tokenID,
			WireExpr: wire.WireExpr{Scope: 0, Suffix: keyExpr},
		},
	})
}

// injectDeclareFinal signals end-of-snapshot for a liveliness Get.
func (m *mockRouter) injectDeclareFinal(t *testing.T, interestID uint32) {
	t.Helper()
	m.inject(t, &wire.Declare{
		InterestID:    interestID,
		HasInterestID: true,
		Body:          &wire.DeclareFinal{},
	})
}

// injectReply simulates a RESPONSE with a PUT body. Equivalent to
// injectReplyWithTS(..., 0).
func (m *mockRouter) injectReply(t *testing.T, requestID uint32, keyExpr, payload string) {
	t.Helper()
	m.injectReplyWithTS(t, requestID, keyExpr, payload, 0)
}

// injectReplyWithTS simulates a RESPONSE with a PUT body. ntp64 > 0
// attaches a Timestamp with that value; 0 omits the timestamp entirely
// — consolidation tests that rely on ordering use non-zero values.
func (m *mockRouter) injectReplyWithTS(t *testing.T, requestID uint32, keyExpr, payload string, ntp64 uint64) {
	t.Helper()
	put := &wire.PutBody{Payload: []byte(payload)}
	if ntp64 > 0 {
		put.Timestamp = &wire.Timestamp{NTP64: ntp64, ZID: wire.ZenohID{Bytes: []byte{1, 2}}}
	}
	m.inject(t, &wire.Response{
		RequestID: requestID,
		KeyExpr:   wire.WireExpr{Scope: 0, Suffix: keyExpr},
		Body:      &wire.ReplyBody{Inner: put},
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
	// Consolidation=None so both replies reach the caller verbatim
	// (default is Auto=Latest, which dedupes same-key replies).
	replyCh, err := sess.Get(ke, &GetOptions{
		Consolidation:    ConsolidationNone,
		HasConsolidation: true,
	})
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

// TestReconnectReplaysEntities: drop the current link after declaring a
// subscriber + queryable + liveliness token. The mockRouter accepts a new
// connection; the reconnect orchestrator must re-send D_SUBSCRIBER,
// D_QUERYABLE, and D_TOKEN with the same IDs.
func TestReconnectReplaysEntities(t *testing.T) {
	router := newMockRouter(t)
	defer router.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg := NewConfig().WithEndpoint("tcp/" + router.Addr())
	cfg.ReconnectInitial = 20 * time.Millisecond
	cfg.ReconnectMax = 40 * time.Millisecond
	sess, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	subKE, _ := NewKeyExpr("demo/sub/**")
	sub, err := sess.DeclareSubscriber(subKE, NewFifoChannel[Sample](4))
	if err != nil {
		t.Fatalf("DeclareSubscriber: %v", err)
	}
	defer sub.Drop()

	qblKE, _ := NewKeyExpr("demo/qbl/**")
	qbl, err := sess.DeclareQueryable(qblKE, func(*Query) {}, &QueryableOptions{Complete: true})
	if err != nil {
		t.Fatalf("DeclareQueryable: %v", err)
	}
	defer qbl.Drop()

	tokKE, _ := NewKeyExpr("demo/token")
	tok, err := sess.Liveliness().DeclareToken(tokKE, nil)
	if err != nil {
		t.Fatalf("DeclareToken: %v", err)
	}
	defer tok.Drop()

	// Drain initial declarations from the first connection.
	gotSubID := drainSubscriberDeclare(t, router, "demo/sub/**")
	gotQblID := drainQueryableDeclare(t, router, "demo/qbl/**")
	gotTokID := drainTokenDeclare(t, router, "demo/token")

	// Drop the current link from the router side.
	router.currentConn().Close()

	// Reconnect orchestrator should redial and replay entities on the
	// freshly accepted connection. Assert we see the same IDs again.
	gotSubID2 := drainSubscriberDeclare(t, router, "demo/sub/**")
	gotQblID2 := drainQueryableDeclare(t, router, "demo/qbl/**")
	gotTokID2 := drainTokenDeclare(t, router, "demo/token")

	if gotSubID != gotSubID2 {
		t.Errorf("sub id changed across reconnect: %d → %d", gotSubID, gotSubID2)
	}
	if gotQblID != gotQblID2 {
		t.Errorf("qbl id changed across reconnect: %d → %d", gotQblID, gotQblID2)
	}
	if gotTokID != gotTokID2 {
		t.Errorf("tok id changed across reconnect: %d → %d", gotTokID, gotTokID2)
	}

	// Data-plane sanity: a PUSH injected on the *new* connection should
	// still reach the subscriber — proves the registry replay restored
	// not just the wire declarations but the end-to-end dispatch.
	router.injectPush(t, "demo/sub/hello", "post-reconnect")
	ch := sub.Handler().(<-chan Sample)
	select {
	case sample := <-ch:
		if got := string(sample.Payload().Bytes()); got != "post-reconnect" {
			t.Errorf("post-reconnect sample = %q, want %q", got, "post-reconnect")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("subscriber did not receive post-reconnect PUSH within 3s")
	}
}

func drainSubscriberDeclare(t *testing.T, router *mockRouter, wantKey string) uint32 {
	t.Helper()
	select {
	case d := <-router.declared:
		if d.key != wantKey {
			t.Errorf("D_SUBSCRIBER key=%q, want %q", d.key, wantKey)
		}
		return d.id
	case <-time.After(3 * time.Second):
		t.Fatalf("D_SUBSCRIBER %q not observed within 3s", wantKey)
	}
	return 0
}

func drainQueryableDeclare(t *testing.T, router *mockRouter, wantKey string) uint32 {
	t.Helper()
	select {
	case d := <-router.declaredQ:
		if d.key != wantKey {
			t.Errorf("D_QUERYABLE key=%q, want %q", d.key, wantKey)
		}
		return d.id
	case <-time.After(3 * time.Second):
		t.Fatalf("D_QUERYABLE %q not observed within 3s", wantKey)
	}
	return 0
}

func drainTokenDeclare(t *testing.T, router *mockRouter, wantKey string) uint32 {
	t.Helper()
	select {
	case d := <-router.declaredT:
		if d.key != wantKey {
			t.Errorf("D_TOKEN key=%q, want %q", d.key, wantKey)
		}
		return d.id
	case <-time.After(3 * time.Second):
		t.Fatalf("D_TOKEN %q not observed within 3s", wantKey)
	}
	return 0
}

// TestGetConsolidationLatest: four replies on two keys with increasing
// timestamps; Latest should emit exactly the newest reply per key.
func TestGetConsolidationLatest(t *testing.T) {
	router := newMockRouter(t)
	defer router.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sess, err := Open(ctx, NewConfig().WithEndpoint("tcp/"+router.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	ke, _ := NewKeyExpr("demo/latest/**")
	replies, err := sess.Get(ke, &GetOptions{
		Consolidation:    ConsolidationLatest,
		HasConsolidation: true,
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	var req inboundRequest
	select {
	case req = <-router.requests:
	case <-time.After(2 * time.Second):
		t.Fatal("no REQUEST observed")
	}

	router.injectReplyWithTS(t, req.requestID, "demo/latest/a", "a-v1", 100)
	router.injectReplyWithTS(t, req.requestID, "demo/latest/a", "a-v2", 300)
	router.injectReplyWithTS(t, req.requestID, "demo/latest/a", "a-v3", 200) // out-of-order
	router.injectReplyWithTS(t, req.requestID, "demo/latest/b", "b-v1", 150)
	router.injectResponseFinal(t, req.requestID)

	got := collectPayloads(t, replies, 2*time.Second)
	want := map[string]bool{"a-v2": true, "b-v1": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want exactly %v", got, want)
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected payload %q (want subset of %v)", p, want)
		}
	}
}

// TestGetConsolidationMonotonic: replies are streamed in arrival order
// except out-of-order ones are dropped.
func TestGetConsolidationMonotonic(t *testing.T) {
	router := newMockRouter(t)
	defer router.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sess, err := Open(ctx, NewConfig().WithEndpoint("tcp/"+router.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	ke, _ := NewKeyExpr("demo/mono/**")
	replies, err := sess.Get(ke, &GetOptions{
		Consolidation:    ConsolidationMonotonic,
		HasConsolidation: true,
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	var req inboundRequest
	select {
	case req = <-router.requests:
	case <-time.After(2 * time.Second):
		t.Fatal("no REQUEST observed")
	}

	router.injectReplyWithTS(t, req.requestID, "demo/mono/a", "a-v1", 100)
	router.injectReplyWithTS(t, req.requestID, "demo/mono/a", "a-v2", 200) // accepted
	router.injectReplyWithTS(t, req.requestID, "demo/mono/a", "a-stale", 150) // dropped
	router.injectReplyWithTS(t, req.requestID, "demo/mono/a", "a-v3", 300)   // accepted
	router.injectResponseFinal(t, req.requestID)

	got := collectPayloads(t, replies, 2*time.Second)
	want := []string{"a-v1", "a-v2", "a-v3"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want exactly %v (stale must be dropped)", got, want)
	}
	for i, p := range want {
		if got[i] != p {
			t.Errorf("got[%d] = %q, want %q", i, got[i], p)
		}
	}
}

// TestGetConsolidationMonotonicNoTimestamp: untimestamped replies must
// be forwarded and must NOT poison the per-key high-water mark —
// Monotonic degrades to None for sources that never stamp.
func TestGetConsolidationMonotonicNoTimestamp(t *testing.T) {
	router := newMockRouter(t)
	defer router.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sess, err := Open(ctx, NewConfig().WithEndpoint("tcp/"+router.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	ke, _ := NewKeyExpr("demo/mono-nots/**")
	replies, err := sess.Get(ke, &GetOptions{
		Consolidation:    ConsolidationMonotonic,
		HasConsolidation: true,
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var req inboundRequest
	select {
	case req = <-router.requests:
	case <-time.After(2 * time.Second):
		t.Fatal("no REQUEST observed")
	}

	router.injectReply(t, req.requestID, "k", "a")               // no-ts — forward
	router.injectReplyWithTS(t, req.requestID, "k", "b", 500)    // ts=500 — forward
	router.injectReply(t, req.requestID, "k", "c")               // no-ts — forward
	router.injectReplyWithTS(t, req.requestID, "k", "d", 300)    // ts=300 ≤ 500 — drop
	router.injectResponseFinal(t, req.requestID)

	got := collectPayloads(t, replies, 2*time.Second)
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i, p := range want {
		if got[i] != p {
			t.Errorf("got[%d] = %q, want %q", i, got[i], p)
		}
	}
}

// TestGetConsolidationNone: every reply reaches the caller, including
// duplicates and out-of-order timestamps.
func TestGetConsolidationNone(t *testing.T) {
	router := newMockRouter(t)
	defer router.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sess, err := Open(ctx, NewConfig().WithEndpoint("tcp/"+router.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	ke, _ := NewKeyExpr("demo/none/**")
	replies, err := sess.Get(ke, &GetOptions{
		Consolidation:    ConsolidationNone,
		HasConsolidation: true,
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var req inboundRequest
	select {
	case req = <-router.requests:
	case <-time.After(2 * time.Second):
		t.Fatal("no REQUEST observed")
	}

	router.injectReply(t, req.requestID, "k", "a")
	router.injectReply(t, req.requestID, "k", "b")
	router.injectReply(t, req.requestID, "k", "a") // duplicate — None must keep
	router.injectResponseFinal(t, req.requestID)

	got := collectPayloads(t, replies, 2*time.Second)
	if got := len(got); got != 3 {
		t.Fatalf("got %d replies, want 3", got)
	}
}

func collectPayloads(t *testing.T, ch <-chan Reply, timeout time.Duration) []string {
	t.Helper()
	deadline := time.After(timeout)
	var out []string
	for {
		select {
		case r, ok := <-ch:
			if !ok {
				return out
			}
			if s, hasSample := r.Sample(); hasSample {
				out = append(out, string(s.Payload().Bytes()))
			}
		case <-deadline:
			t.Fatal("timeout collecting replies")
			return out
		}
	}
}

func TestDeclareQuerierEmitsInterest(t *testing.T) {
	router := newMockRouter(t)
	defer router.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sess, err := Open(ctx, NewConfig().WithEndpoint("tcp/"+router.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	ke, _ := NewKeyExpr("demo/qr/**")
	qr, err := sess.DeclareQuerier(ke, &QuerierOptions{
		Target:    QueryTargetAll,
		HasTarget: true,
	})
	if err != nil {
		t.Fatalf("DeclareQuerier: %v", err)
	}

	// Observe INTEREST[CurrentFuture, Q=1, restricted=demo/qr/**].
	select {
	case obs := <-router.interests:
		if obs.final {
			t.Errorf("unexpected INTEREST Final")
		}
		if obs.mode != wire.InterestModeCurrentFuture {
			t.Errorf("mode=%v, want CurrentFuture", obs.mode)
		}
		if !obs.filter.Queryables {
			t.Errorf("filter.Queryables=false, want true")
		}
		if obs.key != "demo/qr/**" {
			t.Errorf("INTEREST key=%q, want demo/qr/**", obs.key)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("INTEREST not observed within 2s")
	}

	// Querier.Get should produce a REQUEST that carries the inherited
	// QueryTarget option.
	replies, err := qr.Get(nil)
	if err != nil {
		t.Fatalf("Querier.Get: %v", err)
	}
	var reqID uint32
	select {
	case req := <-router.requests:
		reqID = req.requestID
		target := codec.FindExt(req.extensions, wire.ReqExtIDQueryTarget)
		if target == nil || wire.QueryTarget(target.Z64) != wire.QueryTargetAll {
			t.Errorf("QueryTarget ext = %+v, want QueryTargetAll", target)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("REQUEST not observed within 2s")
	}
	// Cleanly finish the Get so the translator goroutine exits.
	router.injectResponseFinal(t, reqID)
	<-replies

	// Drop emits INTEREST Final with the same id.
	qr.Drop()
	select {
	case obs := <-router.interests:
		if !obs.final {
			t.Errorf("expected INTEREST Final after Drop, got %+v", obs)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("INTEREST Final not observed within 2s")
	}
}

// TestDeclareLivelinessSubscriberEmitsInterestAndDelivers: declare a
// liveliness subscriber, verify the INTEREST mode/filter/key, then
// inject D_TOKEN / U_TOKEN and assert the handler sees Put / Delete
// samples.
func TestDeclareLivelinessSubscriberEmitsInterestAndDelivers(t *testing.T) {
	router := newMockRouter(t)
	defer router.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sess, err := Open(ctx, NewConfig().WithEndpoint("tcp/"+router.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	ke, _ := NewKeyExpr("group1/**")
	ch := make(chan Sample, 4)
	sub, err := sess.Liveliness().DeclareSubscriber(ke, Closure[Sample]{
		Call: func(s Sample) { ch <- s },
	}, &LivelinessSubscriberOptions{History: true})
	if err != nil {
		t.Fatalf("DeclareSubscriber: %v", err)
	}
	defer sub.Drop()

	var interestID uint32
	select {
	case obs := <-router.interests:
		if obs.final {
			t.Errorf("unexpected INTEREST Final")
		}
		if obs.mode != wire.InterestModeCurrentFuture {
			t.Errorf("mode=%v, want CurrentFuture (History=true)", obs.mode)
		}
		if !obs.filter.KeyExprs || !obs.filter.Tokens {
			t.Errorf("filter=%+v, want K=1,T=1", obs.filter)
		}
		if obs.key != "group1/**" {
			t.Errorf("key=%q, want group1/**", obs.key)
		}
		interestID = obs.id
	case <-time.After(2 * time.Second):
		t.Fatal("INTEREST not observed within 2s")
	}

	// Inject a D_TOKEN then a U_TOKEN and verify dispatch.
	router.injectTokenDeclare(t, 0, 42, "group1/alpha")
	router.injectTokenUndeclare(t, 42, "group1/alpha")

	for i, want := range []SampleKind{SampleKindPut, SampleKindDelete} {
		select {
		case s := <-ch:
			if s.Kind() != want {
				t.Errorf("sample[%d] kind = %v, want %v", i, s.Kind(), want)
			}
			if s.KeyExpr().String() != "group1/alpha" {
				t.Errorf("sample[%d] key = %q, want group1/alpha", i, s.KeyExpr())
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("sample[%d] not delivered within 2s", i)
		}
	}

	// Drop → INTEREST Final with matching id.
	sub.Drop()
	select {
	case obs := <-router.interests:
		if !obs.final {
			t.Errorf("expected INTEREST Final, got %+v", obs)
		}
		if obs.id != interestID {
			t.Errorf("Final id=%d, want %d", obs.id, interestID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("INTEREST Final not observed within 2s")
	}
}

// TestLivelinessGetYieldsRepliesUntilFinal: the router sends two tokens
// then a DeclareFinal; the replies channel emits two Samples and closes.
func TestLivelinessGetYieldsRepliesUntilFinal(t *testing.T) {
	router := newMockRouter(t)
	defer router.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sess, err := Open(ctx, NewConfig().WithEndpoint("tcp/"+router.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	ke, _ := NewKeyExpr("live/**")
	replies, err := sess.Liveliness().Get(ke, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	var id uint32
	select {
	case obs := <-router.interests:
		if obs.mode != wire.InterestModeCurrent {
			t.Errorf("mode=%v, want Current", obs.mode)
		}
		id = obs.id
	case <-time.After(2 * time.Second):
		t.Fatal("INTEREST not observed within 2s")
	}

	router.injectTokenDeclare(t, id, 1, "live/a")
	router.injectTokenDeclare(t, id, 2, "live/b")
	router.injectDeclareFinal(t, id)

	got := 0
	for r := range replies {
		got++
		if s, ok := r.Sample(); ok {
			if s.Kind() != SampleKindPut {
				t.Errorf("reply kind=%v, want Put", s.Kind())
			}
		}
	}
	if got != 2 {
		t.Errorf("got %d replies, want 2", got)
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
