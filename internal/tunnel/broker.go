package tunnel

import (
	"context"
	"errors"
	"net"
	"sort"
	"sync"
)

var (
	ErrBrokerClosed = errors.New("tunnel broker closed")
	ErrBrokerFull   = errors.New("tunnel broker capacity reached")
)

// PerWorker bounds the connections one worker identity may hold at a time:
// a few idle ones plus long-lived streams (terminal, llama.cpp RPC).
const PerWorker = 32

// Broker holds a small pool of connections per authenticated worker. The
// caller MUST authenticate the connection and check revocation before
// Register; the broker is not an authentication boundary. The agent uses
// RegisterTLS. Connections are consumed once; workers reconnect to replace
// them. limit bounds the number of distinct worker identities.
type Broker struct {
	mu       sync.Mutex
	limit    int
	closed   bool
	sessions map[string][]*session
	changed  chan struct{}
}

// Workers returns a bounded snapshot, including currently leased workers.
func (b *Broker) Workers() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	ids := make([]string, 0, len(b.sessions))
	for id := range b.sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Leased reports whether the worker is connected but has no idle connection
// left, so a new request would have to wait.
func (b *Broker) Leased(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	list := b.sessions[id]
	for _, s := range list {
		if !s.leased {
			return false
		}
	}
	return len(list) > 0
}

// Connected reports whether the worker holds any connection.
func (b *Broker) Connected(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.sessions[id]) > 0
}

func (b *Broker) idle(s *session) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !s.leased
}

type session struct {
	conn   net.Conn
	leased bool
	done   chan struct{}
}

func NewBroker(limit int) (*Broker, error) {
	if limit <= 0 {
		return nil, errors.New("broker capacity must be positive")
	}
	return &Broker{limit: limit, sessions: make(map[string][]*session), changed: make(chan struct{})}, nil
}

// signal wakes waiters while the broker lock is held.
func (b *Broker) signal() {
	close(b.changed)
	b.changed = make(chan struct{})
}

// remove drops one session while the broker lock is held and reports whether
// it was still registered.
func (b *Broker) remove(identity string, s *session) bool {
	list := b.sessions[identity]
	for i, c := range list {
		if c == s {
			list = append(list[:i:i], list[i+1:]...)
			if len(list) == 0 {
				delete(b.sessions, identity)
			} else {
				b.sessions[identity] = list
			}
			close(s.done)
			b.signal()
			return true
		}
	}
	return false
}

// Register transfers ownership of conn to the broker on success.
func (b *Broker) Register(identity string, conn net.Conn) error {
	_, err := b.register(identity, conn)
	return err
}

func (b *Broker) register(identity string, conn net.Conn) (*session, error) {
	if identity == "" || conn == nil {
		return nil, errors.New("worker identity and connection required")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, ErrBrokerClosed
	}
	list := b.sessions[identity]
	if len(list) >= PerWorker {
		return nil, errors.New("worker connection limit reached")
	}
	if len(list) == 0 && len(b.sessions) >= b.limit {
		return nil, ErrBrokerFull
	}
	s := &session{conn: conn, done: make(chan struct{})}
	b.sessions[identity] = append(list, s)
	b.signal()
	return s, nil
}

// Acquire waits for an idle worker connection and leases it exclusively.
// Calling the returned release function closes the connection; it is safe to
// repeat.
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
		var s *session
		for _, c := range b.sessions[identity] {
			if !c.leased {
				s = c
				break
			}
		}
		if s != nil {
			s.leased = true
			b.mu.Unlock()
			var once sync.Once
			release := func() {
				once.Do(func() {
					b.mu.Lock()
					b.remove(identity, s)
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

// Disconnect removes and closes every idle or leased connection of a worker.
// The caller must separately prevent a revoked identity from being registered
// again.
func (b *Broker) Disconnect(identity string) {
	b.mu.Lock()
	list := b.sessions[identity]
	if len(list) > 0 {
		delete(b.sessions, identity)
		for _, s := range list {
			close(s.done)
		}
		b.signal()
	}
	b.mu.Unlock()
	for _, s := range list {
		_ = s.conn.Close()
	}
}

// disconnectSession removes only the exact session being monitored. A stale
// monitor must not disconnect a later connection for the same worker.
func (b *Broker) disconnectSession(identity string, s *session) {
	b.mu.Lock()
	ok := b.remove(identity, s)
	b.mu.Unlock()
	if ok {
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
	b.sessions = make(map[string][]*session)
	for _, list := range sessions {
		for _, s := range list {
			close(s.done)
		}
	}
	b.signal()
	b.mu.Unlock()
	for _, list := range sessions {
		for _, s := range list {
			_ = s.conn.Close()
		}
	}
	return nil
}
