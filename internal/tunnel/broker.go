package tunnel

import (
	"context"
	"errors"
	"net"
	"sync"
)

var (
	ErrBrokerClosed = errors.New("tunnel broker closed")
	ErrBrokerFull   = errors.New("tunnel broker capacity reached")
)

// Broker holds at most one connection per authenticated worker. The caller
// MUST authenticate the connection and check revocation before Register; the
// broker is not an authentication boundary. It is not yet wired into the app.
// Connections are consumed once; workers must reconnect for another request.
type Broker struct {
	mu       sync.Mutex
	limit    int
	closed   bool
	sessions map[string]*session
	changed  chan struct{}
}

type session struct {
	conn   net.Conn
	leased bool
}

func NewBroker(limit int) (*Broker, error) {
	if limit <= 0 {
		return nil, errors.New("broker capacity must be positive")
	}
	return &Broker{limit: limit, sessions: make(map[string]*session), changed: make(chan struct{})}, nil
}

// signal wakes waiters while the broker lock is held.
func (b *Broker) signal() {
	close(b.changed)
	b.changed = make(chan struct{})
}

// Register transfers ownership of conn to the broker on success. Existing
// identities are rejected rather than silently replacing an active session.
func (b *Broker) Register(identity string, conn net.Conn) error {
	if identity == "" || conn == nil {
		return errors.New("worker identity and connection required")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrBrokerClosed
	}
	if _, ok := b.sessions[identity]; ok {
		return errors.New("worker already connected")
	}
	if len(b.sessions) >= b.limit {
		return ErrBrokerFull
	}
	b.sessions[identity] = &session{conn: conn}
	b.signal()
	return nil
}

// Acquire waits for a worker connection and leases it exclusively. Calling
// the returned release function closes the connection; it is safe to repeat.
func (b *Broker) Acquire(ctx context.Context, identity string) (net.Conn, func(), error) {
	if identity == "" {
		return nil, nil, errors.New("worker identity required")
	}
	for {
		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			return nil, nil, ErrBrokerClosed
		}
		if err := ctx.Err(); err != nil {
			b.mu.Unlock()
			return nil, nil, err
		}
		if s := b.sessions[identity]; s != nil && !s.leased {
			s.leased = true
			b.mu.Unlock()
			var once sync.Once
			release := func() {
				once.Do(func() {
					b.mu.Lock()
					if b.sessions[identity] == s {
						delete(b.sessions, identity)
						b.signal()
					}
					b.mu.Unlock()
					_ = s.conn.Close()
				})
			}
			return s.conn, release, nil
		}
		changed := b.changed
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-changed:
		}
	}
}

// Disconnect removes and closes an idle or leased connection. The caller
// must separately prevent a revoked identity from being registered again.
func (b *Broker) Disconnect(identity string) {
	b.mu.Lock()
	s := b.sessions[identity]
	if s != nil {
		delete(b.sessions, identity)
		b.signal()
	}
	b.mu.Unlock()
	if s != nil {
		_ = s.conn.Close()
	}
}

// Close closes all connections and wakes waiters.
func (b *Broker) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	sessions := b.sessions
	b.sessions = make(map[string]*session)
	b.signal()
	b.mu.Unlock()
	for _, s := range sessions {
		_ = s.conn.Close()
	}
	return nil
}
