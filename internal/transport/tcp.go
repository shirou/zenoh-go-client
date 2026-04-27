package transport

import (
	"context"
	"errors"
	"fmt"
	"net"

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
	bound := loc
	if a, ok := ln.Addr().(*net.TCPAddr); ok {
		bound.Address = a.String()
	}
	return &tcpListener{ln: ln, loc: bound}, nil
}

type tcpListener struct {
	ln  net.Listener
	loc locator.Locator
}

func (l *tcpListener) Scheme() locator.Scheme { return locator.SchemeTCP }

func (l *tcpListener) Addr() locator.Locator { return l.loc }

func (l *tcpListener) Accept(ctx context.Context) (Link, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := l.ln.Accept()
		ch <- result{c, err}
	}()
	select {
	case <-ctx.Done():
		_ = l.ln.Close()
		// Drain the in-flight accept so we don't leak the goroutine.
		if r := <-ch; r.conn != nil {
			_ = r.conn.Close()
		}
		return nil, ctx.Err()
	case r := <-ch:
		if r.err != nil {
			if errors.Is(r.err, net.ErrClosed) {
				return nil, ErrListenerClosed
			}
			return nil, r.err
		}
		if tc, ok := r.conn.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
		}
		remote := locator.Locator{Scheme: locator.SchemeTCP, Address: r.conn.RemoteAddr().String()}
		return &tcpLink{conn: r.conn, loc: remote}, nil
	}
}

func (l *tcpListener) Close() error {
	if err := l.ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}
