// Package locator parses zenoh locator strings of the form
// "<scheme>/<address>[?metadata]".
//
// All schemes defined by zenoh are accepted by the parser from day 1 so that
// configuration files stay compatible when new transport backends are added.
// Whether a given scheme can actually dial is determined by whether a
// matching transport.Dialer is registered.
package locator

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Scheme identifies the transport type of a locator.
type Scheme string

// Known schemes. More can be added as transports are implemented.
const (
	SchemeTCP    Scheme = "tcp"
	SchemeUDP    Scheme = "udp"
	SchemeTLS    Scheme = "tls"
	SchemeQUIC   Scheme = "quic"
	SchemeWS     Scheme = "ws"
	SchemeWSS    Scheme = "wss"
	SchemeUnix   Scheme = "unixsock-stream"
	SchemeSerial Scheme = "serial"
)

// knownSchemes is used to validate scheme strings. Unknown schemes are rejected.
var knownSchemes = map[Scheme]bool{
	SchemeTCP:    true,
	SchemeUDP:    true,
	SchemeTLS:    true,
	SchemeQUIC:   true,
	SchemeWS:     true,
	SchemeWSS:    true,
	SchemeUnix:   true,
	SchemeSerial: true,
}

// Locator is a parsed zenoh locator.
type Locator struct {
	Scheme   Scheme
	Address  string            // host:port, /path/socket, device-name, ...
	Metadata map[string]string // ?k=v;k=v (optional, parsed but not interpreted in MVP)
}

// Parse parses a locator string.
//
// Accepted formats (examples):
//
//	tcp/127.0.0.1:7447
//	tcp/[::1]:7447
//	tcp/example.com:7447
//	tls/host:7447?cert_name=foo
//	quic/host:7443?iface=en0
//	unixsock-stream//tmp/zenoh.sock
func Parse(s string) (Locator, error) {
	rest, metaStr, _ := strings.Cut(s, "?")

	scheme, address, ok := strings.Cut(rest, "/")
	if !ok {
		return Locator{}, fmt.Errorf("locator: missing '/' separator in %q", s)
	}

	sch := Scheme(scheme)
	if !knownSchemes[sch] {
		return Locator{}, fmt.Errorf("locator: unknown scheme %q", scheme)
	}
	if address == "" {
		return Locator{}, fmt.Errorf("locator: empty address in %q", s)
	}

	// Validate the address shape for network schemes.
	switch sch {
	case SchemeTCP, SchemeUDP, SchemeTLS, SchemeQUIC, SchemeWS, SchemeWSS:
		if err := validateHostPort(address); err != nil {
			return Locator{}, fmt.Errorf("locator %q: %w", s, err)
		}
	}

	return Locator{
		Scheme:   sch,
		Address:  address,
		Metadata: parseKV(metaStr),
	}, nil
}

// String reconstructs the locator canonical string (without metadata).
func (l Locator) String() string {
	return string(l.Scheme) + "/" + l.Address
}

// validateHostPort validates a "host:port" address. IPv6 literals must be
// bracketed ([::1]:7447). Host names and IPv4 literals are accepted.
func validateHostPort(addr string) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid host:port %q: %w", addr, err)
	}
	if host == "" {
		return fmt.Errorf("empty host in %q", addr)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return fmt.Errorf("invalid port %q: %w", portStr, err)
	}
	if port == 0 {
		return fmt.Errorf("port must be non-zero")
	}
	return nil
}

func parseKV(s string) map[string]string {
	if s == "" {
		return nil
	}
	out := map[string]string{}
	for pair := range strings.SplitSeq(s, ";") {
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			out[pair] = ""
			continue
		}
		out[k] = v
	}
	return out
}
