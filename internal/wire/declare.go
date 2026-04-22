package wire

import (
	"fmt"

	"github.com/shirou/zenoh-go-client/internal/codec"
)

// WireExprExtID is the mandatory WireExpr extension ID used by U_SUBSCRIBER /
// U_QUERYABLE / U_TOKEN. Carries the key expression so routers can drop the
// entity without keeping a reverse subs→KE index.
const WireExprExtID = 0x0F

// QueryableInfoExtID is the Z64 extension attached to D_QUERYABLE to
// advertise the queryable's Complete flag and distance. Z64 value layout:
// bit 0 = Complete, bits 17:1 = distance (0 = local / intra-process).
const QueryableInfoExtID = 0x01

// Declare is the DECLARE network message (ID 0x1E). Carries exactly one
// declaration sub-message in its Body.
//
// Flags: I (interest_id present, bit 5) / reserved (bit 6) / Z (extensions).
type Declare struct {
	InterestID    uint32 // populated only when the I flag is set
	HasInterestID bool
	Extensions    []codec.Extension
	Body          DeclareBody
}

// DeclareBody is one of the declaration sub-messages.
type DeclareBody interface {
	SubID() byte
	EncodeSubTo(w *codec.Writer) error
}

// EncodeTo writes the DECLARE network message. Body must be non-nil.
func (m *Declare) EncodeTo(w *codec.Writer) error {
	if m.Body == nil {
		return fmt.Errorf("wire: DECLARE with nil body")
	}
	h := codec.Header{
		ID: IDNetworkDeclare,
		F1: m.HasInterestID,
		Z:  len(m.Extensions) > 0,
	}
	w.EncodeHeader(h)
	if m.HasInterestID {
		w.EncodeZ32(m.InterestID)
	}
	if h.Z {
		if err := w.EncodeExtensions(m.Extensions); err != nil {
			return err
		}
	}
	return m.Body.EncodeSubTo(w)
}

// DecodeDeclare reads a DECLARE network message.
func DecodeDeclare(r *codec.Reader, h codec.Header) (*Declare, error) {
	if h.ID != IDNetworkDeclare {
		return nil, fmt.Errorf("wire: expected DECLARE header, got id=%#x", h.ID)
	}
	m := &Declare{HasInterestID: h.F1}
	var err error
	if m.HasInterestID {
		if m.InterestID, err = r.DecodeZ32(); err != nil {
			return nil, err
		}
	}
	if h.Z {
		if m.Extensions, err = r.DecodeExtensions(); err != nil {
			return nil, fmt.Errorf("DECLARE extensions: %w", err)
		}
	}
	m.Body, err = decodeDeclareBody(r)
	if err != nil {
		return nil, fmt.Errorf("DECLARE body: %w", err)
	}
	return m, nil
}

func decodeDeclareBody(r *codec.Reader) (DeclareBody, error) {
	h, err := r.DecodeHeader()
	if err != nil {
		return nil, err
	}
	switch h.ID {
	case IDDeclareKeyExpr:
		return decodeDeclareKeyExpr(r, h)
	case IDUndeclareKeyExpr:
		return decodeUndeclareKeyExpr(r, h)
	case IDDeclareSubscriber, IDDeclareQueryable, IDDeclareToken:
		return decodeDeclareEntity(r, h, h.ID)
	case IDUndeclareSubscriber, IDUndeclareQueryable, IDUndeclareToken:
		return decodeUndeclareEntity(r, h, h.ID)
	case IDDeclareFinal:
		return decodeDeclareFinal(r, h)
	default:
		return nil, fmt.Errorf("unknown declaration sub-id %#x", h.ID)
	}
}

// ----- D_KEYEXPR (0x00) / U_KEYEXPR (0x01) -----

// DeclareKeyExpr announces an ExprId alias for a full key expression.
// Flags: N (suffix present, bit 5) / reserved / Z.
type DeclareKeyExpr struct {
	ExprID     uint16
	Scope      uint16
	Suffix     string // present iff N flag set
	Extensions []codec.Extension
}

func (d *DeclareKeyExpr) SubID() byte { return IDDeclareKeyExpr }

func (d *DeclareKeyExpr) EncodeSubTo(w *codec.Writer) error {
	h := codec.Header{
		ID: IDDeclareKeyExpr,
		F1: d.Suffix != "",
		Z:  len(d.Extensions) > 0,
	}
	w.EncodeHeader(h)
	w.EncodeZ16(d.ExprID)
	w.EncodeZ16(d.Scope)
	if d.Suffix != "" {
		if err := w.EncodeStringZ16(d.Suffix); err != nil {
			return err
		}
	}
	if h.Z {
		return w.EncodeExtensions(d.Extensions)
	}
	return nil
}

func decodeDeclareKeyExpr(r *codec.Reader, h codec.Header) (*DeclareKeyExpr, error) {
	d := &DeclareKeyExpr{}
	var err error
	if d.ExprID, err = r.DecodeZ16(); err != nil {
		return nil, err
	}
	if d.Scope, err = r.DecodeZ16(); err != nil {
		return nil, err
	}
	if h.F1 {
		if d.Suffix, err = r.DecodeStringZ16(); err != nil {
			return nil, err
		}
	}
	if h.Z {
		if d.Extensions, err = r.DecodeExtensions(); err != nil {
			return nil, err
		}
	}
	return d, nil
}

// UndeclareKeyExpr releases a previously declared ExprId.
type UndeclareKeyExpr struct {
	ExprID     uint16
	Extensions []codec.Extension
}

func (d *UndeclareKeyExpr) SubID() byte { return IDUndeclareKeyExpr }

func (d *UndeclareKeyExpr) EncodeSubTo(w *codec.Writer) error {
	h := codec.Header{
		ID: IDUndeclareKeyExpr,
		Z:  len(d.Extensions) > 0,
	}
	w.EncodeHeader(h)
	w.EncodeZ16(d.ExprID)
	if h.Z {
		return w.EncodeExtensions(d.Extensions)
	}
	return nil
}

func decodeUndeclareKeyExpr(r *codec.Reader, h codec.Header) (*UndeclareKeyExpr, error) {
	d := &UndeclareKeyExpr{}
	var err error
	if d.ExprID, err = r.DecodeZ16(); err != nil {
		return nil, err
	}
	if h.Z {
		if d.Extensions, err = r.DecodeExtensions(); err != nil {
			return nil, err
		}
	}
	return d, nil
}

// DeclareEntity covers D_SUBSCRIBER (0x02), D_QUERYABLE (0x04), and
// D_TOKEN (0x06) — all three share the same wire layout of
// (entity_id:z32 + WireExpr). Kind selects which sub-message ID is emitted.
type DeclareEntity struct {
	Kind       byte // IDDeclareSubscriber / Queryable / Token
	EntityID   uint32
	KeyExpr    WireExpr
	Extensions []codec.Extension
}

// NewDeclareSubscriber returns a DeclareEntity with Kind=IDDeclareSubscriber.
func NewDeclareSubscriber(id uint32, ke WireExpr) *DeclareEntity {
	return &DeclareEntity{Kind: IDDeclareSubscriber, EntityID: id, KeyExpr: ke}
}

// NewDeclareQueryable returns a DeclareEntity with Kind=IDDeclareQueryable.
func NewDeclareQueryable(id uint32, ke WireExpr) *DeclareEntity {
	return &DeclareEntity{Kind: IDDeclareQueryable, EntityID: id, KeyExpr: ke}
}

// NewDeclareToken returns a DeclareEntity with Kind=IDDeclareToken.
func NewDeclareToken(id uint32, ke WireExpr) *DeclareEntity {
	return &DeclareEntity{Kind: IDDeclareToken, EntityID: id, KeyExpr: ke}
}

func (d *DeclareEntity) SubID() byte { return d.Kind }

func (d *DeclareEntity) EncodeSubTo(w *codec.Writer) error {
	h := codec.Header{
		ID: d.Kind,
		F1: d.KeyExpr.Named(),
		F2: d.KeyExpr.Mapping,
		Z:  len(d.Extensions) > 0,
	}
	w.EncodeHeader(h)
	w.EncodeZ32(d.EntityID)
	if err := d.KeyExpr.EncodeScope(w); err != nil {
		return err
	}
	if h.Z {
		return w.EncodeExtensions(d.Extensions)
	}
	return nil
}

func decodeDeclareEntity(r *codec.Reader, h codec.Header, kind byte) (*DeclareEntity, error) {
	id, err := r.DecodeZ32()
	if err != nil {
		return nil, err
	}
	ke, err := DecodeWireExpr(r, h.F1, h.F2)
	if err != nil {
		return nil, err
	}
	d := &DeclareEntity{Kind: kind, EntityID: id, KeyExpr: ke}
	if h.Z {
		if d.Extensions, err = r.DecodeExtensions(); err != nil {
			return nil, err
		}
	}
	return d, nil
}

// ----- Undeclare entity (U_SUBSCRIBER / U_QUERYABLE / U_TOKEN) -----

// UndeclareEntity is the common layout for U_SUBSCRIBER (0x03),
// U_QUERYABLE (0x05) and U_TOKEN (0x07). The WireExpr extension at ID 0x0F
// is mandatory so routers can drop entries without maintaining a reverse
// index (spec declarations.adoc §WireExprExtension).
type UndeclareEntity struct {
	Kind       byte // one of IDUndeclareSubscriber / Queryable / Token
	EntityID   uint32
	WireExpr   WireExpr          // encoded inside extension 0x0F ZBuf payload
	Extensions []codec.Extension // other extensions (rare)
}

func (d *UndeclareEntity) SubID() byte { return d.Kind }

func (d *UndeclareEntity) EncodeSubTo(w *codec.Writer) error {
	// The WireExpr extension is mandatory, so Z=1 always.
	w.EncodeHeader(codec.Header{ID: d.Kind, Z: true})
	w.EncodeZ32(d.EntityID)

	// Emit the mandatory WireExpr extension (ID 0x0F), followed by any
	// additional extensions. We manage the More flag manually to avoid
	// allocating an intermediate slice.
	wireExprExt, err := encodeWireExprExtension(d.WireExpr)
	if err != nil {
		return err
	}
	wireExprExt.Header.More = len(d.Extensions) > 0
	if err := w.EncodeExtension(wireExprExt); err != nil {
		return err
	}
	return w.EncodeExtensions(d.Extensions)
}

// decodeUndeclareEntity parses a U_* message when the caller already has the
// sub-msg header (h) and knows the Kind.
func decodeUndeclareEntity(r *codec.Reader, h codec.Header, kind byte) (*UndeclareEntity, error) {
	d := &UndeclareEntity{Kind: kind}
	id, err := r.DecodeZ32()
	if err != nil {
		return nil, err
	}
	d.EntityID = id
	if !h.Z {
		return d, nil
	}
	exts, err := r.DecodeExtensions()
	if err != nil {
		return nil, err
	}
	for _, ext := range exts {
		if ext.Header.ID == WireExprExtID && ext.Header.Encoding == codec.ExtEncZBuf {
			ke, err := decodeWireExprExtensionBody(ext.ZBuf)
			if err != nil {
				return nil, fmt.Errorf("WireExpr extension body: %w", err)
			}
			d.WireExpr = ke
			continue
		}
		d.Extensions = append(d.Extensions, ext)
	}
	return d, nil
}

// encodeWireExprExtension builds the WireExpr extension (ID 0x0F, mandatory,
// ZBuf) carrying the key expression for U_* messages.
//
// Extension body layout (spec declarations.adoc §WireExpr Extension):
//
//	 7 6 5 4 3 2 1 0
//	+-+-+-+-+-+-+-+-+
//	|X|X|X|X|X|X|M|N|
//	+-+-+-+-+-+-+-+-+
//	% key_scope:z16 %
//	~  key_suffix   ~   if N==1: <u8;z16>
func encodeWireExprExtension(ke WireExpr) (codec.Extension, error) {
	body := codec.NewWriter(16)
	var flags byte
	if ke.Named() {
		flags |= 1 << 0 // N
	}
	if ke.Mapping {
		flags |= 1 << 1 // M
	}
	body.AppendByte(flags)
	body.EncodeZ16(ke.Scope)
	if ke.Named() {
		if err := body.EncodeStringZ16(ke.Suffix); err != nil {
			return codec.Extension{}, err
		}
	}
	return codec.Extension{
		Header: codec.ExtHeader{
			ID:        WireExprExtID,
			Encoding:  codec.ExtEncZBuf,
			Mandatory: true,
		},
		ZBuf: body.Bytes(),
	}, nil
}

func decodeWireExprExtensionBody(b []byte) (WireExpr, error) {
	r := codec.NewReader(b)
	flags, err := r.ReadByte()
	if err != nil {
		return WireExpr{}, err
	}
	named := flags&(1<<0) != 0
	mapping := flags&(1<<1) != 0
	return DecodeWireExpr(r, named, mapping)
}

// ----- D_FINAL (0x1A) -----

// DeclareFinal signals completion of an INTEREST snapshot. The interest_id
// is on the enclosing DECLARE message; D_FINAL itself carries no body.
type DeclareFinal struct {
	Extensions []codec.Extension
}

func (d *DeclareFinal) SubID() byte { return IDDeclareFinal }

func (d *DeclareFinal) EncodeSubTo(w *codec.Writer) error {
	h := codec.Header{ID: IDDeclareFinal, Z: len(d.Extensions) > 0}
	w.EncodeHeader(h)
	if h.Z {
		return w.EncodeExtensions(d.Extensions)
	}
	return nil
}

func decodeDeclareFinal(r *codec.Reader, h codec.Header) (*DeclareFinal, error) {
	d := &DeclareFinal{}
	if h.Z {
		var err error
		if d.Extensions, err = r.DecodeExtensions(); err != nil {
			return nil, err
		}
	}
	return d, nil
}
