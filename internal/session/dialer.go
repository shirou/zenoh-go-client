package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/shirou/zenoh-go-client/internal/locator"
	"github.com/shirou/zenoh-go-client/internal/transport"
)

// DialOne dials a single endpoint and returns the resulting Link.
func DialOne(ctx context.Context, endpoint string) (transport.Link, error) {
	loc, err := locator.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", endpoint, err)
	}
	d := transport.DialerFor(loc.Scheme)
	if d == nil {
		return nil, fmt.Errorf("no dialer for scheme %q", loc.Scheme)
	}
	link, err := d.Dial(ctx, loc)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", endpoint, err)
	}
	return link, nil
}

// DialFirst iterates endpoints in order and returns the first successfully
// connected Link. Errors from every endpoint are joined so the caller can
// see which ones failed.
//
// ctx is passed through to each dialer, so a deadline or cancellation
// short-circuits the remaining attempts.
func DialFirst(ctx context.Context, endpoints []string) (transport.Link, error) {
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("dialer: no endpoints")
	}
	var attempts []error
	for _, ep := range endpoints {
		if err := ctx.Err(); err != nil {
			attempts = append(attempts, fmt.Errorf("%s: %w", ep, err))
			break
		}

		loc, err := locator.Parse(ep)
		if err != nil {
			attempts = append(attempts, fmt.Errorf("parse %q: %w", ep, err))
			continue
		}
		d := transport.DialerFor(loc.Scheme)
		if d == nil {
			attempts = append(attempts, fmt.Errorf("%s: no dialer for scheme %q", ep, loc.Scheme))
			continue
		}
		link, err := d.Dial(ctx, loc)
		if err == nil {
			return link, nil
		}
		attempts = append(attempts, fmt.Errorf("%s: %w", ep, err))
	}
	return nil, fmt.Errorf("dialer: all endpoints failed: %w", errors.Join(attempts...))
}
