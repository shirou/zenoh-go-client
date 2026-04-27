// Package transport provides the link abstraction over which Zenoh protocol
// messages are exchanged. Concrete TCP/TLS/QUIC implementations register
// themselves via RegisterDialer.
package transport

import (
	"context"
	"errors"

	"github.com/shirou/zenoh-go-client/internal/locator"
)

// ErrListenerClosed is returned by Listener.Accept when the listener has
// been closed.
var ErrListenerClosed = errors.New("transport: listener closed")

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
	// LocalAddress returns the string form of the local endpoint
	// (e.g. "192.0.2.1:54321"). Empty if the transport has no meaningful
	// local address (datagram / in-memory pipes).
	LocalAddress() string
}

// Dialer establishes a new Link to the given locator.
type Dialer interface {
	Scheme() locator.Scheme
	Dial(ctx context.Context, loc locator.Locator) (Link, error)
}

// Listener accepts incoming Links. Implementations must be safe for one
// concurrent Accept caller and one concurrent Close caller. Close unblocks
// any pending Accept with ErrListenerClosed.
type Listener interface {
	// Scheme reports the transport scheme this listener serves.
	Scheme() locator.Scheme
	// Addr returns the resolved listening locator (with the actually-bound
	// port if the request used :0).
	Addr() locator.Locator
	// Accept blocks until an incoming connection is established or the
	// context is cancelled or the listener is closed.
	Accept(ctx context.Context) (Link, error)
	// Close stops accepting new connections.
	Close() error
}

// ListenerFactory opens a Listener bound to the given locator.
type ListenerFactory interface {
	Scheme() locator.Scheme
	Listen(ctx context.Context, loc locator.Locator) (Listener, error)
}

// dialers is populated by init() functions of concrete transport files.
var dialers = map[locator.Scheme]Dialer{}

// listenerFactories is populated by init() functions of concrete transport
// files. Schemes that cannot listen (e.g. serial) simply do not register one.
var listenerFactories = map[locator.Scheme]ListenerFactory{}

// RegisterDialer registers a dialer for a given scheme. Called by transport
// implementations at init-time.
func RegisterDialer(d Dialer) {
	dialers[d.Scheme()] = d
}

// DialerFor returns the registered dialer for the given scheme, or nil.
func DialerFor(scheme locator.Scheme) Dialer {
	return dialers[scheme]
}

// RegisterListenerFactory registers a listener factory for a given scheme.
// Called by transport implementations at init-time.
func RegisterListenerFactory(f ListenerFactory) {
	listenerFactories[f.Scheme()] = f
}

// ListenerFactoryFor returns the registered listener factory for the given
// scheme, or nil.
func ListenerFactoryFor(scheme locator.Scheme) ListenerFactory {
	return listenerFactories[scheme]
}
