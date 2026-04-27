package transport

import (
	"context"
	"fmt"

	"github.com/shirou/zenoh-go-client/internal/locator"
)

func init() {
	for _, s := range stubSchemes {
		RegisterDialer(stubDialer(s))
	}
	for _, s := range stubListenSchemes {
		RegisterListenerFactory(stubListenerFactory(s))
	}
}

var stubSchemes = []locator.Scheme{
	locator.SchemeUDP,
	locator.SchemeTLS,
	locator.SchemeQUIC,
	locator.SchemeWS,
	locator.SchemeWSS,
	locator.SchemeUnix,
	locator.SchemeSerial,
}

// stubListenSchemes is the set of schemes for which a listener factory is
// stubbed. Serial is intentionally omitted: it has no concept of accepting
// inbound connections.
var stubListenSchemes = []locator.Scheme{
	locator.SchemeUDP,
	locator.SchemeTLS,
	locator.SchemeQUIC,
	locator.SchemeWS,
	locator.SchemeWSS,
	locator.SchemeUnix,
}

// stubDialer is a zero-sized Dialer that fails every Dial with ErrNotImplemented.
type stubDialer locator.Scheme

func (d stubDialer) Scheme() locator.Scheme { return locator.Scheme(d) }

func (d stubDialer) Dial(ctx context.Context, loc locator.Locator) (Link, error) {
	return nil, fmt.Errorf("transport: scheme %q dialer not yet implemented", d)
}

// stubListenerFactory fails Listen with ErrNotImplemented so callers can
// detect the scheme without crashing on a missing factory.
type stubListenerFactory locator.Scheme

func (f stubListenerFactory) Scheme() locator.Scheme { return locator.Scheme(f) }

func (f stubListenerFactory) Listen(ctx context.Context, loc locator.Locator) (Listener, error) {
	return nil, fmt.Errorf("transport: scheme %q listener not yet implemented", f)
}
