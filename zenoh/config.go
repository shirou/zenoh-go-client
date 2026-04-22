package zenoh

import "time"

// Config is the minimal session configuration. This MVP covers
// `connect/endpoints`, `id`, and reconnect timing; the full
// Rust-compatible schema comes in a later phase.
type Config struct {
	// Endpoints is the ordered list of locator strings (e.g.
	// "tcp/127.0.0.1:7447") to try when opening the session.
	Endpoints []string

	// ZID is the local ZenohID (1..16 bytes hex). Empty means "generate a
	// random 16-byte ID at Open time".
	ZID string

	// ReconnectInitial / ReconnectMax / ReconnectFactor control the
	// exponential backoff used when the link drops. Zero values fall
	// back to Rust-compatible defaults (1s / 4s / 2.0).
	ReconnectInitial time.Duration
	ReconnectMax     time.Duration
	ReconnectFactor  float64

	// Scouting governs SCOUT/HELLO discovery via Scout. The zero value
	// selects multicast-enabled scouting on 224.0.0.224:7446 with a 3s
	// timeout, matching zenoh-rust defaults.
	Scouting ScoutingConfig
}

// MulticastMode selects whether Scout transmits to the multicast group.
// The zero value (MulticastAuto) enables multicast on the default group.
type MulticastMode uint8

const (
	// MulticastAuto sends to MulticastAddress (or the default group when empty).
	MulticastAuto MulticastMode = iota
	// MulticastOff suppresses multicast; only UnicastAddresses are used.
	MulticastOff
)

// ScoutingConfig is the subset of scouting settings honoured by Scout.
// Field names align with the Rust `scouting/*` config keys so the same
// configuration can be shared across implementations.
type ScoutingConfig struct {
	// MulticastMode selects whether multicast transmission is enabled.
	MulticastMode MulticastMode

	// MulticastAddress is the "host:port" UDP target (or "udp/host:port").
	// Empty = default (224.0.0.224:7446).
	MulticastAddress string

	// Timeout is the overall Scout duration. 0 = default (3s).
	Timeout time.Duration
}

// NewConfig returns a Config with no endpoints. Caller fills in fields as
// needed.
func NewConfig() Config { return Config{} }

// WithEndpoint returns a copy of c with endpoint appended.
func (c Config) WithEndpoint(endpoint string) Config {
	out := c
	out.Endpoints = append(append([]string(nil), c.Endpoints...), endpoint)
	return out
}
