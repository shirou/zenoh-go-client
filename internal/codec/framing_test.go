package codec

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestStreamBatchRoundtrip(t *testing.T) {
	sizes := []int{0, 1, 127, 256, 65535}
	for _, n := range sizes {
		payload := make([]byte, n)
		for i := range payload {
			payload[i] = byte(i)
		}
		var buf bytes.Buffer
		if err := EncodeStreamBatch(&buf, payload); err != nil {
			t.Fatalf("Encode(%d): %v", n, err)
		}
		if buf.Len() != 2+n {
			t.Errorf("encoded length = %d, want %d", buf.Len(), 2+n)
		}
		readBuf := make([]byte, MaxBatchSize)
		got, err := ReadStreamBatch(&buf, readBuf)
		if err != nil {
			t.Fatalf("Read(%d): %v", n, err)
		}
		if got != n {
			t.Errorf("read length = %d, want %d", got, n)
		}
		if !bytes.Equal(readBuf[:got], payload) {
			t.Errorf("payload mismatch")
		}
	}
}

func TestStreamBatchTooLarge(t *testing.T) {
	var buf bytes.Buffer
	big := make([]byte, MaxBatchSize+1)
	if err := EncodeStreamBatch(&buf, big); !errors.Is(err, ErrBatchTooLarge) {
		t.Errorf("expected ErrBatchTooLarge, got %v", err)
	}
}

func TestReadStreamBatchEOF(t *testing.T) {
	// Empty reader: EOF from the length prefix.
	_, err := ReadStreamBatch(bytes.NewReader(nil), make([]byte, 16))
	if !errors.Is(err, io.EOF) {
		t.Errorf("expected io.EOF, got %v", err)
	}
}

func TestReadStreamBatchTruncated(t *testing.T) {
	// Length says 10, only 5 bytes follow → ErrUnexpectedEOF.
	bs := []byte{0x0A, 0x00, 1, 2, 3, 4, 5}
	_, err := ReadStreamBatch(bytes.NewReader(bs), make([]byte, 16))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("expected io.ErrUnexpectedEOF, got %v", err)
	}
}
