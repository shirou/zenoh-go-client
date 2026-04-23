package transport

import (
	"context"
	"fmt"
	"net"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/locator"
)

func init() {
	RegisterDialer(tcpDialer{})
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
