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

// stubDialer is a zero-sized Dialer that fails every Dial with ErrNotImplemented.
type stubDialer locator.Scheme

func (d stubDialer) Scheme() locator.Scheme { return locator.Scheme(d) }

func (d stubDialer) Dial(ctx context.Context, loc locator.Locator) (Link, error) {
	return nil, fmt.Errorf("transport: scheme %q dialer not yet implemented", d)
}
