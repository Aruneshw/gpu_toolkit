package tunnel

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// lease takes one idle connection of an authenticated worker and re-checks
// its certificate against current group membership.
func (b *Broker) lease(ctx context.Context, id string, authorize func(*x509.Certificate) (string, error)) (net.Conn, func(), error) {
	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	conn, release, err := b.Acquire(wait, id)
	cancel()
	if err != nil {
		return nil, nil, err
	}
	tlsConn, ok := conn.(*tls.Conn)
	if !ok || authorize == nil {
		release()
		return nil, nil, errors.New("authenticated worker required")
	}
	cs := tlsConn.ConnectionState()
	if len(cs.PeerCertificates) == 0 {
		release()
		return nil, nil, errors.New("missing worker certificate")
	}
	current, err := authorize(cs.PeerCertificates[0])
	if err != nil || current != id {
		release()
		return nil, nil, errors.New("worker authorization failed")
	}
	return conn, release, nil
}

// StreamProtocol is the Upgrade token of a raw byte stream to a worker.
const StreamProtocol = "meshcompute-stream/1"

// OpenStream turns one worker connection into a raw, bidirectional byte
// stream handled by the worker at path (for example a terminal or a pipe to
// its local llama.cpp RPC server). The stream has no deadline; closing it, a
// revocation or cancelling ctx before it is established ends it. The broker's
// per-second authorization re-check keeps applying while it is open.
func (b *Broker) OpenStream(ctx context.Context, id, path string, authorize func(*x509.Certificate) (string, error)) (net.Conn, error) {
	conn, release, err := b.lease(ctx, id, authorize)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, release)
	fail := func(err error) (net.Conn, error) { stop(); release(); return nil, err }
	reader, err := upgrade(conn, "worker.internal", path)
	if err != nil {
		return fail(err)
	}
	if !stop() {
		// ctx was cancelled while the stream was being set up.
		release()
		return nil, ctx.Err()
	}
	return &streamConn{Conn: conn, reader: reader, release: release}, nil
}

type streamConn struct {
	net.Conn
	reader  *bufio.Reader
	release func()
}

func (c *streamConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
func (c *streamConn) Close() error               { c.release(); return nil }

type detachKey struct{}

// AcceptStream is the worker side of OpenStream. It must be called from the
// handler given to ServeWorker. On success the caller owns the returned
// connection, and the worker's reconnect loop is released immediately so
// status and inference keep working while the stream stays open.
func AcceptStream(w http.ResponseWriter, r *http.Request) (net.Conn, error) {
	detach, _ := r.Context().Value(detachKey{}).(func())
	if detach == nil {
		http.Error(w, "stream upgrade required", http.StatusBadRequest)
		return nil, errors.New("not a worker tunnel request")
	}
	conn, err := HijackStream(w, r)
	if err == nil {
		detach()
	}
	return conn, err
}

// IsStream reports whether r asks to become a raw stream.
func IsStream(r *http.Request) bool {
	return r.Method == http.MethodPost && r.Header.Get("Upgrade") == StreamProtocol && strings.EqualFold(r.Header.Get("Connection"), "Upgrade")
}

// HijackStream answers a stream upgrade on an ordinary HTTP/1.1 server and
// returns the raw connection with all deadlines cleared.
func HijackStream(w http.ResponseWriter, r *http.Request) (net.Conn, error) {
	if !IsStream(r) {
		http.Error(w, "stream upgrade required", http.StatusBadRequest)
		return nil, errors.New("not a stream request")
	}
	h, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "HTTP/1.1 required", http.StatusBadRequest)
		return nil, errors.New("connection cannot be hijacked")
	}
	conn, rw, err := h.Hijack()
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + StreamProtocol + "\r\n\r\n"); err == nil {
		err = rw.Flush()
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	return &streamConn{Conn: conn, reader: rw.Reader, release: func() { conn.Close() }}, nil
}

// DialStream opens a raw stream to an agent's HTTPS endpoint at path.
func DialStream(ctx context.Context, endpoint, path string, cfg *tls.Config) (net.Conn, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	d := tls.Dialer{NetDialer: &net.Dialer{KeepAlive: 15 * time.Second}, Config: cfg}
	conn, err := d.DialContext(dialCtx, "tcp", u.Host)
	if err != nil {
		return nil, err
	}
	stream, err := upgrade(conn, u.Host, path)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return &streamConn{Conn: conn, reader: stream, release: func() { conn.Close() }}, nil
}

// upgrade performs the client half of a stream upgrade on conn.
func upgrade(conn net.Conn, host, path string) (*bufio.Reader, error) {
	if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return nil, err
	}
	req := &http.Request{Method: http.MethodPost, URL: &url.URL{Scheme: "http", Host: host, Path: path}, Host: host, Header: make(http.Header)}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", StreamProtocol)
	if err := req.Write(conn); err != nil {
		return nil, err
	}
	limited := &io.LimitedReader{R: conn, N: 16 << 10}
	reader := bufio.NewReader(limited)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Upgrade") != StreamProtocol {
		resp.Body.Close()
		return nil, fmt.Errorf("stream refused (HTTP %d)", resp.StatusCode)
	}
	limited.N = 1<<63 - 1
	return reader, conn.SetDeadline(time.Time{})
}

// Pipe copies both directions until either side ends, then closes both.
func Pipe(a, b io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
	a.Close()
	b.Close()
	<-done
}

// RoundTrip sends exactly one HTTP request over an outbound worker TLS
// connection. No redial or replay of a dispatched request is possible. Closing
// the response releases the lease and cancels the worker's request on failure.
func (b *Broker) RoundTrip(req *http.Request, id string, authorize func(*x509.Certificate) (string, error)) (*http.Response, error) {
	conn, release, err := b.lease(req.Context(), id, authorize)
	if err != nil {
		return nil, err
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
	closed := false
	l.once.Do(func() { close(l.done); closed = true })
	if closed {
		l.conn.Close()
	}
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
	return ServeWorkerNotify(ctx, endpoint, cfg, handler, nil)
}

// ServeWorkerNotify is ServeWorker with a callback invoked once the host has
// accepted the reverse connection (before any request is served).
func ServeWorkerNotify(ctx context.Context, endpoint string, cfg *tls.Config, handler http.Handler, accepted func()) error {
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
	var detached atomic.Bool
	defer func() {
		if !detached.Load() {
			conn.Close()
		}
	}()
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
	if accepted != nil {
		accepted()
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return err
	}
	listener := &singleListener{done: make(chan struct{})}
	listener.conn = &bufferedConn{Conn: conn, reader: reader, closed: func() { listener.once.Do(func() { close(listener.done) }) }}
	defer listener.Close()
	cs := conn.ConnectionState()
	var served atomic.Bool
	// A stream handler takes the connection over: stop serving without closing it.
	detach := func() { detached.Store(true); listener.once.Do(func() { close(listener.done) }) }
	server := &http.Server{ReadHeaderTimeout: 40 * time.Second, ReadTimeout: 50 * time.Second, WriteTimeout: 31 * time.Minute, MaxHeaderBytes: 16 << 10,
		ErrorLog: log.New(io.Discard, "", 0), BaseContext: func(net.Listener) context.Context { return ctx },
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			served.Store(true)
			r.TLS = &cs
			handler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), detachKey{}, detach)))
		})}
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
