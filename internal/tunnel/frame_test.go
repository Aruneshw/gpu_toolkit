package tunnel

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	for _, payload := range [][]byte{nil, []byte("hello"), bytes.Repeat([]byte{42}, MaxPayload)} {
		var b bytes.Buffer
		if err := WriteFrame(&b, Frame{StreamID: 42, Payload: payload}); err != nil {
			t.Fatal(err)
		}
		got, err := ReadFrame(&b)
		if err != nil || got.StreamID != 42 || !bytes.Equal(got.Payload, payload) {
			t.Fatalf("frame mismatch: id=%d length=%d error=%v", got.StreamID, len(got.Payload), err)
		}
	}
}

func TestFrameRejectsOversizeAndTruncation(t *testing.T) {
	var b bytes.Buffer
	if err := WriteFrame(&b, Frame{Payload: make([]byte, MaxPayload+1)}); err == nil || b.Len() != 0 {
		t.Fatal("oversized write accepted or wrote data")
	}
	var header [8]byte
	binary.BigEndian.PutUint32(header[4:], MaxPayload+1)
	if _, err := ReadFrame(bytes.NewReader(header[:])); err == nil {
		t.Fatal("oversized frame accepted")
	}
	if _, err := ReadFrame(bytes.NewReader(header[:3])); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated header: %v", err)
	}
	binary.BigEndian.PutUint32(header[4:], 2)
	if _, err := ReadFrame(bytes.NewReader(append(header[:], 1))); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated payload: %v", err)
	}
}

type zeroWriter struct{}

func (zeroWriter) Write([]byte) (int, error) { return 0, nil }

func TestFrameShortWrite(t *testing.T) {
	if err := WriteFrame(zeroWriter{}, Frame{}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("expected short write, got %v", err)
	}
}
