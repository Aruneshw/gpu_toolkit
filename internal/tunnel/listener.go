package tunnel

import (
	"net"
	"sync"
)

// LimitListener bounds all live TCP connections, including unauthenticated
// TLS handshakes and hijacked reverse connections. Excess sockets are closed.
func LimitListener(l net.Listener, limit int) net.Listener {
	return &limitedListener{Listener: l, slots: make(chan struct{}, limit)}
}

type limitedListener struct {
	net.Listener
	slots chan struct{}
}

func (l *limitedListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.slots <- struct{}{}:
			return &limitedConn{Conn: c, release: func() { <-l.slots }}, nil
		default:
			c.Close()
		}
	}
}

type limitedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitedConn) Close() error { err := c.Conn.Close(); c.once.Do(c.release); return err }
