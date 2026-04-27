package transport

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/locator"
)

func TestTCPListenerAcceptDial(t *testing.T) {
	factory := ListenerFactoryFor(locator.SchemeTCP)
	if factory == nil {
		t.Fatal("no TCP listener factory registered")
	}
	loc := locator.Locator{Scheme: locator.SchemeTCP, Address: "127.0.0.1:0"}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ln, err := factory.Listen(ctx, loc)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	bound := ln.Addr()
	if bound.Scheme != locator.SchemeTCP {
		t.Fatalf("bound scheme = %q, want tcp", bound.Scheme)
	}
	if bound.Address == "127.0.0.1:0" || bound.Address == "" {
		t.Fatalf("bound address not resolved: %q", bound.Address)
	}

	type acceptResult struct {
		link Link
		err  error
	}
	accepted := make(chan acceptResult, 1)
	go func() {
		l, err := ln.Accept(ctx)
		accepted <- acceptResult{l, err}
	}()

	dialer := DialerFor(locator.SchemeTCP)
	if dialer == nil {
		t.Fatal("no TCP dialer registered")
	}
	dialed, err := dialer.Dial(ctx, bound)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer dialed.Close()

	r := <-accepted
	if r.err != nil {
		t.Fatalf("Accept: %v", r.err)
	}
	defer r.link.Close()

	payload := []byte("ping")
	if err := dialed.WriteBatch(payload); err != nil {
		t.Fatalf("dialed.WriteBatch: %v", err)
	}
	buf := make([]byte, codec.MaxBatchSize)
	n, err := r.link.ReadBatch(buf)
	if err != nil {
		t.Fatalf("accepted.ReadBatch: %v", err)
	}
	if !bytes.Equal(buf[:n], payload) {
		t.Errorf("got %q, want %q", buf[:n], payload)
	}
}

func TestTCPListenerCloseUnblocksAccept(t *testing.T) {
	factory := ListenerFactoryFor(locator.SchemeTCP)
	loc, _ := locator.Parse("tcp/127.0.0.1:0")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ln, err := factory.Listen(ctx, loc)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := ln.Accept(ctx)
		done <- err
	}()

	// Give Accept a moment to block.
	time.Sleep(50 * time.Millisecond)
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case err := <-done:
		if !errors.Is(err, ErrListenerClosed) {
			t.Errorf("Accept after Close returned %v, want ErrListenerClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Accept did not return after Close")
	}
}

func TestTCPListenerContextCancel(t *testing.T) {
	factory := ListenerFactoryFor(locator.SchemeTCP)
	loc, _ := locator.Parse("tcp/127.0.0.1:0")
	bg := context.Background()

	ln, err := factory.Listen(bg, loc)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() {
		_, err := ln.Accept(ctx)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Accept after cancel returned %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Accept did not return after context cancel")
	}
}

func TestStubListenerFactoryFails(t *testing.T) {
	for _, s := range []locator.Scheme{locator.SchemeUDP, locator.SchemeTLS, locator.SchemeQUIC} {
		f := ListenerFactoryFor(s)
		if f == nil {
			t.Errorf("scheme %q has no listener factory", s)
			continue
		}
		_, err := f.Listen(context.Background(), locator.Locator{Scheme: s, Address: "127.0.0.1:0"})
		if err == nil {
			t.Errorf("stub scheme %q.Listen should fail", s)
		}
	}
	// Serial intentionally has no listener factory.
	if f := ListenerFactoryFor(locator.SchemeSerial); f != nil {
		t.Errorf("serial should have no listener factory, got %T", f)
	}
}
