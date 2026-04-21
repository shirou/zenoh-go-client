package session

import (
	"errors"
	"io"
	"net"
	"sync"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/locator"
)

// pipeLink wraps a net.Conn (typically from net.Pipe) as a transport.Link
// using the same stream framing as the real TCP link. Used for handshake
// and session-level tests that don't need a real network.
type pipeLink struct {
	conn net.Conn
	loc  locator.Locator
	mu   sync.Mutex // serializes ReadBatch to match test expectations
}

func newPipeLink(conn net.Conn) *pipeLink {
	return &pipeLink{
		conn: conn,
		loc:  locator.Locator{Scheme: locator.SchemeTCP, Address: "127.0.0.1:0"},
	}
}

func (l *pipeLink) ReadBatch(buf []byte) (int, error) {
	n, err := codec.ReadStreamBatch(l.conn, buf)
	// net.Pipe close surfaces as io.ErrClosedPipe; translate to io.EOF so
	// callers can treat clean close uniformly.
	if err != nil && errors.Is(err, io.ErrClosedPipe) {
		return 0, io.EOF
	}
	return n, err
}

func (l *pipeLink) WriteBatch(batch []byte) error {
	return codec.EncodeStreamBatch(l.conn, batch)
}

func (l *pipeLink) Close() error                  { return l.conn.Close() }
func (l *pipeLink) RemoteLocator() locator.Locator { return l.loc }

// newPipeLinks returns a pair (client, server) connected to each other.
func newPipeLinks() (*pipeLink, *pipeLink) {
	a, b := net.Pipe()
	return newPipeLink(a), newPipeLink(b)
}
