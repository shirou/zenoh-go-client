package zenoh

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Config is the session configuration. Field names align with the
// corresponding Rust `DEFAULT_CONFIG.json5` key paths so that values can be
// populated programmatically or via NewConfigFromString / InsertJSON5.
type Config struct {
	// Mode selects the session role. Zero value (ModeClient) preserves
	// pre-peer-mode behaviour. ModePeer enables incoming Listener
	// acceptance and outbound peer discovery via Scout.
	Mode SessionMode

	// Endpoints is the ordered list of locator strings (e.g.
	// "tcp/127.0.0.1:7447") to try when opening the session.
	Endpoints []string

	// ListenEndpoints, set in peer/router mode, is the ordered list of
	// locator strings the session binds for incoming connections (e.g.
	// "tcp/0.0.0.0:7447"). Ignored in client mode.
	ListenEndpoints []string

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

	// AutoconnectMask is the role bitmap consulted in peer mode when
	// Scouting receives a HELLO: discovered peers whose WhatAmI matches
	// the mask are auto-dialled. Zero value falls back to WhatDefault
	// (router|peer) at session open time.
	AutoconnectMask What
}

// SessionMode selects the local role advertised in INIT/OPEN handshakes
// and JOIN messages.
type SessionMode uint8

const (
	// ModeClient is the default. The session connects to one router or
	// peer at a time and does not accept incoming connections.
	ModeClient SessionMode = iota
	// ModePeer accepts incoming connections, dials configured endpoints,
	// and (when scouting is enabled) auto-connects to discovered peers.
	ModePeer
	// ModeRouter is reserved; the parser accepts the value but the
	// runtime currently rejects it because routing logic is not
	// implemented.
	ModeRouter
)

// String implements fmt.Stringer for SessionMode.
func (m SessionMode) String() string {
	switch m {
	case ModeClient:
		return "client"
	case ModePeer:
		return "peer"
	case ModeRouter:
		return "router"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(m))
	}
}

// AsWhatAmI converts the local SessionMode to the WhatAmI value used in
// INIT/OPEN/JOIN handshakes.
func (m SessionMode) AsWhatAmI() WhatAmI {
	switch m {
	case ModePeer:
		return WhatAmIPeer
	case ModeRouter:
		return WhatAmIRouter
	default:
		return WhatAmIClient
	}
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
	// For IPv6 use the bracketed form, e.g. "[ff02::224]:7446".
	// Empty = default (224.0.0.224:7446).
	MulticastAddress string

	// MulticastInterface names the outgoing network interface (e.g. "eth0")
	// for multicast SCOUT and group membership. Empty = OS default.
	MulticastInterface string

	// MulticastTTL sets the multicast hop limit. 0 = OS default (typically
	// 1, i.e. link-local only).
	MulticastTTL int

	// MulticastListen, when true, opens an additional socket bound to the
	// multicast group + port so periodic HELLO advertisements from other
	// peers are observed. Unicast replies to our own SCOUT are received
	// regardless of this flag.
	MulticastListen bool

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

// Rust-compatible config key paths. These mirror the names in
// zenoh-rust's `DEFAULT_CONFIG.json5`; kept here so the walker tree below
// reads like a schema map rather than a string pile.
const (
	keyMode                 = "mode"
	keyID                   = "id"
	keyConnect              = "connect"
	keyScouting             = "scouting"
	keyEndpoints            = "endpoints"
	keyRetry                = "retry"
	keyPeriodInitMs         = "period_init_ms"
	keyPeriodMaxMs          = "period_max_ms"
	keyPeriodIncreaseFactor = "period_increase_factor"
	keyTimeout              = "timeout"
	keyMulticast            = "multicast"
	keyEnabled              = "enabled"
	keyAddress              = "address"
	keyInterface            = "interface"
	keyTTL                  = "ttl"
	keyListen               = "listen"
	keyAutoconnect          = "autoconnect"
)

// NewConfigFromString parses a JSON5 config document into a Config, using
// the same key names as zenoh-rust's `DEFAULT_CONFIG.json5`. Keys the pure-Go
// client does not yet understand are silently ignored; keys with the wrong
// value type surface as an error.
//
// Currently recognised keys:
//
//	mode                                    // validated; only "client" is runnable
//	id                                      // ZenohID as lowercase hex
//	connect/endpoints                       // []string
//	connect/retry/period_init_ms            // number, ms
//	connect/retry/period_max_ms             // number, ms
//	connect/retry/period_increase_factor    // number
//	scouting/timeout                        // number, ms
//	scouting/multicast/enabled              // bool
//	scouting/multicast/address              // string
//	scouting/multicast/interface            // string ("auto" is treated as unset)
//	scouting/multicast/ttl                  // number
//	scouting/multicast/listen               // bool  (rust per-role object form is not yet supported)
func NewConfigFromString(s string) (Config, error) {
	v, err := parseJSON5(s)
	if err != nil {
		return Config{}, err
	}
	root, ok := v.(map[string]any)
	if !ok {
		return Config{}, fmt.Errorf("zenoh: config root must be a JSON object")
	}
	var c Config
	if err := applyConfig(&c, root); err != nil {
		return Config{}, err
	}
	return c, nil
}

// NewConfigFromFile loads and parses a JSON5 config file. See
// NewConfigFromString for the recognised key set.
func NewConfigFromFile(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("zenoh: read config %q: %w", path, err)
	}
	return NewConfigFromString(string(data))
}

// InsertJSON5 parses jsonValue as a JSON5 value and assigns it to the config
// key addressed by keyPath. keyPath uses "/" as the separator and mirrors the
// nested structure of the Rust config schema (e.g.
// "scouting/multicast/enabled" or "connect/endpoints"). An unknown top-level
// or nested segment returns an error so typos are caught eagerly.
func (c *Config) InsertJSON5(keyPath, jsonValue string) error {
	v, err := parseJSON5(jsonValue)
	if err != nil {
		return err
	}
	segs := strings.Split(keyPath, "/")
	if len(segs) == 0 || segs[0] == "" {
		return fmt.Errorf("zenoh: InsertJSON5: empty key path")
	}
	return applyKey(c, segs, v)
}

// applyConfig walks a parsed JSON5 root and populates c. Unknown top-level
// or nested keys are ignored so a config aimed at zenoh-rust (which defines
// many more keys than we implement) can be fed in without modification; a
// value of the wrong shape at a recognised key is always an error.
//
// InsertJSON5 follows the same walker but rejects unknown leaf keys because
// callers addressing a specific path almost always mean to type a key we
// know about; silently dropping their write would mask typos.
//
// `mode` is processed first so downstream handlers (currently autoconnect,
// which selects a per-role array) can read the resolved local role.
func applyConfig(c *Config, root map[string]any) error {
	if v, ok := root[keyMode]; ok {
		if err := applyKey(c, []string{keyMode}, v); err != nil {
			return err
		}
	}
	for k, v := range root {
		if k == keyMode {
			continue
		}
		if err := applyKey(c, []string{k}, v); err != nil && !isUnknownKey(err) {
			return err
		}
	}
	return nil
}

// unknownKeyError flags a leaf that no handler recognised. The error type is
// what lets applyConfig distinguish "forward-compat, ignore" from real value
// errors.
type unknownKeyError struct{ path string }

func (e unknownKeyError) Error() string {
	return fmt.Sprintf("zenoh: unknown config key %q", e.path)
}

func isUnknownKey(err error) bool { _, ok := err.(unknownKeyError); return ok }

// applyKey dispatches one path (e.g. ["scouting", "multicast", "enabled"])
// into the appropriate setter. The path form lets the same walker service
// both whole-document parsing and InsertJSON5's single-key writes.
func applyKey(c *Config, path []string, v any) error {
	joined := strings.Join(path, "/")
	switch path[0] {
	case keyMode:
		return leafString(path, v, joined, func(s string) error {
			switch s {
			case "", "client":
				c.Mode = ModeClient
			case "peer":
				c.Mode = ModePeer
			case "router":
				c.Mode = ModeRouter
			default:
				return fmt.Errorf("zenoh: mode %q not supported", s)
			}
			return nil
		})
	case keyID:
		return leafString(path, v, joined, func(s string) error { c.ZID = s; return nil })
	case keyConnect:
		return walk(c, path, v, connectHandlers)
	case keyListen:
		return walk(c, path, v, listenHandlers)
	case keyScouting:
		return walk(c, path, v, scoutingHandlers)
	default:
		return unknownKeyError{joined}
	}
}

// leaf is the shape every config key ends in: coerce the JSON value and
// stuff it into the Config. Each entry in connectHandlers / scoutingHandlers
// is either a leaf or a subtree (another handlers map).
type leaf func(c *Config, v any, path string) error

type subtree map[string]any // values are leaf or subtree

var listenHandlers = subtree{
	keyEndpoints: leaf(func(c *Config, v any, p string) error {
		eps, err := asStringSlice(v, p)
		if err != nil {
			return err
		}
		c.ListenEndpoints = eps
		return nil
	}),
}

var connectHandlers = subtree{
	keyEndpoints: leaf(func(c *Config, v any, p string) error {
		eps, err := asStringSlice(v, p)
		if err != nil {
			return err
		}
		c.Endpoints = eps
		return nil
	}),
	keyRetry: subtree{
		keyPeriodInitMs: leaf(func(c *Config, v any, p string) error {
			ms, err := asUint(v, p)
			if err != nil {
				return err
			}
			c.ReconnectInitial = time.Duration(ms) * time.Millisecond
			return nil
		}),
		keyPeriodMaxMs: leaf(func(c *Config, v any, p string) error {
			ms, err := asUint(v, p)
			if err != nil {
				return err
			}
			c.ReconnectMax = time.Duration(ms) * time.Millisecond
			return nil
		}),
		keyPeriodIncreaseFactor: leaf(func(c *Config, v any, p string) error {
			f, err := asFloat(v, p)
			if err != nil {
				return err
			}
			c.ReconnectFactor = f
			return nil
		}),
	},
}

var scoutingHandlers = subtree{
	keyTimeout: leaf(func(c *Config, v any, p string) error {
		ms, err := asUint(v, p)
		if err != nil {
			return err
		}
		c.Scouting.Timeout = time.Duration(ms) * time.Millisecond
		return nil
	}),
	keyMulticast: subtree{
		keyEnabled: leaf(func(c *Config, v any, p string) error {
			b, err := asBool(v, p)
			if err != nil {
				return err
			}
			if b {
				c.Scouting.MulticastMode = MulticastAuto
			} else {
				c.Scouting.MulticastMode = MulticastOff
			}
			return nil
		}),
		keyAddress: leaf(func(c *Config, v any, p string) error {
			s, err := asString(v, p)
			if err != nil {
				return err
			}
			c.Scouting.MulticastAddress = s
			return nil
		}),
		keyInterface: leaf(func(c *Config, v any, p string) error {
			s, err := asString(v, p)
			if err != nil {
				return err
			}
			if s == "auto" {
				s = ""
			}
			c.Scouting.MulticastInterface = s
			return nil
		}),
		keyTTL: leaf(func(c *Config, v any, p string) error {
			n, err := asUint(v, p)
			if err != nil {
				return err
			}
			c.Scouting.MulticastTTL = int(n)
			return nil
		}),
		keyListen: leaf(func(c *Config, v any, p string) error {
			// rust configs also encode listen as
			//   { router: true, peer: true, client: true }
			// keyed by local role. We only run in client mode today, so
			// silently ignore the object form — picking the right sub-key
			// lands with mode-aware handling.
			b, ok := v.(bool)
			if !ok {
				return nil
			}
			c.Scouting.MulticastListen = b
			return nil
		}),
		keyAutoconnect: leaf(func(c *Config, v any, p string) error {
			mask, err := parseAutoconnect(c.Mode, v, p)
			if err != nil {
				return err
			}
			c.AutoconnectMask = mask
			return nil
		}),
	},
}

// parseAutoconnect coerces the autoconnect value into a What bitmask. Two
// shapes are accepted:
//
//   - []string of role names ("router", "peer", "client") — applies to
//     every local role.
//   - {router: [...], peer: [...], client: [...]} — Rust per-role form;
//     the entry keyed by the local SessionMode is selected.
//
// An empty array is a valid "match nothing" mask. Unknown keys in the
// per-role object (e.g. typos) surface as errors.
func parseAutoconnect(localMode SessionMode, v any, p string) (What, error) {
	switch t := v.(type) {
	case []any:
		return autoconnectFromArray(t, p)
	case map[string]any:
		key := localMode.String()
		entry, ok := t[key]
		if !ok {
			return 0, nil
		}
		arr, ok := entry.([]any)
		if !ok {
			return 0, fmt.Errorf("zenoh: %s/%s: expected array, got %T", p, key, entry)
		}
		return autoconnectFromArray(arr, p+"/"+key)
	default:
		return 0, fmt.Errorf("zenoh: %s: expected array or object, got %T", p, v)
	}
}

func autoconnectFromArray(arr []any, p string) (What, error) {
	var mask What
	for i, e := range arr {
		s, ok := e.(string)
		if !ok {
			return 0, fmt.Errorf("zenoh: %s[%d]: expected string, got %T", p, i, e)
		}
		switch s {
		case "router":
			mask |= WhatRouter
		case "peer":
			mask |= WhatPeer
		case "client":
			mask |= WhatClient
		default:
			return 0, fmt.Errorf("zenoh: %s[%d]: unknown role %q", p, i, s)
		}
	}
	return mask, nil
}

// walk descends one step into the tree pointed at by path[0]. For a subtree
// with nothing left in the path, v must be an object and every child key is
// recursively walked. A leaf handler ends the descent.
func walk(c *Config, path []string, v any, tree subtree) error {
	joined := strings.Join(path, "/")
	// path[0] is the bucket name (e.g. "connect" / "scouting"). Everything
	// after is navigated through `tree`.
	node := any(tree)
	for i := 1; i < len(path); i++ {
		sub, ok := node.(subtree)
		if !ok {
			return unknownKeyError{joined}
		}
		next, ok := sub[path[i]]
		if !ok {
			return unknownKeyError{joined}
		}
		node = next
	}
	switch n := node.(type) {
	case leaf:
		return n(c, v, joined)
	case subtree:
		m, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("zenoh: %s: expected object", joined)
		}
		for k, val := range m {
			child := append(append([]string(nil), path...), k)
			if err := walk(c, child, val, tree); err != nil && !isUnknownKey(err) {
				return err
			}
		}
		return nil
	default:
		return unknownKeyError{joined}
	}
}

// leafString is the common "mode" / "id" coercion: require len(path) == 1
// and hand the string off to onValue.
func leafString(path []string, v any, joined string, onValue func(string) error) error {
	if len(path) != 1 {
		return unknownKeyError{joined}
	}
	s, err := asString(v, joined)
	if err != nil {
		return err
	}
	return onValue(s)
}

func asString(v any, path string) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("zenoh: %s: expected string, got %T", path, v)
	}
	return s, nil
}

func asBool(v any, path string) (bool, error) {
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("zenoh: %s: expected bool, got %T", path, v)
	}
	return b, nil
}

func asUint(v any, path string) (uint64, error) {
	switch n := v.(type) {
	case int64:
		if n < 0 {
			return 0, fmt.Errorf("zenoh: %s: expected non-negative integer, got %d", path, n)
		}
		return uint64(n), nil
	case float64:
		if n < 0 || n != float64(uint64(n)) {
			return 0, fmt.Errorf("zenoh: %s: expected non-negative integer, got %v", path, n)
		}
		return uint64(n), nil
	}
	return 0, fmt.Errorf("zenoh: %s: expected number, got %T", path, v)
}

func asFloat(v any, path string) (float64, error) {
	switch n := v.(type) {
	case int64:
		return float64(n), nil
	case float64:
		return n, nil
	}
	return 0, fmt.Errorf("zenoh: %s: expected number, got %T", path, v)
}

func asStringSlice(v any, path string) ([]string, error) {
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("zenoh: %s: expected array, got %T", path, v)
	}
	out := make([]string, len(arr))
	for i, e := range arr {
		s, ok := e.(string)
		if !ok {
			return nil, fmt.Errorf("zenoh: %s[%d]: expected string, got %T", path, i, e)
		}
		out[i] = s
	}
	return out, nil
}
