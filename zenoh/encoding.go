package zenoh

import "github.com/shirou/zenoh-go-client/internal/wire"

// Encoding describes how a payload should be interpreted.
//
// Built-in encodings cover the zenoh-rust ID space; callers can also
// specify a free-form schema string (e.g. MIME type).
type Encoding struct {
	id     uint32
	schema string
}

// NewEncodingDefault returns the "no encoding" default (id=0, no schema).
func NewEncodingDefault() Encoding { return Encoding{} }

// NewEncodingFromString builds an Encoding with no known id and the given
// schema (treated as a free-form MIME-type-like string).
func NewEncodingFromString(schema string) Encoding {
	return Encoding{id: 0, schema: schema}
}

// String returns the schema text. For encodings without a schema it
// returns the empty string.
func (e Encoding) String() string { return e.schema }

// SetSchema updates the schema on a copy of e.
func (e Encoding) SetSchema(schema string) Encoding {
	e.schema = schema
	return e
}

// Common predefined encodings. The id values mirror those of zenoh-rust.
// This is a minimal subset; the complete catalogue (50+ entries) comes in
// later phases.
var (
	EncodingZenohBytes            = Encoding{id: 0}
	EncodingZenohString           = Encoding{id: 1}
	EncodingZenohSerialized       = Encoding{id: 2}
	EncodingApplicationOctetStream = Encoding{id: 3}
	EncodingTextPlain             = Encoding{id: 4}
	EncodingApplicationJson       = Encoding{id: 5}
	EncodingTextJson              = Encoding{id: 6}
)

// toWire converts the public Encoding to the wire representation.
func (e Encoding) toWire() wire.Encoding {
	return wire.Encoding{ID: e.id, Schema: e.schema}
}

// encodingFromWire builds a public Encoding from the wire representation.
func encodingFromWire(w wire.Encoding) Encoding {
	return Encoding{id: w.ID, schema: w.Schema}
}
