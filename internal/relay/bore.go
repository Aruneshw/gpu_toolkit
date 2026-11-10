// Package relay exposes a local TCP port through a public "bore" relay
// (https://github.com/ekzhang/bore), so a group host is reachable from any
// network without IPv6, port forwarding or firewall changes.
//
// Only TCP bytes are relayed. MeshCompute's own TLS 1.3 mutual authentication
// with an exact certificate pin runs end to end through the relay, so the
// relay operator sees ciphertext and connection metadata only.
//
// Wire protocol (bore v0.5): JSON messages terminated by a NUL byte on TCP
// port 7835. Client: {"Hello":port} -> server: {"Hello":port} | {"Error":".."}.
// The server then sends "Heartbeat" about every 500 ms and
// {"Connection":"<uuid>"} for each visitor; the client opens a new connection
// to port 7835, sends {"Accept":"<uuid>"} and from then on copies raw bytes.
package relay

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ControlPort is the bore server's fixed control port.
const ControlPort = 7835

const (
	maxFrame      = 256
	dialTimeout   = 10 * time.Second
	helloTimeout  = 10 * time.Second
	silenceLimit  = 20 * time.Second // heartbeats arrive about every 500 ms
	maxConcurrent = 256
)

// ErrPortInUse means the relay refused the requested public port.
var ErrPortInUse = errors.New("relay port already in use")

// Tunnel forwards connections arriving at Server:Port to Local.
type Tunnel struct {
	Server string // relay host name or IP, e.g. "bore.pub"
	Port   int    // requested public port; 0 asks the relay to pick one
	Local  string // local address to forward to, e.g. "127.0.0.1:7443"
	Log    *log.Logger
	// Dial is replaceable in tests.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (t *Tunnel) dial(ctx context.Context, addr string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	if t.Dial != nil {
		return t.Dial(ctx, "tcp", addr)
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

func (t *Tunnel) logf(format string, v ...any) {
	if t.Log != nil {
		t.Log.Printf(format, v...)
	}
}

func (t *Tunnel) control() string {
	return net.JoinHostPort(t.Server, strconv.Itoa(ControlPort))
}

func send(c net.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_ = c.SetWriteDeadline(time.Now().Add(dialTimeout))
	_, err = c.Write(append(b, 0))
	_ = c.SetWriteDeadline(time.Time{})
	return err
}

// message is one decoded server frame.
type message struct {
	kind  string // Hello, Heartbeat, Connection, Error, Challenge
	port  int
	value string
}

func recv(r *bufio.Reader) (message, error) {
	frame, err := r.ReadSlice(0)
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return message{}, errors.New("relay frame too large")
		}
		return message{}, err
	}
	frame = frame[:len(frame)-1]
	var unit string
	if json.Unmarshal(frame, &unit) == nil {
		return message{kind: unit}, nil
	}
	var tagged map[string]json.RawMessage
	if err := json.Unmarshal(frame, &tagged); err != nil || len(tagged) != 1 {
		return message{}, fmt.Errorf("unexpected relay message %q", frame)
	}
	for k, raw := range tagged {
		m := message{kind: k}
		if k == "Hello" {
			if err := json.Unmarshal(raw, &m.port); err != nil {
				return message{}, errors.New("invalid relay hello")
			}
		} else if err := json.Unmarshal(raw, &m.value); err != nil {
			return message{}, fmt.Errorf("invalid relay %s message", k)
		}
		return m, nil
	}
	return message{}, errors.New("empty relay message")
}

// Session is one live control connection holding a public port.
type Session struct {
	Port int
	t    *Tunnel
	conn net.Conn
	r    *bufio.Reader
}

// Open registers with the relay and returns the granted public port.
func (t *Tunnel) Open(ctx context.Context) (*Session, error) {
	if t.Port < 0 || t.Port > 65535 {
		return nil, errors.New("relay port must be 0-65535")
	}
	c, err := t.dial(ctx, t.control())
	if err != nil {
		return nil, fmt.Errorf("cannot reach relay %s: %w", t.control(), err)
	}
	r := bufio.NewReaderSize(c, maxFrame)
	if err := send(c, map[string]int{"Hello": t.Port}); err != nil {
		c.Close()
		return nil, err
	}
	_ = c.SetReadDeadline(time.Now().Add(helloTimeout))
	m, err := recv(r)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("relay did not answer: %w", err)
	}
	switch m.kind {
	case "Hello":
		if m.port < 1 || m.port > 65535 || (t.Port != 0 && m.port != t.Port) {
			c.Close()
			return nil, fmt.Errorf("relay granted unexpected port %d", m.port)
		}
		_ = c.SetReadDeadline(time.Time{})
		return &Session{Port: m.port, t: t, conn: c, r: r}, nil
	case "Error":
		c.Close()
		if strings.Contains(strings.ToLower(m.value), "in use") {
			return nil, ErrPortInUse
		}
		return nil, fmt.Errorf("relay refused: %s", m.value)
	case "Challenge":
		c.Close()
		return nil, errors.New("relay requires a secret; use a public relay without authentication")
	default:
		c.Close()
		return nil, fmt.Errorf("unexpected relay reply %q", m.kind)
	}
}

// Serve forwards visitors until the control connection fails or ctx ends.
func (s *Session) Serve(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() { s.conn.Close() })
	defer stop()
	defer s.conn.Close()
	slots := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		_ = s.conn.SetReadDeadline(time.Now().Add(silenceLimit))
		m, err := recv(s.r)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("relay connection lost: %w", err)
		}
		switch m.kind {
		case "Heartbeat":
		case "Connection":
			id := m.value
			select {
			case slots <- struct{}{}:
			default:
				s.t.logf("event=relay_visitor state=dropped reason=too_many")
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-slots }()
				if err := s.t.accept(ctx, id); err != nil && ctx.Err() == nil {
					s.t.logf("event=relay_visitor state=failed reason=%q", err.Error())
				}
			}()
		case "Error":
			s.t.logf("event=relay_error message=%q", m.value)
		}
	}
}

// Close releases the public port.
func (s *Session) Close() error { return s.conn.Close() }

func (t *Tunnel) accept(ctx context.Context, id string) error {
	remote, err := t.dial(ctx, t.control())
	if err != nil {
		return err
	}
	defer remote.Close()
	if err := send(remote, map[string]string{"Accept": id}); err != nil {
		return err
	}
	local, err := t.dial(ctx, t.Local)
	if err != nil {
		return fmt.Errorf("local host not reachable: %w", err)
	}
	defer local.Close()
	stop := context.AfterFunc(ctx, func() { remote.Close(); local.Close() })
	defer stop()
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go pipe(local, remote)
	go pipe(remote, local)
	<-done
	<-done
	return nil
}

// Keep holds the public port for the life of ctx, reconnecting with backoff.
// ready is called with the granted port after every successful registration.
// The same port is requested again after a drop, because workers and
// invitations refer to it.
func (t *Tunnel) Keep(ctx context.Context, ready func(port int)) error {
	delay := time.Second
	for ctx.Err() == nil {
		s, err := t.Open(ctx)
		if err == nil {
			t.Port = s.Port
			delay = time.Second
			if ready != nil {
				ready(s.Port)
			}
			err = s.Serve(ctx)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		t.logf("event=relay state=reconnecting reason=%q", err.Error())
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if delay < 30*time.Second {
			delay *= 2
		}
	}
	return ctx.Err()
}
