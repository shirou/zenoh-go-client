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
  - On reconnect, every live `Subscriber` / `Queryable` / `LivelinessToken`
    is re-declared on the fresh link — user code does not need to retry

### Key expressions

- Canonical form validation and autocanonize (`**/**` collapse, `**/*` → `*/**`)
- `Intersects` / `Includes` (classical recursive, literal-only fast path)
- `Join` / `Concat`

### Publish / Subscribe

- `Session.Put`, `Session.Delete`, `Publisher.Put`, `Publisher.Delete`
- `DeclareSubscriber` with `Handler[T]`:
  - `Closure[T]` — dedicated dispatcher goroutine per subscriber
  - `FifoChannel[T]` — bounded channel, drop-on-full
  - `RingChannel[T]` — bounded channel, drop-oldest
- Per-emission options: encoding, priority, congestion control (`Block` / `Drop`), express

### Query / Queryable

- `Session.Get` (+ `GetWithContext`) with options:
  - `ConsolidationMode` (Auto / None / Monotonic / Latest)
  - `QueryTarget` (BestMatching / All / AllComplete)
  - `Budget` (max replies)
  - `Timeout` (deadline, enforced client-side *and* advertised on the wire)
  - `Parameters` string
- `DeclareQueryable` with `Complete` option
- `Query.Reply` / `ReplyDel` / `ReplyErr`; `RESPONSE_FINAL` auto-emitted

### Liveliness

- `Session.Liveliness().DeclareToken(keyExpr, nil)` → `LivelinessToken`
- Tokens participate in auto-reconnect replay

### Context / cancellation

- `Open(ctx, cfg)` — ctx bounds the dial + handshake
- `Session.GetWithContext(ctx, keyExpr, opts)` — ctx cancel closes the reply
  channel promptly; combined with `opts.Timeout` for hard deadlines
- `CancellationToken` (behind `-tags zenoh_unstable`) — zenoh-rust-style
  cancellation handle, convertible to a context

### Testing

- 100% green under `go test -race -count=1 ./...`
- `go.uber.org/goleak` — zero goroutine leak on `Session.Close`
- Fuzz tests for every wire decoder (INIT / OPEN / FRAME / FRAGMENT / PUSH /
  REQUEST / RESPONSE / DECLARE / INTEREST / SCOUT / HELLO / VLE)
- Integration tests under [`tests/interop/`](tests/interop/) using
  `docker-compose` + `eclipse/zenoh:1.0.0` + Python `eclipse-zenoh==1.2.1` as
  the canonical counterpart — covers both directions (Go ↔ Py) for pub/sub,
  get/queryable, and liveliness

## Future work

Not yet implemented, in rough priority order:

- **Liveliness Subscriber** and **Liveliness Get** on the Go side (both need
  INTEREST send-path plumbing, see below)
- **Matching Listener** on publishers — notifications when subscribers
  appear/disappear (needs INTEREST)
- **Querier** type (`Session.DeclareQuerier` / `Querier.Get`) with INTEREST
- **Scout** — UDP multicast SCOUT / HELLO discovery (byte-level codec is
  already written; UDP transport and scout state machine are not)
- **KeyExpr aliasing** — `D_KEYEXPR` send side for smaller on-wire messages
- **Full `Encoding` predefined constant set** — currently 7 / ~50; rest will
  be ported from upstream
- **`SourceInfo` extension** (behind unstable)
- **Link / Transport info API** — inspect the current link (addr, stats)
- **Full JSON5 Config parser** — today `Config` is a flat struct; JSON5 is
  not parsed yet
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
- Client-side Get consolidation (today the router handles consolidation;
  local consolidation is a correctness/observability improvement)

## Running the interop suite

```sh
make interop-up    # build the Python test image + start zenohd
make interop-test  # go test -race -tags interop ./tests/interop/...
make interop-down
```

## License

[Apache License 2.0](LICENSE) — same as upstream Eclipse Zenoh.
