package scout

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// fakeResponder answers SCOUT with a HELLO on the same UDP socket.
// It returns the local address to feed into Options.UnicastAddresses.
type fakeResponder struct {
	t        *testing.T
	conn     *net.UDPConn
	hello    []byte
	respond  func(req *wire.Scout) bool // gate; return false to suppress reply
	received chan *wire.Scout
}

func newFakeResponder(t *testing.T, hello *wire.Hello, respond func(*wire.Scout) bool) *fakeResponder {
	return newFakeResponderOn(t, "udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}, hello, respond)
}

func newFakeResponderOn(t *testing.T, network string, laddr *net.UDPAddr, hello *wire.Hello, respond func(*wire.Scout) bool) *fakeResponder {
	t.Helper()
	conn, err := net.ListenUDP(network, laddr)
	if err != nil {
		t.Fatalf("listen %s: %v", network, err)
	}
	w := codec.NewWriter(64)
	if err := hello.EncodeTo(w); err != nil {
		t.Fatalf("encode HELLO: %v", err)
	}
	body := append([]byte(nil), w.Bytes()...)

	return &fakeResponder{
		t:        t,
		conn:     conn,
		hello:    body,
		respond:  respond,
		received: make(chan *wire.Scout, 8),
	}
}

func (r *fakeResponder) addr() string { return r.conn.LocalAddr().String() }

func (r *fakeResponder) run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() {
		<-ctx.Done()
		_ = r.conn.SetReadDeadline(time.Now())
	})
	defer wg.Wait()
	defer r.conn.Close()

	buf := make([]byte, 2048)
	for {
		n, src, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		rd := codec.NewReader(buf[:n])
		h, err := rd.DecodeHeader()
		if err != nil || h.ID != wire.IDScoutScout {
			continue
		}
		s, err := wire.DecodeScout(rd, h)
		if err != nil {
			continue
		}
		r.received <- s
		if r.respond != nil && !r.respond(s) {
			continue
		}
		_, _ = r.conn.WriteToUDP(r.hello, src)
	}
}

func makeZID(b byte) wire.ZenohID {
	return wire.ZenohID{Bytes: []byte{b, b + 1, b + 2, b + 3}}
}

func TestRun_DeliversUnicastHello(t *testing.T) {
	responderZID := makeZID(0x10)
	resp := newFakeResponder(t, &wire.Hello{
		Version:  wire.ProtoVersion,
		WhatAmI:  wire.WhatAmIRouter,
		ZID:      responderZID,
		Locators: []string{"tcp/127.0.0.1:7447"},
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go resp.run(ctx)
	t.Cleanup(cancel)

	var mu sync.Mutex
	var got []Hello
	err := Run(ctx, Options{
		UnicastAddresses: []string{resp.addr()},
		Interval:         50 * time.Millisecond,
		Timeout:          500 * time.Millisecond,
	}, func(h Hello) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, h)
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("want 1 HELLO, got %d: %+v", len(got), got)
	}
	if got[0].WhatAmI != wire.WhatAmIRouter {
		t.Errorf("WhatAmI = %v, want Router", got[0].WhatAmI)
	}
	if !got[0].ZID.Equal(responderZID) {
		t.Errorf("ZID = %x, want %x", got[0].ZID.Bytes, responderZID.Bytes)
	}
	if len(got[0].Locators) != 1 || got[0].Locators[0] != "tcp/127.0.0.1:7447" {
		t.Errorf("Locators = %v, want [tcp/127.0.0.1:7447]", got[0].Locators)
	}
}

func TestRun_DedupesByZID(t *testing.T) {
	resp := newFakeResponder(t, &wire.Hello{
		Version:  wire.ProtoVersion,
		WhatAmI:  wire.WhatAmIPeer,
		ZID:      makeZID(0x20),
		Locators: []string{"tcp/127.0.0.1:7500"},
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go resp.run(ctx)
	t.Cleanup(cancel)

	var count int
	err := Run(ctx, Options{
		UnicastAddresses: []string{resp.addr()},
		Interval:         30 * time.Millisecond, // multiple SCOUTs within timeout
		Timeout:          250 * time.Millisecond,
	}, func(Hello) { count++ })
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected dedup to 1 delivery, got %d", count)
	}
}

func TestRun_SelfSuppression(t *testing.T) {
	selfZID := makeZID(0x30)
	resp := newFakeResponder(t, &wire.Hello{
		Version:  wire.ProtoVersion,
		WhatAmI:  wire.WhatAmIClient,
		ZID:      selfZID, // responder impersonates our own ZID
		Locators: []string{"tcp/127.0.0.1:9999"},
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go resp.run(ctx)
	t.Cleanup(cancel)

	var count int
	err := Run(ctx, Options{
		UnicastAddresses: []string{resp.addr()},
		ZID:              selfZID,
		Interval:         30 * time.Millisecond,
		Timeout:          200 * time.Millisecond,
	}, func(Hello) { count++ })
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected self-suppression, got %d deliveries", count)
	}
}

func TestRun_SynthesizesLocatorFromSource(t *testing.T) {
	resp := newFakeResponder(t, &wire.Hello{
		Version: wire.ProtoVersion,
		WhatAmI: wire.WhatAmIRouter,
		ZID:     makeZID(0x40),
		// No locators → receiver should synthesize one from the src addr.
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go resp.run(ctx)
	t.Cleanup(cancel)

	var got Hello
	err := Run(ctx, Options{
		UnicastAddresses: []string{resp.addr()},
		Interval:         30 * time.Millisecond,
		Timeout:          200 * time.Millisecond,
	}, func(h Hello) { got = h })
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got.Locators) != 1 {
		t.Fatalf("want 1 synthesized locator, got %v", got.Locators)
	}
	// Source address is the responder's conn.LocalAddr (the listener),
	// not resp.addr() literally — but both point to 127.0.0.1 on the
	// ephemeral port. The responder writes back on the same socket, so
	// the source on our side matches resp.addr().
	want := "udp/" + resp.addr()
	if got.Locators[0] != want {
		t.Errorf("synthesized locator = %q, want %q", got.Locators[0], want)
	}
}

func TestRun_TimeoutHonoured(t *testing.T) {
	// No responder: the call must return after the timeout.
	start := time.Now()
	err := Run(t.Context(), Options{
		UnicastAddresses: []string{"127.0.0.1:1"}, // black-hole address; no one listens
		Interval:         50 * time.Millisecond,
		Timeout:          150 * time.Millisecond,
	}, func(Hello) { t.Fatal("unexpected delivery") })
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed < 100*time.Millisecond || elapsed > 1*time.Second {
		t.Errorf("elapsed = %v, expected ~150ms", elapsed)
	}
}

func TestRun_ContextCancelStops(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			UnicastAddresses: []string{"127.0.0.1:1"},
			Interval:         30 * time.Millisecond,
			Timeout:          -1, // disable implicit timeout
		}, func(Hello) {})
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not exit after ctx cancel")
	}
}

func TestRun_NoTargetsError(t *testing.T) {
	err := Run(context.Background(), Options{}, func(Hello) {})
	if err == nil {
		t.Fatal("expected error when no targets configured")
	}
}

// Two responders with the same ZID — the dispatcher must dedup across them.
func TestRun_DedupAcrossSockets(t *testing.T) {
	sharedZID := makeZID(0x50)
	hello := &wire.Hello{
		Version:  wire.ProtoVersion,
		WhatAmI:  wire.WhatAmIRouter,
		ZID:      sharedZID,
		Locators: []string{"tcp/127.0.0.1:7447"},
	}
	r1 := newFakeResponder(t, hello, nil)
	r2 := newFakeResponder(t, hello, nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go r1.run(ctx)
	go r2.run(ctx)
	t.Cleanup(cancel)

	var mu sync.Mutex
	var count int
	err := Run(ctx, Options{
		UnicastAddresses: []string{r1.addr(), r2.addr()},
		Interval:         30 * time.Millisecond,
		Timeout:          200 * time.Millisecond,
	}, func(Hello) {
		mu.Lock()
		count++
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 delivery despite two responders, got %d", count)
	}
}

// TestRun_IPv6Unicast exercises the v6 send/receive path. Skipped when the
// runtime does not have working IPv6 loopback (CI runners sometimes don't).
func TestRun_IPv6Unicast(t *testing.T) {
	probe, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback, Port: 0})
	if err != nil {
		t.Skipf("IPv6 not available: %v", err)
	}
	_ = probe.Close()

	responderZID := makeZID(0x60)
	resp := newFakeResponderOn(t, "udp6", &net.UDPAddr{IP: net.IPv6loopback, Port: 0}, &wire.Hello{
		Version:  wire.ProtoVersion,
		WhatAmI:  wire.WhatAmIRouter,
		ZID:      responderZID,
		Locators: []string{"tcp/[::1]:7447"},
	}, nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go resp.run(ctx)
	t.Cleanup(cancel)

	var got Hello
	err = Run(ctx, Options{
		UnicastAddresses: []string{resp.addr()},
		Interval:         30 * time.Millisecond,
		Timeout:          200 * time.Millisecond,
	}, func(h Hello) { got = h })
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !got.ZID.Equal(responderZID) {
		t.Fatalf("got ZID = %x, want %x", got.ZID.Bytes, responderZID.Bytes)
	}
}

func TestRun_InvalidMulticastAddress(t *testing.T) {
	err := Run(t.Context(), Options{
		MulticastEnabled: true,
		MulticastAddress: "not-a-host:port",
		Interval:         10 * time.Millisecond,
		Timeout:          50 * time.Millisecond,
	}, func(Hello) {})
	if err == nil {
		t.Fatal("expected error for invalid multicast address")
	}
}
