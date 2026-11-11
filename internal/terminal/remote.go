package terminal

import (
	"encoding/binary"
	"errors"
	"io"
	"meshcompute/internal/tunnel"
	"sync"
	"time"
)

// A remote shell travels over one authenticated tunnel stream as bounded
// frames. The frame's stream ID is the message type.
const (
	msgOpen   = 1 // host -> worker: cols, rows (uint16 each); must be first
	msgData   = 2 // both ways: keyboard input / shell output
	msgResize = 3 // host -> worker: cols, rows
	msgExit   = 4 // worker -> host: exit code (int32), then the stream closes
	msgPing   = 5 // both ways: keeps idle NAT and relay mappings alive
)

func size(cols, rows int) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint16(b, uint16(cols))
	binary.BigEndian.PutUint16(b[2:], uint16(rows))
	return b
}

// framer serializes frame writes on one stream.
type framer struct {
	mu sync.Mutex
	rw io.ReadWriteCloser
}

func (f *framer) send(kind uint32, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for {
		chunk := payload
		if len(chunk) > tunnel.MaxPayload {
			chunk = chunk[:tunnel.MaxPayload]
		}
		if err := tunnel.WriteFrame(f.rw, tunnel.Frame{StreamID: kind, Payload: chunk}); err != nil {
			return err
		}
		payload = payload[len(chunk):]
		if len(payload) == 0 {
			return nil
		}
	}
}

func (f *framer) keepalive(stop <-chan struct{}) {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if f.send(msgPing, nil) != nil {
				return
			}
		}
	}
}

// Serve runs on the worker: it starts one local shell for the stream and
// relays it until the shell exits or the host closes the stream, which kills
// the shell. The caller has already authenticated the host and checked that
// the worker's owner allowed remote terminals.
func (m *Manager) Serve(stream io.ReadWriteCloser) error {
	defer stream.Close()
	first, err := tunnel.ReadFrame(stream)
	if err != nil {
		return err
	}
	if first.StreamID != msgOpen || len(first.Payload) != 4 {
		return errors.New("terminal stream must start with a size")
	}
	f := &framer{rw: stream}
	sess, err := m.Open(int(binary.BigEndian.Uint16(first.Payload)), int(binary.BigEndian.Uint16(first.Payload[2:])))
	if err != nil {
		_ = f.send(msgData, []byte("\r\n"+err.Error()+"\r\n"))
		_ = f.send(msgExit, []byte{0, 0, 0, 1})
		return err
	}
	defer m.Close(sess.ID)
	detach := sess.Attach()
	defer detach()
	stop := make(chan struct{})
	defer close(stop)
	go f.keepalive(stop)
	go func() {
		// Input side: ends when the host closes the stream, then kills the shell.
		defer m.Close(sess.ID)
		for {
			in, err := tunnel.ReadFrame(stream)
			if err != nil {
				return
			}
			switch in.StreamID {
			case msgData:
				if sess.Write(in.Payload) != nil {
					return
				}
			case msgResize:
				if len(in.Payload) == 4 {
					_ = sess.Resize(int(binary.BigEndian.Uint16(in.Payload)), int(binary.BigEndian.Uint16(in.Payload[2:])))
				}
			}
		}
	}()
	offset := int64(-1)
	for {
		data, next, wake, exited := sess.Read(offset)
		offset = next
		if len(data) > 0 {
			if err := f.send(msgData, data); err != nil {
				return err
			}
		}
		if exited {
			code := make([]byte, 4)
			binary.BigEndian.PutUint32(code, uint32(int32(sess.ExitCode())))
			return f.send(msgExit, code)
		}
		<-wake
	}
}

// remote is the host's end of a shell running on a worker.
type remote struct {
	f     *framer
	stop  chan struct{}
	once  sync.Once
	code  int
	ended bool
}

// Attach runs on the group host: it registers a shell on a worker, reached
// through stream, as a session with the same scrollback and reconnect
// behaviour as a local one.
func (m *Manager) Attach(stream io.ReadWriteCloser, cols, rows int) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.room(); err != nil {
		stream.Close()
		return nil, err
	}
	r := &remote{f: &framer{rw: stream}, stop: make(chan struct{}), code: -1}
	if err := r.f.send(msgOpen, size(clamp(cols, 20, 500, 80), clamp(rows, 5, 200, 24))); err != nil {
		stream.Close()
		return nil, err
	}
	go r.f.keepalive(r.stop)
	return m.add(r)
}

func (r *remote) Read(b []byte) (int, error) {
	for {
		// A frame is at most tunnel.MaxPayload; callers pass a buffer that large.
		in, err := tunnel.ReadFrame(r.f.rw)
		if err != nil {
			return 0, err
		}
		switch in.StreamID {
		case msgData:
			if len(in.Payload) == 0 {
				continue
			}
			return copy(b, in.Payload), nil
		case msgExit:
			if len(in.Payload) == 4 {
				r.code, r.ended = int(int32(binary.BigEndian.Uint32(in.Payload))), true
			}
			return 0, io.EOF
		}
	}
}

func (r *remote) Write(b []byte) (int, error) {
	if err := r.f.send(msgData, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (r *remote) Resize(cols, rows int) error { return r.f.send(msgResize, size(cols, rows)) }
func (r *remote) Kill()                       { r.once.Do(func() { close(r.stop); r.f.rw.Close() }) }
func (r *remote) Wait() int                   { r.Kill(); return r.code }
