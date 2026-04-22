package zenoh

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/shirou/zenoh-go-client/internal/locator"
	"github.com/shirou/zenoh-go-client/internal/scout"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// What is a bitmap of node roles to scout for. Bit positions correspond to
// WhatAmI values (bit0=Router, bit1=Peer, bit2=Client) so a single bit shift
// converts between the two.
type What uint8

// What flag constants. Combine with bitwise-OR.
const (
	WhatRouter What = 1 << WhatAmIRouter
	WhatPeer   What = 1 << WhatAmIPeer
	WhatClient What = 1 << WhatAmIClient

	// WhatAny matches all defined roles.
	WhatAny What = WhatRouter | WhatPeer | WhatClient

	// WhatDefault matches routers and peers, mirroring zenoh-go's default.
	WhatDefault What = WhatRouter | WhatPeer
)

// Hello is a reply from a scouted zenoh node.
type Hello struct {
	whatAmI  WhatAmI
	zid      Id
	locators []string
}

// WhatAmI returns the responder's node role.
func (h Hello) WhatAmI() WhatAmI { return h.whatAmI }

// ZId returns the responder's ZenohID.
func (h Hello) ZId() Id { return h.zid }

// Locators returns a copy of the locators advertised by the responder.
// When the HELLO carried no explicit locator list, a single synthesized
// "udp/<src>" entry based on the datagram source address is returned.
func (h Hello) Locators() []string { return slices.Clone(h.locators) }

// String returns a human-readable representation, matching zenoh-go's format.
func (h Hello) String() string {
	return fmt.Sprintf("Hello { pid: %s, whatami: %s, locators: %v }", h.zid, h.whatAmI, h.locators)
}

// ScoutOptions tunes a Scout call. Zero value = defaults.
type ScoutOptions struct {
	// TimeoutMs caps the overall scouting duration. 0 means default (3000 ms).
	TimeoutMs uint64

	// What selects which roles to discover. 0 means WhatDefault.
	What What
}

// Scout broadcasts SCOUT messages and delivers each HELLO reply via handler.
//
// When handler's Attach returns a non-nil channel handle (FifoChannel /
// RingChannel), Scout runs in the background and returns the channel
// immediately; the channel is closed when scouting stops. When the handle is
// nil (Closure), Scout blocks until the timeout elapses and returns (nil, nil).
func Scout(config Config, handler Handler[Hello], options *ScoutOptions) (<-chan Hello, error) {
	scoutOpts, err := buildScoutOptions(config, options)
	if err != nil {
		return nil, err
	}

	deliver, drop, userHandle := handler.Attach()
	// Closure handlers return nil; channel handlers return <-chan Hello.
	ch, _ := userHandle.(<-chan Hello)

	run := func() {
		err := scout.Run(context.Background(), scoutOpts, func(h scout.Hello) {
			deliver(helloFromInternal(h))
		})
		drop()
		if err != nil {
			// Surface at Warn: background-mode callers only see an empty,
			// immediately-closed channel otherwise.
			slog.Default().Warn("zenoh: scout terminated with error", "err", err)
		}
	}

	if ch != nil {
		go run()
		return ch, nil
	}
	// Closure-style handler: no user-visible channel, so block here to
	// match zenoh-go's foreground semantics.
	run()
	return nil, nil
}

// buildScoutOptions converts public Config + ScoutOptions into the internal
// scout.Options. Unicast UDP endpoints are harvested from config.Endpoints
// (locators starting with "udp/"); multicast is enabled by default.
func buildScoutOptions(config Config, options *ScoutOptions) (scout.Options, error) {
	if options == nil {
		options = &ScoutOptions{}
	}
	what := options.What
	if what == 0 {
		what = WhatDefault
	}
	if invalid := what & ^WhatAny; invalid != 0 {
		return scout.Options{}, fmt.Errorf("zenoh: Scout What has invalid bits %#b", invalid)
	}

	var unicast []string
	for _, ep := range config.Endpoints {
		// Malformed endpoints are Open's concern; silently skip here.
		loc, err := locator.Parse(ep)
		if err != nil {
			continue
		}
		if loc.Scheme == locator.SchemeUDP {
			unicast = append(unicast, loc.Address)
		}
	}

	sc := config.Scouting
	mcastAddr := sc.MulticastAddress
	// Accept either raw "host:port" or the locator-style "udp/host:port".
	if loc, err := locator.Parse(mcastAddr); err == nil && loc.Scheme == locator.SchemeUDP {
		mcastAddr = loc.Address
	}

	var zid wire.ZenohID
	if config.ZID != "" {
		id, err := NewIdFromHex(config.ZID)
		if err != nil {
			return scout.Options{}, fmt.Errorf("zenoh: Scout invalid ZID: %w", err)
		}
		zid = id.ToWireID()
	}

	timeout := time.Duration(options.TimeoutMs) * time.Millisecond
	if timeout == 0 {
		timeout = sc.Timeout
	}

	return scout.Options{
		MulticastEnabled: sc.MulticastMode != MulticastOff,
		MulticastAddress: mcastAddr,
		UnicastAddresses: unicast,
		Matcher:          wire.WhatAmIMatcher(what),
		ZID:              zid,
		Timeout:          timeout,
	}, nil
}

func helloFromInternal(h scout.Hello) Hello {
	return Hello{
		whatAmI:  WhatAmI(h.WhatAmI),
		zid:      IdFromWireID(h.ZID),
		locators: h.Locators,
	}
}
