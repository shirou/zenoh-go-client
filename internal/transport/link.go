// Package transport provides the link abstraction over which Zenoh protocol
// messages are exchanged. Concrete TCP/TLS/QUIC implementations register
// themselves via RegisterDialer.
package transport

import (
	"context"

	"github.com/shirou/zenoh-go-client/internal/locator"
)

// Link represents a full-duplex framed connection to a peer.
//
// Implementations must be safe for one concurrent reader + one concurrent
// writer (standard net.Conn convention). The caller serializes Write calls
// and serializes Read calls.
//
// Each Read and Write operates on one framed batch (stream-based transports
// prepend a u16 LE length; datagram transports use the datagram boundary).
type Link interface {
	// ReadBatch fills buf with one framed batch and returns the number of
	// bytes read. Returns io.EOF on clean close.
	ReadBatch(buf []byte) (int, error)
	// WriteBatch writes one framed batch.
	WriteBatch(batch []byte) error
	// Close closes the link.
	Close() error
	// RemoteLocator returns the locator the link is connected to.
	RemoteLocator() locator.Locator
}

// Dialer establishes a new Link to the given locator.
type Dialer interface {
	Scheme() locator.Scheme
	Dial(ctx context.Context, loc locator.Locator) (Link, error)
}

// dialers is populated by init() functions of concrete transport files.
var dialers = map[locator.Scheme]Dialer{}

// RegisterDialer registers a dialer for a given scheme. Called by transport
// implementations at init-time.
func RegisterDialer(d Dialer) {
	dialers[d.Scheme()] = d
}

// DialerFor returns the registered dialer for the given scheme, or nil.
func DialerFor(scheme locator.Scheme) Dialer {
	return dialers[scheme]
}
