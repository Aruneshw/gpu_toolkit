package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"meshcompute/internal/backend"
	"meshcompute/internal/config"
	"meshcompute/internal/security"
	"meshcompute/internal/tunnel"
	"net/http"
	"sort"
	"strings"
	"time"
)

func (a *Agent) verifyWorker(cert *x509.Certificate) (string, error) {
	return security.VerifyWorkerCertificate(a.dir, cert)
}

func (a *Agent) connectWorker(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.Header.Get("Upgrade") != "meshcompute/1" || !strings.EqualFold(r.Header.Get("Connection"), "Upgrade") || r.ContentLength != 0 || len(r.TransferEncoding) > 0 {
		http.Error(w, "invalid tunnel request", 400)
		return
	}
	id, err := a.verifyWorker(r.TLS.PeerCertificates[0])
	if err != nil {
		http.Error(w, "worker denied", 403)
		return
	}
	a.mu.Lock()
	if len(a.known) >= 64 && a.known[id] == nil {
		a.mu.Unlock()
		http.Error(w, "group capacity reached", 503)
		return
	}
	a.known[id] = r.TLS.PeerCertificates[0]
	a.mu.Unlock()
	h, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "HTTP/1.1 required", 400)
		return
	}
	conn, rw, err := h.Hijack()
	if err != nil {
		return
	}
	tc, ok := conn.(*tls.Conn)
	if !ok || rw.Reader.Buffered() != 0 {
		conn.Close()
		return
	}
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: meshcompute/1\r\n\r\n"); err == nil {
		err = rw.Flush()
	}
	if err != nil {
		conn.Close()
		return
	}
	conn.SetDeadline(time.Time{})
	_, _ = a.broker.RegisterTLS(r.Context(), tc, a.verifyWorker)
}

func (a *Agent) listWorkers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	peers := []config.Peer{}
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, cert := range a.known {
		if _, err := a.verifyWorker(cert); err != nil {
			delete(a.known, id)
			delete(a.workerCache, id)
			a.broker.Disconnect(id)
			continue
		}
		peers = append(peers, config.Peer{Name: id, Endpoint: a.c.LocalEndpoint() + "/v1/workers/" + id})
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].Name < peers[j].Name })
	encode(w, peers)
}

// Only fixed resource/inference operations can be forwarded. No arbitrary
// URL, enrollment, shell, or recursive relay request can cross this route.
func allowedOperation(path, method string) bool {
	switch path {
	case "/v1/status", "/v1/ping", "/v1/probe":
		return method == http.MethodGet
	case "/v1/run", "/v1/run/stream":
		return method == http.MethodPost
	}
	return false
}

func (a *Agent) proxyWorker(w http.ResponseWriter, r *http.Request) {
	id, path, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v1/workers/"), "/")
	if !ok || len(id) != 71 || !strings.HasPrefix(id, "worker-") {
		http.NotFound(w, r)
		return
	}
	if _, err := hex.DecodeString(id[7:]); err != nil {
		http.NotFound(w, r)
		return
	}
	path = "/" + path
	if !allowedOperation(path, r.Method) || r.URL.RawQuery != "" {
		http.NotFound(w, r)
		return
	}
	if path == "/v1/status" && a.broker.Leased(id) {
		a.mu.Lock()
		cached, ok := a.workerCache[id]
		cert := a.known[id]
		a.mu.Unlock()
		if ok {
			if _, err := a.verifyWorker(cert); err == nil {
				cached.Busy = true
				encode(w, cached)
				return
			}
		}
	}
	request := r.Clone(r.Context())
	request.URL.Path = path
	request.Header = make(http.Header)
	request.Header.Set("Content-Type", "application/json")
	request.Body = http.MaxBytesReader(w, r.Body, backend.MaxPromptBytes+4096)
	defer request.Body.Close()
	resp, err := a.broker.RoundTrip(request, id, a.verifyWorker)
	if err != nil {
		http.Error(w, "worker disconnected, busy, or unauthorized; request not retried", 503)
		return
	}
	defer resp.Body.Close()
	if path == "/v1/status" && resp.StatusCode == http.StatusOK {
		b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var st Status
		if err != nil || json.Unmarshal(b, &st) != nil || st.NodeID != id {
			http.Error(w, "invalid worker status", 502)
			return
		}
		a.mu.Lock()
		a.workerCache[id] = st
		a.mu.Unlock()
		encode(w, st)
		return
	}
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	// Limit total output even if the worker is compromised. Flush each chunk so
	// SSE tokens arrive immediately; cancellation closes the leased socket.
	body := io.LimitReader(resp.Body, backend.MaxResponseBytes+1)
	buf := make([]byte, 16<<10)
	var total int64
	for {
		n, err := body.Read(buf)
		if n > 0 {
			total += int64(n)
			if total > backend.MaxResponseBytes {
				return
			}
			if rc.SetWriteDeadline(time.Now().Add(15*time.Second)) != nil {
				return
			}
			if _, e := w.Write(buf[:n]); e != nil {
				return
			}
			if rc.Flush() != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (a *Agent) connectLoop(ctx context.Context, endpoint string) {
	delay := 250 * time.Millisecond
	cfg, err := security.WorkerTLS(a.dir)
	if err != nil {
		a.log.Print("event=worker_connection error=identity_unavailable")
		return
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowedOperation(r.URL.Path, r.Method) {
			http.NotFound(w, r)
			return
		}
		a.Handler().ServeHTTP(w, r)
	})
	for ctx.Err() == nil {
		start := time.Now()
		err = tunnel.ServeWorker(ctx, endpoint, cfg, handler)
		if ctx.Err() != nil {
			return
		}
		if err == nil || time.Since(start) > 10*time.Second {
			delay = 250 * time.Millisecond
		} else if delay < 5*time.Second {
			delay *= 2
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			a.log.Print("event=worker_connection state=reconnecting")
		}
		// Replenish a consumed connection promptly. Failed handshakes back off.
		pause := time.Duration(rand.IntN(50)) * time.Millisecond
		if err != nil {
			pause += delay
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
