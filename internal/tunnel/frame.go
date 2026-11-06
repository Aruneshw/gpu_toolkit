// Package tunnel contains bounded framing primitives for a future host-first
// transport. It does not itself authenticate peers or establish connections.
package tunnel

import (
	"encoding/binary"
	"errors"
	"io"
)

// MaxPayload bounds memory allocated for one frame from an untrusted peer.
const MaxPayload = 64 * 1024

// Frame carries a multiplexed stream identifier and application payload.
// Stream zero is reserved for connection-level control messages.
type Frame struct {
	StreamID uint32
	Payload  []byte
}

// WriteFrame writes one complete frame. Callers must serialize concurrent
// writes and close the connection on any error, since a partial frame may exist.
func WriteFrame(w io.Writer, f Frame) error {
	if len(f.Payload) > MaxPayload {
		return errors.New("tunnel frame exceeds maximum size")
	}
	var header [8]byte
	binary.BigEndian.PutUint32(header[:4], f.StreamID)
	binary.BigEndian.PutUint32(header[4:], uint32(len(f.Payload)))
	if err := writeAll(w, header[:]); err != nil {
		return err
	}
	return writeAll(w, f.Payload)
}

func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if n < 0 || n > len(b) {
			return errors.New("invalid write count")
		}
		b = b[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

// ReadFrame refuses oversized frames before allocating memory.
func ReadFrame(r io.Reader) (Frame, error) {
	var f Frame
	var header [8]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return f, err
	}
	size := binary.BigEndian.Uint32(header[4:])
	if size > MaxPayload {
		return f, errors.New("tunnel frame exceeds maximum size")
	}
	f.StreamID = binary.BigEndian.Uint32(header[:4])
	f.Payload = make([]byte, int(size))
	if _, err := io.ReadFull(r, f.Payload); err != nil {
		return Frame{}, err
	}
	return f, nil
}
