package zenoh

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// testdata/config/*.json5 is the source of truth for happy-path fixtures.
// Keeping the JSON in files rather than inline makes it easy to paste in
// real zenohd configs and diff them against the expected Config.
const configFixtureDir = "testdata/config"

type fixtureCase struct {
	name string
	file string
	want Config
}

func fixtureCases() []fixtureCase {
	return []fixtureCase{
		{
			name: "minimal",
			file: "minimal.json5",
			want: Config{
				Endpoints: []string{"tcp/127.0.0.1:7447"},
			},
		},
		{
			name: "full",
			file: "full.json5",
			want: Config{
				Endpoints: []string{
					"tcp/127.0.0.1:7447",
					"tcp/[::1]:7447",
					"tcp/router.example:7447",
				},
				ZID:              "0123456789abcdef",
				ReconnectInitial: 250 * time.Millisecond,
				ReconnectMax:     8 * time.Second,
				ReconnectFactor:  1.5,
				Scouting: ScoutingConfig{
					MulticastMode:      MulticastAuto,
					MulticastAddress:   "224.0.0.224:7446",
					MulticastInterface: "eth0",
					MulticastTTL:       4,
					MulticastListen:    true,
					Timeout:            2500 * time.Millisecond,
				},
			},
		},
		{
			name: "rust_compatible",
			file: "rust_compatible.json5",
			want: Config{
				Endpoints:        []string{"tcp/127.0.0.1:7447"},
				ReconnectInitial: time.Second,
				ReconnectMax:     4 * time.Second,
				ReconnectFactor:  2.0,
				Scouting: ScoutingConfig{
					MulticastMode:    MulticastAuto,
					MulticastAddress: "224.0.0.224:7446",
					// "auto" collapses to the empty string
					MulticastInterface: "",
					MulticastTTL:       1,
					// listen is an object form we don't parse yet: ignored
					MulticastListen: false,
					Timeout:         3 * time.Second,
				},
			},
		},
		{
			name: "scouting_only",
			file: "scouting_only.json5",
			want: Config{
				Scouting: ScoutingConfig{
					MulticastMode:    MulticastOff,
					MulticastAddress: "[ff02::224]:7446",
					Timeout:          5 * time.Second,
				},
			},
		},
		{
			name: "comments_only",
			file: "comments_only.json5",
			want: Config{},
		},
		{
			name: "json5_features",
			file: "json5_features.json5",
			want: Config{
				Endpoints: []string{
					"tcp/127.0.0.1:7447",
					"tcp/[::1]:7447",
				},
				ZID:              "cafe", // hex-escape decoded
				ReconnectInitial: 500 * time.Millisecond,
				ReconnectMax:     10 * time.Second,
				ReconnectFactor:  0.5,
				Scouting: ScoutingConfig{
					MulticastMode: MulticastAuto,
					// line-continuation stripped the backslash+newline
					MulticastAddress:   "224.0.0.224:7446",
					MulticastInterface: "", // "auto" → unset
					MulticastTTL:       2,
					MulticastListen:    false,
					Timeout:            3 * time.Second,
				},
			},
		},
	}
}

func loadFixture(t *testing.T, file string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(configFixtureDir, file))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestNewConfigFromStringFixtures(t *testing.T) {
	for _, tc := range fixtureCases() {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewConfigFromString(loadFixture(t, tc.file))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s mismatch\n got: %#v\nwant: %#v", tc.file, got, tc.want)
			}
		})
	}
}

func TestNewConfigFromFileFixtures(t *testing.T) {
	// Same content, reached via the file-path entry point. Keeps both APIs
	// on the same golden data so they can't drift apart.
	for _, tc := range fixtureCases() {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewConfigFromFile(filepath.Join(configFixtureDir, tc.file))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s mismatch\n got: %#v\nwant: %#v", tc.file, got, tc.want)
			}
		})
	}
}

func TestNewConfigFromFileMissing(t *testing.T) {
	_, err := NewConfigFromFile(filepath.Join(configFixtureDir, "does-not-exist.json5"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestNewConfigFromStringInvalidFixtures(t *testing.T) {
	cases := []string{
		"invalid/malformed.json5",
		"invalid/bad_mode.json5",
		"invalid/wrong_type.json5",
		"invalid/non_object_root.json5",
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NewConfigFromString(loadFixture(t, name))
			if err == nil {
				t.Errorf("expected error for %s", name)
			}
		})
	}
}

func TestNewConfigFromStringRejectsBadTypes(t *testing.T) {
	// Inline cases exercise the type-coercion helpers directly without a
	// fixture file per line. They complement invalid/wrong_type.json5.
	cases := map[string]string{
		"endpoints-as-string":   `{ connect: { endpoints: "not-an-array" } }`,
		"endpoint-element-non-string": `{ connect: { endpoints: [1, 2] } }`,
		"ttl-as-string":         `{ scouting: { multicast: { ttl: "nope" } } }`,
		"ttl-negative":          `{ scouting: { multicast: { ttl: -1 } } }`,
		"ttl-non-integer-float": `{ scouting: { multicast: { ttl: 1.5 } } }`,
		"enabled-as-number":     `{ scouting: { multicast: { enabled: 1 } } }`,
		"period-init-as-string": `{ connect: { retry: { period_init_ms: "1s" } } }`,
		"factor-as-string":      `{ connect: { retry: { period_increase_factor: "2x" } } }`,
		"scouting-as-scalar":    `{ scouting: 42 }`,
		"multicast-as-scalar":   `{ scouting: { multicast: true } }`,
		"retry-as-scalar":       `{ connect: { retry: 1000 } }`,
		"id-as-number":          `{ id: 42 }`,
		"mode-as-number":        `{ mode: 1 }`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewConfigFromString(src); err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestInsertJSON5(t *testing.T) {
	// Build a full config from single InsertJSON5 calls and check that the
	// result matches the fixture-based "full" config.
	var c Config
	mustInsert(t, &c, "mode", `"client"`)
	mustInsert(t, &c, "id", `"0123456789abcdef"`)
	mustInsert(t, &c, "connect/endpoints",
		`["tcp/127.0.0.1:7447", "tcp/[::1]:7447", "tcp/router.example:7447"]`)
	mustInsert(t, &c, "connect/retry/period_init_ms", `250`)
	mustInsert(t, &c, "connect/retry/period_max_ms", `8000`)
	mustInsert(t, &c, "connect/retry/period_increase_factor", `1.5`)
	mustInsert(t, &c, "scouting/timeout", `2500`)
	mustInsert(t, &c, "scouting/multicast/enabled", `true`)
	mustInsert(t, &c, "scouting/multicast/address", `"224.0.0.224:7446"`)
	mustInsert(t, &c, "scouting/multicast/interface", `"eth0"`)
	mustInsert(t, &c, "scouting/multicast/ttl", `4`)
	mustInsert(t, &c, "scouting/multicast/listen", `true`)

	want := fixtureCases()[1].want // "full"
	if !reflect.DeepEqual(c, want) {
		t.Errorf("InsertJSON5-built config mismatch\n got: %#v\nwant: %#v", c, want)
	}
}

func TestInsertJSON5ObjectValue(t *testing.T) {
	// Writing an entire subtree at once must traverse the walker and land
	// all recognised leaves.
	var c Config
	if err := c.InsertJSON5("scouting/multicast",
		`{ enabled: false, address: "224.0.0.1:7446", ttl: 8 }`); err != nil {
		t.Fatal(err)
	}
	if c.Scouting.MulticastMode != MulticastOff {
		t.Errorf("MulticastMode = %v", c.Scouting.MulticastMode)
	}
	if c.Scouting.MulticastAddress != "224.0.0.1:7446" {
		t.Errorf("MulticastAddress = %q", c.Scouting.MulticastAddress)
	}
	if c.Scouting.MulticastTTL != 8 {
		t.Errorf("MulticastTTL = %d", c.Scouting.MulticastTTL)
	}
}

func TestInsertJSON5Errors(t *testing.T) {
	cases := map[string]struct{ path, value string }{
		"unknown-top":           {"nonsense", `1`},
		"unknown-nested-leaf":   {"connect/nonsense", `true`},
		"unknown-deep-leaf":     {"connect/retry/nonsense", `1`},
		"empty-path":            {"", `1`},
		"bad-json5":             {"connect/endpoints", `[`},
		"type-mismatch":         {"connect/endpoints", `42`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var cfg Config
			if err := cfg.InsertJSON5(c.path, c.value); err == nil {
				t.Errorf("expected error for path=%q value=%q", c.path, c.value)
			}
		})
	}
}

func TestInsertJSON5Repeatable(t *testing.T) {
	// Later writes overwrite earlier ones on the same key.
	var c Config
	mustInsert(t, &c, "connect/endpoints", `["tcp/a:1"]`)
	mustInsert(t, &c, "connect/endpoints", `["tcp/b:2", "tcp/c:3"]`)
	if want := []string{"tcp/b:2", "tcp/c:3"}; !reflect.DeepEqual(c.Endpoints, want) {
		t.Errorf("Endpoints = %v, want %v", c.Endpoints, want)
	}
}

func TestInsertJSON5TogglesMulticastMode(t *testing.T) {
	var c Config
	mustInsert(t, &c, "scouting/multicast/enabled", `true`)
	if c.Scouting.MulticastMode != MulticastAuto {
		t.Errorf("after enabled=true: MulticastMode = %v", c.Scouting.MulticastMode)
	}
	mustInsert(t, &c, "scouting/multicast/enabled", `false`)
	if c.Scouting.MulticastMode != MulticastOff {
		t.Errorf("after enabled=false: MulticastMode = %v", c.Scouting.MulticastMode)
	}
}

func mustInsert(t *testing.T, c *Config, path, value string) {
	t.Helper()
	if err := c.InsertJSON5(path, value); err != nil {
		t.Fatalf("InsertJSON5(%q, %q): %v", path, value, err)
	}
}

// --- JSON5 parser unit tests -------------------------------------------------

func TestParseJSON5Primitives(t *testing.T) {
	cases := map[string]any{
		`"abc"`:           "abc",
		`'abc'`:           "abc",
		`""`:              "",
		`'\n'`:            "\n",
		`"\t"`:            "\t",
		`"A"`:        "A",
		`"\x41"`:          "A",
		`"line1\` + "\n" + `line2"`: "line1line2", // line continuation
		`42`:              int64(42),
		`-7`:              int64(-7),
		`+5`:              int64(5),
		`0x10`:            int64(16),
		`0xDEADBEEF`:      int64(0xDEADBEEF),
		`3.14`:            3.14,
		`-0.5`:            -0.5,
		`.5`:              0.5,
		`5.`:              5.0,
		`1e3`:             1000.0,
		`1.5e-2`:          0.015,
		`true`:            true,
		`false`:           false,
		`null`:            nil,
		`{}`:              map[string]any{},
		`[]`:              []any(nil),
		`[1, 2, 3,]`:      []any{int64(1), int64(2), int64(3)},
		`{a: 1, b: 2,}`:   map[string]any{"a": int64(1), "b": int64(2)},
		`{"quoted": 1}`:   map[string]any{"quoted": int64(1)},
		`[[1, 2], [3]]`:   []any{[]any{int64(1), int64(2)}, []any{int64(3)}},
		`{"a": {"b": 1}}`: map[string]any{"a": map[string]any{"b": int64(1)}},
	}
	for src, want := range cases {
		t.Run(src, func(t *testing.T) {
			got, err := parseJSON5(src)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("parseJSON5(%q) = %#v, want %#v", src, got, want)
			}
		})
	}
}

func TestParseJSON5CommentVariants(t *testing.T) {
	// Comments must be accepted between every structural token.
	src := `
		// before root
		{
			/* between fields */
			a: 1, // inline after value
			b /* between key and colon */ : /* between colon and value */ 2,
			c: [
				1, // inside array
				/* between elements */
				2,
			],
			d: { /* empty object with comment */ },
		}
		// trailing
	`
	v, err := parseJSON5(src)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"a": int64(1),
		"b": int64(2),
		"c": []any{int64(1), int64(2)},
		"d": map[string]any{},
	}
	if !reflect.DeepEqual(v, want) {
		t.Errorf("parsed = %#v, want %#v", v, want)
	}
}

func TestParseJSON5DeeplyNested(t *testing.T) {
	// 16 levels of nesting exercises recursive descent without a stack limit.
	const depth = 16
	var b strings.Builder
	for range depth {
		b.WriteString("{ a: ")
	}
	b.WriteString("42")
	for range depth {
		b.WriteString(" }")
	}
	v, err := parseJSON5(b.String())
	if err != nil {
		t.Fatal(err)
	}
	for i := range depth {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("level %d not a map: %T", i, v)
		}
		v = m["a"]
	}
	if v != int64(42) {
		t.Errorf("leaf = %v, want 42", v)
	}
}

func TestParseJSON5Errors(t *testing.T) {
	bad := []string{
		``,                         // empty input
		`{`,                        // unterminated object
		`[1, 2`,                    // unterminated array
		`{"a"}`,                    // missing ':' and value
		`{a: 1 b: 2}`,              // missing ',' separator
		`{a: }`,                    // missing value
		`"unterminated`,            // unterminated string
		`'unterminated`,            // unterminated single-quoted string
		`"\z"`,                     // unknown escape
		`/* unterminated comment`,  // unterminated block comment
		`0x`,                       // empty hex
		`0xZZ`,                     // non-hex digit
		`[1, 2] extra`,             // trailing non-whitespace content
		`{,}`,                      // bare comma
		`{a: 1,, b: 2}`,            // double comma
	}
	for _, src := range bad {
		t.Run(src, func(t *testing.T) {
			if _, err := parseJSON5(src); err == nil {
				t.Errorf("parseJSON5(%q) expected error", src)
			}
		})
	}
}

func TestParseJSON5ErrorPosition(t *testing.T) {
	// Error messages must contain "line N col M" so they point at the
	// offending byte rather than just a vague "parse error".
	_, err := parseJSON5("{\n  a: 1\n  b: 2\n}")
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "line ") || !strings.Contains(msg, "col ") {
		t.Errorf("error lacks line/col: %q", msg)
	}
}
