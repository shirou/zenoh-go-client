package codec

import (
	"bytes"
	"testing"
)

// FuzzDecodeZ8 / Z16 / Z32 sit next to the existing FuzzDecodeZ64 in
// vle_test.go; each variant has tighter byte budgets (2 / 3 / 5) and
// overflow semantics the fuzzer must exercise.

func FuzzDecodeZ8(f *testing.F) {
	for _, v := range []uint64{0, 1, 127, 128, 255} {
		w := NewWriter(2)
		w.EncodeZ64(v)
		f.Add(w.Bytes())
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		r := NewReader(input)
		_, _ = r.DecodeZ8()
	})
}

func FuzzDecodeZ16(f *testing.F) {
	for _, v := range []uint64{0, 127, 128, 16383, 65535} {
		w := NewWriter(3)
		w.EncodeZ64(v)
		f.Add(w.Bytes())
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		r := NewReader(input)
		_, _ = r.DecodeZ16()
	})
}

func FuzzDecodeZ32(f *testing.F) {
	for _, v := range []uint64{0, 1 << 14, 1 << 28, 0xFFFFFFFF} {
		w := NewWriter(5)
		w.EncodeZ64(v)
		f.Add(w.Bytes())
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		r := NewReader(input)
		_, _ = r.DecodeZ32()
	})
}

// FuzzDecodeExtensions exercises the extension-chain decoder. The chain
// walks until it sees an extension with Z==0, so a malformed "Z keeps
// being set" input must terminate gracefully rather than spinning.
func FuzzDecodeExtensions(f *testing.F) {
	seeds := [][]Extension{
		{NewZ64Ext(0x1, true, 7)},
		{NewZ64Ext(0x1, false, 1), NewZ64Ext(0x2, true, 2)},
		{},
	}
	for _, exts := range seeds {
		w := NewWriter(16)
		if err := w.EncodeExtensions(exts); err != nil {
			f.Fatalf("seed encode: %v", err)
		}
		f.Add(w.Bytes())
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		r := NewReader(input)
		_, _ = r.DecodeExtensions()
	})
}

// FuzzDecodeBytesZ32 / FuzzDecodeStringZ16 exercise the length-prefixed
// buffer/string decoders. A short length prefix must not over-read past
// input bounds.

func FuzzDecodeBytesZ32(f *testing.F) {
	for _, body := range [][]byte{nil, {}, {0x01}, bytes.Repeat([]byte("x"), 128)} {
		w := NewWriter(256)
		if err := w.EncodeBytesZ32(body); err != nil {
			f.Fatalf("seed encode: %v", err)
		}
		f.Add(w.Bytes())
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		r := NewReader(input)
		_, _ = r.DecodeBytesZ32()
	})
}

func FuzzDecodeStringZ16(f *testing.F) {
	for _, s := range []string{"", "demo", "a/b/c/**", "日本語"} {
		w := NewWriter(64)
		if err := w.EncodeStringZ16(s); err != nil {
			f.Fatalf("seed encode: %v", err)
		}
		f.Add(w.Bytes())
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		r := NewReader(input)
		_, _ = r.DecodeStringZ16()
	})
}

// FuzzReadStreamBatch: the TCP stream framing. A bad length prefix must
// not cause an over-read or panic.
func FuzzReadStreamBatch(f *testing.F) {
	for _, body := range [][]byte{nil, {0x05}, bytes.Repeat([]byte{0xAB}, 64)} {
		var buf bytes.Buffer
		if err := EncodeStreamBatch(&buf, body); err != nil {
			f.Fatalf("seed encode: %v", err)
		}
		f.Add(buf.Bytes())
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		readBuf := make([]byte, MaxBatchSize)
		_, _ = ReadStreamBatch(bytes.NewReader(input), readBuf)
	})
}
