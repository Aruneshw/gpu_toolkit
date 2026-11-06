package tunnel

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// RoundTrip sends exactly one HTTP request over an outbound worker TLS
// connection. No redial or replay of a dispatched request is possible. Closing
// the response releases the lease and cancels the worker's request on failure.
func (b *Broker) RoundTrip(req *http.Request, id string, authorize func(*x509.Certificate) (string, error)) (*http.Response, error) {
	wait, cancel := context.WithTimeout(req.Context(), 5*time.Second)
	conn, release, err := b.Acquire(wait, id)
	cancel()
	if err != nil {
		return nil, err
	}
	tlsConn, ok := conn.(*tls.Conn)
	if !ok || authorize == nil {
		release()
		return nil, errors.New("authenticated worker required")
	}
	cs := tlsConn.ConnectionState()
	if len(cs.PeerCertificates) == 0 {
		release()
		return nil, errors.New("missing worker certificate")
	}
	current, err := authorize(cs.PeerCertificates[0])
	if err != nil || current != id {
		release()
		return nil, errors.New("worker authorization failed")
	}
	deadline := time.Now().Add(31 * time.Minute)
	if d, ok := req.Context().Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		release()
		return nil, err
	}
	stop := context.AfterFunc(req.Context(), release)
	var once sync.Once
	cleanup := func() { once.Do(func() { stop(); release() }) }
	tr := &http.Transport{Proxy: nil, DisableKeepAlives: true, MaxResponseHeaderBytes: 16 << 10,
		DialContext: func(context.Context, string, string) (net.Conn, error) { return conn, nil }}
	copyReq := req.Clone(req.Context())
	copyReq.URL = &url.URL{Scheme: "http", Host: "worker.internal", Path: req.URL.Path}
	copyReq.Host = "worker.internal"
	copyReq.RequestURI = ""
	copyReq.Close = true
	copyReq.GetBody = nil
	resp, err := tr.RoundTrip(copyReq)
	if err != nil {
		cleanup()
		tr.CloseIdleConnections()
		return nil, err
	}
	resp.Body = &responseBody{ReadCloser: resp.Body, close: func() { cleanup(); tr.CloseIdleConnections() }}
	return resp, nil
}

type responseBody struct {
	io.ReadCloser
	close func()
}

func (r *responseBody) Close() error { r.close(); return r.ReadCloser.Close() }

type singleListener struct {
	conn     net.Conn
	accepted bool
	done     chan struct{}
	once     sync.Once
}

func (l *singleListener) Accept() (net.Conn, error) {
	if !l.accepted {
		l.accepted = true
		return l.conn, nil
	}
	<-l.done
	return nil, net.ErrClosed
}
func (l *singleListener) Close() error {
	l.once.Do(func() { close(l.done); l.conn.Close() })
	return nil
}
func (l *singleListener) Addr() net.Addr { return l.conn.LocalAddr() }

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
	closed func()
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
func (c *bufferedConn) Close() error               { err := c.Conn.Close(); c.closed(); return err }

// ServeWorker establishes an outbound pinned TLS connection and serves one
// reverse HTTP request. The caller reconnects after it completes. Both the
// upgrade and the server have bounded headers and deadlines. handler receives
// the host's verified TLS identity, never a synthetic worker-supplied identity.
func ServeWorker(ctx context.Context, endpoint string, cfg *tls.Config, handler http.Handler) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}, Config: cfg}
	raw, err := d.DialContext(dialCtx, "tcp", u.Host)
	if err != nil {
		return err
	}
	conn := raw.(*tls.Conn)
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	request := &http.Request{Method: http.MethodPost, URL: &url.URL{Scheme: "https", Host: u.Host, Path: "/v1/tunnel"}, Host: u.Host, Header: make(http.Header)}
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "meshcompute/1")
	if err := request.Write(conn); err != nil {
		return err
	}
	// A bounded reader prevents a hostile endpoint from allocating unbounded
	// response headers. It is reset after parsing the upgrade response.
	limited := &io.LimitedReader{R: conn, N: 16 << 10}
	reader := bufio.NewReader(limited)
	resp, err := http.ReadResponse(reader, request)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Upgrade") != "meshcompute/1" {
		resp.Body.Close()
		return errors.New("group host rejected reverse connection")
	}
	limited.N = 1<<63 - 1
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return err
	}
	listener := &singleListener{done: make(chan struct{})}
	listener.conn = &bufferedConn{Conn: conn, reader: reader, closed: func() { listener.once.Do(func() { close(listener.done) }) }}
	defer listener.Close()
	cs := conn.ConnectionState()
	var served atomic.Bool
	server := &http.Server{ReadHeaderTimeout: 40 * time.Second, ReadTimeout: 50 * time.Second, WriteTimeout: 31 * time.Minute, MaxHeaderBytes: 16 << 10,
		ErrorLog: log.New(io.Discard, "", 0), BaseContext: func(net.Listener) context.Context { return ctx },
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { served.Store(true); r.TLS = &cs; handler.ServeHTTP(w, r) })}
	err = server.Serve(listener)
	server.Close()
	if !served.Load() {
		return errors.New("unused worker connection closed")
	}
	if errors.Is(err, net.ErrClosed) || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
