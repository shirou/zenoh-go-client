package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/locator"
)

func init() {
	RegisterDialer(tcpDialer{})
	RegisterListenerFactory(tcpListenerFactory{})
}

type tcpDialer struct{}

func (tcpDialer) Scheme() locator.Scheme { return locator.SchemeTCP }

func (tcpDialer) Dial(ctx context.Context, loc locator.Locator) (Link, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", loc.Address)
	if err != nil {
		return nil, fmt.Errorf("tcp dial %q: %w", loc.Address, err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	return &tcpLink{conn: conn, loc: loc}, nil
}

type tcpLink struct {
	conn net.Conn
	loc  locator.Locator
}

func (l *tcpLink) ReadBatch(buf []byte) (int, error) {
	return codec.ReadStreamBatch(l.conn, buf)
}

func (l *tcpLink) WriteBatch(batch []byte) error {
	return codec.EncodeStreamBatch(l.conn, batch)
}

func (l *tcpLink) Close() error { return l.conn.Close() }

func (l *tcpLink) RemoteLocator() locator.Locator { return l.loc }

func (l *tcpLink) LocalAddress() string {
	if a := l.conn.LocalAddr(); a != nil {
		return a.String()
	}
	return ""
}

type tcpListenerFactory struct{}

func (tcpListenerFactory) Scheme() locator.Scheme { return locator.SchemeTCP }

func (tcpListenerFactory) Listen(ctx context.Context, loc locator.Locator) (Listener, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", loc.Address)
	if err != nil {
		return nil, fmt.Errorf("tcp listen %q: %w", loc.Address, err)
	}
	tcpLn, ok := ln.(*net.TCPListener)
	if !ok {
		_ = ln.Close()
		return nil, fmt.Errorf("tcp listen %q: returned %T, not *net.TCPListener", loc.Address, ln)
	}
	bound := loc
	bound.Address = tcpLn.Addr().String()
	return &tcpListener{ln: tcpLn, loc: bound}, nil
}

// tcpListener implements Listener over *net.TCPListener. ctx-aware Accept
// is built on SetDeadline rather than closing the listener so a single
// Listener can serve multiple Accept calls with different contexts —
// only Close() is allowed to permanently stop the listener.
type tcpListener struct {
	ln  *net.TCPListener
	loc locator.Locator
}

func (l *tcpListener) Scheme() locator.Scheme { return locator.SchemeTCP }

func (l *tcpListener) Addr() locator.Locator { return l.loc }

func (l *tcpListener) Accept(ctx context.Context) (Link, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Clear any prior deadline left over from a previous ctx-cancelled
	// Accept so this call blocks indefinitely until either the deadline
	// helper below pokes it or a real connection arrives.
	_ = l.ln.SetDeadline(time.Time{})

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			// Wake the blocked Accept by setting an immediately-elapsed
			// deadline. The listener stays open for subsequent calls.
			_ = l.ln.SetDeadline(time.Now())
		case <-done:
		}
	}()

	c, err := l.ln.Accept()
	if err != nil {
		if errors.Is(err, net.ErrClosed) {
			return nil, ErrListenerClosed
		}
		// Translate the SetDeadline-induced timeout back to ctx.Err()
		// so callers see the original cancel cause, not a timeout error.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	remote := locator.Locator{Scheme: locator.SchemeTCP, Address: c.RemoteAddr().String()}
	return &tcpLink{conn: c, loc: remote}, nil
}

func (l *tcpListener) Close() error {
	if err := l.ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}
