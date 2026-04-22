package zenoh

import (
	"strconv"
	"strings"

	"github.com/shirou/zenoh-go-client/internal/wire"
)

// Encoding describes how a payload should be interpreted.
//
// Built-in encodings cover the zenoh-rust ID space (see the constants
// below); callers can also specify a free-form schema string (e.g. a MIME
// parameter like "utf-8").
type Encoding struct {
	id     uint32
	schema string
}

// NewEncodingDefault returns the "no encoding" default (ZENOH_BYTES, id=0).
func NewEncodingDefault() Encoding { return Encoding{} }

// NewEncodingFromString parses a zenoh-encoding string of the form
// "<prefix>[;schema]". The prefix is matched against the predefined
// catalogue (e.g. "text/plain"); an unknown prefix becomes the default
// encoding with the full string stored as the schema. The result never
// returns an error — parse is lossless (round-trips through String()).
func NewEncodingFromString(s string) Encoding {
	prefix, schema, _ := strings.Cut(s, schemaSep)
	if id, ok := encodingPrefixToID[prefix]; ok {
		return Encoding{id: id, schema: schema}
	}
	return Encoding{id: 0, schema: s}
}

// String returns the zenoh-encoding string "<prefix>[;schema]". The
// prefix is the predefined name for id (e.g. "text/plain"); for unknown
// ids the prefix is the numeric id surrounded by <>.
func (e Encoding) String() string {
	prefix, ok := encodingIDToPrefix[e.id]
	if !ok {
		prefix = fmtUnknownEncoding(e.id)
	}
	if e.schema == "" {
		return prefix
	}
	return prefix + schemaSep + e.schema
}

// ID returns the numeric encoding identifier.
func (e Encoding) ID() uint32 { return e.id }

// Schema returns the schema string (may be empty).
func (e Encoding) Schema() string { return e.schema }

// WithSchema returns a copy of e with the schema replaced.
func (e Encoding) WithSchema(schema string) Encoding {
	e.schema = schema
	return e
}

// SetSchema is an alias for WithSchema, kept for zenoh-go parity.
func (e Encoding) SetSchema(schema string) Encoding { return e.WithSchema(schema) }

const schemaSep = ";"

// Predefined encodings. IDs mirror zenoh-rust
// (`zenoh/src/api/encoding.rs`) so the wire representation is identical.
// Adding a new constant here requires adding the same id to
// encodingIDToPrefix (the prefix→id map is derived from it at init).
//
// Treat these values as read-only. Go does not allow composite literals
// as const, so they must be declared as `var`, but reassigning one (e.g.
// `EncodingTextPlain = Encoding{id: 999}`) silently corrupts every
// subsequent default — don't do that.
var (
	// 0–2: zenoh-native
	EncodingZenohBytes      = Encoding{id: 0}
	EncodingZenohString     = Encoding{id: 1}
	EncodingZenohSerialized = Encoding{id: 2}

	// 3–15: application/* + text/* core
	EncodingApplicationOctetStream            = Encoding{id: 3}
	EncodingTextPlain                         = Encoding{id: 4}
	EncodingApplicationJson                   = Encoding{id: 5}
	EncodingTextJson                          = Encoding{id: 6}
	EncodingApplicationCdr                    = Encoding{id: 7}
	EncodingApplicationCbor                   = Encoding{id: 8}
	EncodingApplicationYaml                   = Encoding{id: 9}
	EncodingTextYaml                          = Encoding{id: 10}
	EncodingTextJson5                         = Encoding{id: 11}
	EncodingApplicationPythonSerializedObject = Encoding{id: 12}
	EncodingApplicationProtobuf               = Encoding{id: 13}
	EncodingApplicationJavaSerializedObject   = Encoding{id: 14}
	EncodingApplicationOpenmetricsText        = Encoding{id: 15}

	// 16–20: images
	EncodingImagePng  = Encoding{id: 16}
	EncodingImageJpeg = Encoding{id: 17}
	EncodingImageGif  = Encoding{id: 18}
	EncodingImageBmp  = Encoding{id: 19}
	EncodingImageWebp = Encoding{id: 20}

	// 21–37: more application/* + text/*
	EncodingApplicationXml                = Encoding{id: 21}
	EncodingApplicationXWwwFormUrlencoded = Encoding{id: 22}
	EncodingTextHtml                      = Encoding{id: 23}
	EncodingTextXml                       = Encoding{id: 24}
	EncodingTextCss                       = Encoding{id: 25}
	EncodingTextJavascript                = Encoding{id: 26}
	EncodingTextMarkdown                  = Encoding{id: 27}
	EncodingTextCsv                       = Encoding{id: 28}
	EncodingApplicationSql                = Encoding{id: 29}
	EncodingApplicationCoapPayload        = Encoding{id: 30}
	EncodingApplicationJsonPatchJson      = Encoding{id: 31}
	EncodingApplicationJsonSeq            = Encoding{id: 32}
	EncodingApplicationJsonpath           = Encoding{id: 33}
	EncodingApplicationJwt                = Encoding{id: 34}
	EncodingApplicationMp4                = Encoding{id: 35}
	EncodingApplicationSoapXml            = Encoding{id: 36}
	EncodingApplicationYang               = Encoding{id: 37}

	// 38–42: audio/*
	EncodingAudioAac    = Encoding{id: 38}
	EncodingAudioFlac   = Encoding{id: 39}
	EncodingAudioMp4    = Encoding{id: 40}
	EncodingAudioOgg    = Encoding{id: 41}
	EncodingAudioVorbis = Encoding{id: 42}

	// 43–52: video/*
	EncodingVideoH261 = Encoding{id: 43}
	EncodingVideoH263 = Encoding{id: 44}
	EncodingVideoH264 = Encoding{id: 45}
	EncodingVideoH265 = Encoding{id: 46}
	EncodingVideoH266 = Encoding{id: 47}
	EncodingVideoMp4  = Encoding{id: 48}
	EncodingVideoOgg  = Encoding{id: 49}
	EncodingVideoRaw  = Encoding{id: 50}
	EncodingVideoVp8  = Encoding{id: 51}
	EncodingVideoVp9  = Encoding{id: 52}
)

// encodingIDToPrefix is the single source of truth for id↔name mapping.
// Mirror zenoh-rust's `Encoding::ID_TO_STR`. The reverse map
// encodingPrefixToID below is derived at package init.
var encodingIDToPrefix = map[uint32]string{
	0:  "zenoh/bytes",
	1:  "zenoh/string",
	2:  "zenoh/serialized",
	3:  "application/octet-stream",
	4:  "text/plain",
	5:  "application/json",
	6:  "text/json",
	7:  "application/cdr",
	8:  "application/cbor",
	9:  "application/yaml",
	10: "text/yaml",
	11: "text/json5",
	12: "application/python-serialized-object",
	13: "application/protobuf",
	14: "application/java-serialized-object",
	15: "application/openmetrics-text",
	16: "image/png",
	17: "image/jpeg",
	18: "image/gif",
	19: "image/bmp",
	20: "image/webp",
	21: "application/xml",
	22: "application/x-www-form-urlencoded",
	23: "text/html",
	24: "text/xml",
	25: "text/css",
	26: "text/javascript",
	27: "text/markdown",
	28: "text/csv",
	29: "application/sql",
	30: "application/coap-payload",
	31: "application/json-patch+json",
	32: "application/json-seq",
	33: "application/jsonpath",
	34: "application/jwt",
	35: "application/mp4",
	36: "application/soap+xml",
	37: "application/yang",
	38: "audio/aac",
	39: "audio/flac",
	40: "audio/mp4",
	41: "audio/ogg",
	42: "audio/vorbis",
	43: "video/h261",
	44: "video/h263",
	45: "video/h264",
	46: "video/h265",
	47: "video/h266",
	48: "video/mp4",
	49: "video/ogg",
	50: "video/raw",
	51: "video/vp8",
	52: "video/vp9",
}

// encodingPrefixToID is the inverse of encodingIDToPrefix, derived at
// package init so the two never drift.
var encodingPrefixToID = func() map[string]uint32 {
	m := make(map[string]uint32, len(encodingIDToPrefix))
	for id, prefix := range encodingIDToPrefix {
		m[prefix] = id
	}
	return m
}()

// AllPredefinedEncodings returns the full catalogue of predefined encodings,
// ordered by ID. Callers must not mutate the returned slice.
func AllPredefinedEncodings() []Encoding {
	out := make([]Encoding, len(encodingIDToPrefix))
	for id := range encodingIDToPrefix {
		out[id] = Encoding{id: id}
	}
	return out
}

// fmtUnknownEncoding formats an unknown encoding id for display. Mirrors
// zenoh-rust's "<N>" convention. Called only when an id is not in the
// 53-entry catalogue, so it is not on the data-plane hot path.
func fmtUnknownEncoding(id uint32) string {
	return "<" + strconv.FormatUint(uint64(id), 10) + ">"
}

// toWire converts the public Encoding to the wire representation.
func (e Encoding) toWire() wire.Encoding {
	return wire.Encoding{ID: e.id, Schema: e.schema}
}

// encodingFromWire builds a public Encoding from the wire representation.
func encodingFromWire(w wire.Encoding) Encoding {
	return Encoding{id: w.ID, schema: w.Schema}
}
