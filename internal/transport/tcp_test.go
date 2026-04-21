package transport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/locator"
)

func TestTCPLinkRoundtrip(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	serverDone := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		buf := make([]byte, codec.MaxBatchSize)
		n, err := codec.ReadStreamBatch(conn, buf)
		if err != nil {
			serverDone <- err
			return
		}
		serverDone <- codec.EncodeStreamBatch(conn, buf[:n])
	}()

	loc, err := locator.Parse("tcp/" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	d := DialerFor(locator.SchemeTCP)
	if d == nil {
		t.Fatal("no TCP dialer registered")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	link, err := d.Dial(ctx, loc)
	if err != nil {
		t.Fatal(err)
	}
	defer link.Close()

	if link.RemoteLocator().String() != loc.String() {
		t.Errorf("RemoteLocator = %v, want %v", link.RemoteLocator(), loc)
	}

	payload := []byte("hello zenoh")
	if err := link.WriteBatch(payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, codec.MaxBatchSize)
	n, err := link.ReadBatch(got)
	if err != nil {
		t.Fatalf("ReadBatch: %v", err)
	}
	if !bytes.Equal(got[:n], payload) {
		t.Errorf("echo mismatch: got %q, want %q", got[:n], payload)
	}
	if err := <-serverDone; err != nil {
		t.Errorf("server: %v", err)
	}
}

// TestTCPReadEOF: a clean server close must surface as io.EOF so the
// session reader can distinguish peer-closed from protocol-error.
func TestTCPReadEOF(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		if conn, err := ln.Accept(); err == nil {
			conn.Close()
		}
	}()

	loc, err := locator.Parse("tcp/" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	link, err := DialerFor(locator.SchemeTCP).Dial(ctx, loc)
	if err != nil {
		t.Fatal(err)
	}
	defer link.Close()

	buf := make([]byte, 128)
	if _, err := link.ReadBatch(buf); !errors.Is(err, io.EOF) {
		t.Errorf("expected io.EOF on clean peer close, got %v", err)
	}
}

func TestStubDialers(t *testing.T) {
	for _, s := range stubSchemes {
		t.Run(string(s), func(t *testing.T) {
			d := DialerFor(s)
			if d == nil {
				t.Fatalf("no dialer for %q", s)
			}
			var addr string
			switch s {
			case locator.SchemeUnix:
				addr = "/tmp/x.sock"
			case locator.SchemeSerial:
				addr = "/dev/null"
			default:
				addr = "example.com:7447"
			}
			_, err := d.Dial(context.Background(), locator.Locator{Scheme: s, Address: addr})
			if err == nil {
				t.Fatalf("%q stub dial should fail", s)
			}
			if !strings.Contains(err.Error(), string(s)) {
				t.Errorf("stub error for %q should mention scheme name; got %q", s, err)
			}
		})
	}
}

func TestTCPDialContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// TEST-NET-1 is unroutable; the cancelled context must short-circuit
	// before the OS connect timeout.
	loc, err := locator.Parse("tcp/192.0.2.1:1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = DialerFor(locator.SchemeTCP).Dial(ctx, loc)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestTCPWriteBatchOversize(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(io.Discard, c)
	}()

	loc, err := locator.Parse("tcp/" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	link, err := DialerFor(locator.SchemeTCP).Dial(ctx, loc)
	if err != nil {
		t.Fatal(err)
	}
	defer link.Close()

	err = link.WriteBatch(make([]byte, codec.MaxBatchSize+1))
	if !errors.Is(err, codec.ErrBatchTooLarge) {
		t.Errorf("expected ErrBatchTooLarge, got %v", err)
	}
}

func TestTCPReadAfterClose(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// Server accepts then immediately closes. Regardless of which side
	// closes first, ReadBatch on a closed link must return an error.
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Close()
		}
	}()

	loc, err := locator.Parse("tcp/" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	link, err := DialerFor(locator.SchemeTCP).Dial(ctx, loc)
	if err != nil {
		t.Fatal(err)
	}
	_ = link.Close()

	if _, err := link.ReadBatch(make([]byte, 16)); err == nil {
		t.Error("ReadBatch on closed link should fail")
	}
}
