# zenoh-go-client

A **pure-Go** client for [Eclipse Zenoh](https://zenoh.io/), speaking the Zenoh
1.0 wire protocol (version `0x09`) directly against a `zenohd` router — no
cgo, no `zenoh-c` dependency, no external runtime.

The public API is a thin mirror of the upstream [cgo-based
`eclipse-zenoh/zenoh-go`](https://github.com/eclipse-zenoh/zenoh-go), so code
written against this library can be ported to (or from) the canonical binding
with minimal changes.

**Status: pre-1.0 / API may change.** The wire protocol is stable (Zenoh 1.0)
and interop-tested against `eclipse/zenoh:1.0.0` + Python `eclipse-zenoh` as
the canonical counterpart.

## Why pure Go?

The upstream `zenoh-go` wraps `zenoh-c` via cgo. That brings two constraints
this library removes:

- **No cgo toolchain.** `go build` works out of the box: no CMake, no
  `LD_LIBRARY_PATH`, no platform-specific shared libraries to ship with your
  binary. Fully cross-compilable with `GOOS`/`GOARCH` alone.
- **No external native library at runtime.** Single static binary; no
  version-skew between the Go package and the underlying C library.

The trade-off is scope: this library implements only what has been written
(see below). Features parity with `zenoh-rust` lags the upstream binding.

## Install

```sh
go get github.com/shirou/zenoh-go-client
```

Requires Go 1.26+.

## Quick start

### Publisher

```go
package main

import (
	"context"
	"time"

	"github.com/shirou/zenoh-go-client/zenoh"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg := zenoh.NewConfig().WithEndpoint("tcp/127.0.0.1:7447")
	session, _ := zenoh.Open(ctx, cfg)
	defer session.Close()

	ke, _ := zenoh.NewKeyExpr("demo/example/zenoh-go")
	pub, _ := session.DeclarePublisher(ke, nil)
	defer pub.Drop()

	_ = pub.Put(zenoh.NewZBytesFromString("hello from pure Go!"), nil)
}
```

### Subscriber

```go
ke, _ := zenoh.NewKeyExpr("demo/example/**")
sub, _ := session.DeclareSubscriber(ke, zenoh.Closure[zenoh.Sample]{
	Call: func(s zenoh.Sample) {
		fmt.Printf(">> %s %q\n", s.KeyExpr(), s.Payload().String())
	},
})
defer sub.Drop()
```

### Get / Queryable

```go
// Queryable side
qbl, _ := session.DeclareQueryable(ke, func(q *zenoh.Query) {
	_ = q.Reply(ke, zenoh.NewZBytesFromString("pong"), nil)
}, nil)
defer qbl.Drop()

// Getter side
replies, _ := session.Get(ke, &zenoh.GetOptions{
	Timeout: 2 * time.Second,
	Target:  zenoh.QueryTargetAll,
	HasTarget: true,
})
for r := range replies {
	if s, ok := r.Sample(); ok {
		fmt.Println("reply:", s.Payload().String())
	}
}
```

### Matching Listener

```go
pub, _ := session.DeclarePublisher(ke, nil)
defer pub.Drop()

listener, _ := pub.DeclareMatchingListener(zenoh.NewFifoChannel[zenoh.MatchingStatus](4))
defer listener.Drop()

for st := range listener.Handler() {
    fmt.Println("matching subscribers present:", st.Matching)
}
```

### Scout

```go
cfg := zenoh.NewConfig() // multicast on 224.0.0.224:7446 by default
ch, _ := zenoh.Scout(cfg, zenoh.NewFifoChannel[zenoh.Hello](8), &zenoh.ScoutOptions{
    TimeoutMs: 3000,
    What:      zenoh.WhatRouter | zenoh.WhatPeer,
})
for hello := range ch {
    fmt.Printf("discovered %s %s %v\n", hello.WhatAmI(), hello.ZId(), hello.Locators())
}
```

Runnable variants live under [`examples/`](examples/):
`z_pub`, `z_sub`, `z_get`, `z_queryable`, `z_liveliness`.

## Implemented features

### Transport

- TCP unicast (client mode)
- INIT / OPEN 4-message handshake
- Session lease + KEEPALIVE ticker
- Lease watchdog (detects silent peer)
- 16 QoS lanes (8 priorities × reliable / best-effort)
- QoS extension on FRAME (priority, `D` / `E` bits)
- FRAGMENT send + receive (1 GiB reassembly cap)
- Batching with express bypass (`E` flag)
- CLOSE with reason codes, surfaced as typed errors (including `CONNECTION_TO_SELF`)
- SHAKE128 deterministic initial sequence number
- Multi-endpoint failover during `Open`
- **Automatic reconnect** with exponential backoff, configurable via
  `Config.ReconnectInitial` / `ReconnectMax` / `ReconnectFactor`
  - On reconnect, every live `Subscriber` / `Queryable` / `Publisher` /
    `Querier` / `LivelinessToken` / liveliness subscriber is re-declared
    on the fresh link — user code does not need to retry. Matching
    listeners attached to a `Publisher` or `Querier` survive the reset
    and re-receive the fresh snapshot (see the Matching Listener section).

### Key expressions

- Canonical form validation and autocanonize (`**/**` collapse, `**/*` → `*/**`)
- `Intersects` / `Includes` (classical recursive, literal-only fast path)
- `Join` / `Concat`

### Publish / Subscribe

- `Session.Put` / `PutWithContext`, `Session.Delete` / `DeleteWithContext`,
  `Publisher.Put`, `Publisher.Delete`
- `DeclareSubscriber` with `Handler[T]`:
  - `Closure[T]` — dedicated dispatcher goroutine per subscriber
  - `FifoChannel[T]` — bounded channel, drop-on-full
  - `RingChannel[T]` — bounded channel, drop-oldest
- Per-emission options: encoding, priority, congestion control (`Block` / `Drop`), express

### Encoding

- Full upstream catalogue of 53 predefined encodings — IDs mirror
  zenoh-rust so the wire representation is identical
  (`EncodingZenohBytes` / `EncodingTextPlain` / `EncodingApplicationJson` /
  `EncodingImagePng` / `EncodingVideoH264` / …)
- `Encoding.String()` / `NewEncodingFromString()` round-trip through the
  canonical `"<prefix>;<schema>"` form (e.g. `text/plain;utf-8`)

### Query / Queryable

- `Session.Get` (+ `GetWithContext`) with options:
  - `ConsolidationMode` (Auto / None / Monotonic / Latest) — **applied
    client-side** in the translator:
    - `None` streams every reply in arrival order
    - `Monotonic` drops replies whose timestamp ≤ the per-key high-water mark
    - `Latest` / `Auto` (default) buffers every reply and emits at most one
      per key at `RESPONSE_FINAL`, picking the newest timestamp; error
      replies are flushed first, then data replies in first-arrival order
  - **Default changed** from "stream every reply" to `Auto` (= Latest).
    Callers that want the old streaming behaviour must set
    `HasConsolidation: true, Consolidation: ConsolidationNone`.
  - `QueryTarget` (BestMatching / All / AllComplete)
  - `Budget` (max replies)
  - `Timeout` (deadline, enforced client-side *and* advertised on the wire)
  - `Parameters` string
- `DeclareQueryable` with `Complete` option
- `Query.Reply` / `ReplyDel` / `ReplyErr`; `RESPONSE_FINAL` auto-emitted
- `Session.DeclareQuerier` — emits `INTEREST[Mode=CurrentFuture, Q=1,
  restricted=keyExpr]` so the router tracks matching queryables; successive
  `Querier.Get` / `Querier.GetWithContext` inherit the querier's defaults

### Liveliness

- `Session.Liveliness().DeclareToken(keyExpr, nil)` → `LivelinessToken` —
  own-side presence marker, auto-replayed on reconnect
- `Session.Liveliness().DeclareSubscriber(keyExpr, handler, opts)` — emits
  `INTEREST[Mode=Future|CurrentFuture, K+T, restricted=keyExpr]`. Each
  D_TOKEN arrives as a `Put` Sample, each U_TOKEN as a `Delete` Sample.
  `opts.History=true` asks the router to also replay currently-alive
  tokens at declare time. Auto-replayed on reconnect.
- `Session.Liveliness().Get(keyExpr, opts)` (+ `GetWithContext`) — emits
  `INTEREST[Mode=Current, K+T, restricted=keyExpr]`. Returns a reply
  channel that yields one `Put` Sample per currently-alive matching
  token, then closes when the router sends `DeclareFinal`.
- Receive-side `D_KEYEXPR` alias resolution lands alongside the
  Liveliness Subscriber path (zenohd uses scoped expressions when
  forwarding D_TOKEN, so this is required for interop).

### Matching Listener

- `Publisher.DeclareMatchingListener(handler)` — fires when the set of
  matching Subscribers transitions between empty and non-empty. Emits
  `INTEREST[Mode=CurrentFuture, K+S, restricted=keyExpr]` on declare
  and `INTEREST[Final]` on Drop. The first delivery arrives when the
  router's initial snapshot completes (`DeclareFinal`), reflecting the
  current count; subsequent deliveries fire only on 0↔1 transitions.
- `Querier.DeclareMatchingListener(handler)` — same semantics over
  matching Queryables.
- `DeclareBackgroundMatchingListener(Closure)` for fire-and-forget use.
- `GetMatchingStatus()` reads the current flag synchronously.
- Auto-replayed on reconnect: matching counts reset, INTEREST is
  re-emitted, and listeners re-receive the fresh snapshot.

### Discovery (Scout)

- `zenoh.Scout(config, handler, options)` — UDP SCOUT / HELLO over
  IPv4 or IPv6 multicast (default group `224.0.0.224:7446`). Supports
  unicast targets from `config.Endpoints` (entries beginning with
  `udp/`), outgoing-interface selection, TTL override, and an optional
  listen socket for observing passive HELLO advertisements.
- Background vs foreground is chosen by handler kind: `Closure` blocks,
  `FifoChannel` / `RingChannel` return a channel immediately and close
  it on timeout.

### Configuration

`Config` can be built programmatically or loaded from JSON5:

```go
// Programmatic
cfg := zenoh.NewConfig().WithEndpoint("tcp/127.0.0.1:7447")
cfg.ZID = "deadbeef"
cfg.ReconnectInitial = 500 * time.Millisecond

// From a JSON5 file (Rust-compatible key paths)
cfg, err := zenoh.NewConfigFromFile("zenoh.json5")

// From an inline JSON5 string
cfg, err := zenoh.NewConfigFromString(`{
    mode: "client",
    connect: {
        endpoints: ["tcp/127.0.0.1:7447"],
        retry: { period_init_ms: 500, period_max_ms: 4000 },
    },
    scouting: { multicast: { enabled: false } },
}`)

// Incremental edits via the Rust-style '/' path
cfg.InsertJSON5("connect/endpoints", `["tcp/127.0.0.1:7447"]`)
cfg.InsertJSON5("scouting/multicast/ttl", `4`)
```

JSON5 features supported: `//` and `/* */` comments, single-quoted strings,
trailing commas, unquoted identifier keys, hex integer literals. Keys the
pure-Go client does not yet understand (e.g. `transport/*`, `routing/*`) are
silently ignored so a config file aimed at `zenohd` can be reused verbatim;
a wrong value type at a recognised key surfaces as an error.

### Introspection

```go
if info := session.LinkInfo(); info != nil {
    fmt.Printf("connected %s ↔ %s (peer %s, batch=%d, qos=%v)\n",
        info.LocalAddress, info.RemoteLocator,
        info.PeerZID, info.NegotiatedBatchSize, info.QoSEnabled)
}
```

`LinkInfo` is a snapshot of the current link (local/remote addresses, peer
ZID + role, negotiated batch size + resolution + leases, QoS flag). Returns
`nil` when the session is closed or mid-reconnect.

Per-lane byte / message counters (cumulative traffic statistics) are not yet
exposed — only the negotiated handshake parameters above.

### Context / cancellation

- `Open(ctx, cfg)` — ctx bounds the dial + handshake
- `Session.GetWithContext(ctx, keyExpr, opts)` — ctx cancel closes the reply
  channel promptly; combined with `opts.Timeout` for hard deadlines
- `Session.PutWithContext` / `DeleteWithContext` / `Querier.GetWithContext`
  for symmetric ctx support on every blocking user-facing call
- `CancellationToken` (behind `-tags zenoh_unstable`) — zenoh-rust-style
  cancellation handle. Attach via
  `opts := (&GetOptions{}).WithCancellation(tok)`; `tok.Cancel()` aborts the
  Get whether or not the caller holds the ctx

### Testing

- 100% green under `go test -race -count=1 ./...`
- `go.uber.org/goleak` — zero goroutine leak on `Session.Close`
- Fuzz tests for every wire decoder (INIT / OPEN / FRAME / FRAGMENT / PUSH /
  REQUEST / RESPONSE / DECLARE / INTEREST / SCOUT / HELLO / VLE)
- Integration tests under [`tests/interop/`](tests/interop/) using
  `docker-compose` + `eclipse/zenoh:1.0.0` + Python `eclipse-zenoh==1.2.1` as
  the canonical counterpart — covers both directions (Go ↔ Py) for pub/sub,
  get/queryable, liveliness tokens, and Python-token → Go-liveliness-subscriber

## Future work

Not yet implemented, in rough priority order:

- **Scout enhancements** — IPv6 fully but `MulticastInterface` / TTL
  exercised only on IPv4; periodic-advertisement emitter not yet ported
  (we only listen for them when `MulticastListen` is set)
- **`D_KEYEXPR` send side** — today we resolve inbound aliases but never
  declare our own; a send-side implementation shrinks outbound messages
  by letting the router reuse a scope id for a long key expression
- **`CancellationToken` wiring on other operations** — currently only
  `GetOptions.WithCancellation` accepts one; extend to liveliness Get /
  Querier Get for symmetry with zenoh-rust
- **`SourceInfo` extension** (behind unstable)
- **Per-lane traffic counters** — cumulative byte / message / drop counters
  on top of the existing `Session.LinkInfo` snapshot
- **TLS**, **QUIC**, **WebSocket**, **Unix-socket**, **Serial** transports
  (dialer stubs are already registered; they return scheme-not-supported
  errors until a real implementation lands)
- **Auth extension** (username/password, public-key)
- **LowLatency mode** (INIT extension 0x5)
- **Compression extension** (0x6)
- **Peer role** + **multicast JOIN**
- **Patch ≥ 1 wire features** (`BlockFirst`, FRAGMENT First/Drop markers)
- Port the remaining upstream examples (`z_pub_thr`, `z_ping`, `z_pong`, …)
- Benchmark suite
- expvar / Prometheus metrics hooks
- Router-side entity-level ACL support

## Running the interop suite

```sh
make interop-up    # build the Python test image + start zenohd
make interop-test  # go test -race -tags interop ./tests/interop/...
make interop-down
```

The Scout test uses a host-networked zenohd (Linux only) so multicast
traffic reaches the Go process running on the host:

```sh
make interop-multicast-up
make interop-multicast-test
make interop-multicast-down
```

## License

[Apache License 2.0](LICENSE) — same as upstream Eclipse Zenoh.
