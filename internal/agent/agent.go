// Package agent exposes only bounded resource and inference operations over TLS.
package agent

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"meshcompute/internal/backend"
	"meshcompute/internal/config"
	"meshcompute/internal/model"
	"meshcompute/internal/network"
	"meshcompute/internal/resource"
	"meshcompute/internal/security"
	"meshcompute/internal/sse"
	"meshcompute/internal/task"
	"meshcompute/internal/terminal"
	"meshcompute/internal/tunnel"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"time"
)

type Status struct {
	NodeID              string                `json:"node_id"`
	Resources           resource.Snapshot     `json:"resources"`
	Backend             *backend.Capabilities `json:"backend,omitempty"`
	BackendReady        bool                  `json:"backend_ready"`
	Busy                bool                  `json:"busy"`
	Model               string                `json:"model,omitempty"`
	LastOutcome         string                `json:"last_outcome,omitempty"`
	TokensPerSecond     float64               `json:"tokens_per_second,omitempty"`
	DistributedVerified bool                  `json:"distributed_verified"`
	// RPCReady: this worker lends its GPU to the host's pooled model.
	RPCReady bool `json:"rpc_ready"`
	// Terminal: this worker's owner allowed the host to open a shell on it.
	Terminal bool `json:"terminal"`
	// Tasks is this worker's task policy: ask, auto or off.
	Tasks string `json:"tasks,omitempty"`
	// Pool describes the pooled model on a group host.
	Pool *PoolStatus `json:"pool,omitempty"`
}
type Result struct {
	backend.Response
	NodeID         string `json:"node_id"`
	Model          string `json:"model"`
	Mode           string `json:"mode"`
	DeviceEvidence string `json:"device_evidence"`
}
type Agent struct {
	dir         string
	c           config.Config
	mu          sync.Mutex
	busy        bool
	model, last string
	rate        float64
	log         *log.Logger
	broker      *tunnel.Broker
	known       map[string]*x509.Certificate
	workerCache map[string]Status
	pool        *pool
	resources   resourceCache
	// Worker side.
	worker   bool
	rpc      *backend.RPCServer
	term     *terminal.Manager
	termOpen int
	tasks    *task.Worker
	link     HostLink
	dashURL  string
	streams  chan struct{}
}

func New(dir string, c config.Config, audit io.Writer) *Agent {
	a := &Agent{dir: dir, c: c, log: log.New(audit, "", log.LstdFlags|log.LUTC), streams: make(chan struct{}, maxStreams)}
	if c.GroupHost {
		a.broker, _ = tunnel.NewBroker(64)
		a.known = make(map[string]*x509.Certificate)
		a.workerCache = make(map[string]Status)
		a.pool = newPool(a)
	}
	return a
}

// begin claims the single inference slot. The caller must call end exactly
// once when the attempt finishes, whether it succeeded or failed.
func (a *Agent) begin(r backend.Request) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.busy {
		return errors.New("worker busy; retry explicitly later")
	}
	a.busy = true
	a.model = r.Model
	a.last = "running"
	return nil
}

func (a *Agent) end(outcome string, rate float64) {
	a.mu.Lock()
	a.busy = false
	a.last = outcome
	if rate > 0 {
		a.rate = rate
	}
	a.mu.Unlock()
	a.log.Printf("event=inference outcome=%s", outcome)
}

func (a *Agent) registered(name string) error {
	if _, err := model.Get(filepath.Join(a.dir, "models"), name); err != nil {
		return errors.New("model is not registered on this worker")
	}
	return nil
}

func (a *Agent) adapter() (*backend.Llama, error) {
	return backend.New(a.c.BackendURL)
}
func (a *Agent) Status(ctx context.Context) (Status, error) {
	s, e := a.resources.get(ctx, a.dir)
	if e != nil {
		return Status{}, e
	}
	id, e := security.NodeID(a.dir)
	if e != nil {
		return Status{}, e
	}
	st := Status{NodeID: id, Resources: s}
	a.mu.Lock()
	st.Busy = a.busy
	st.Model = a.model
	st.LastOutcome = a.last
	st.TokensPerSecond = a.rate
	rpc := a.rpc
	st.Terminal = a.worker && a.c.AllowTerminal && terminalSupported()
	if a.worker {
		st.Tasks = taskPolicy(a.c.TaskApproval)
	}
	a.mu.Unlock()
	if rpc != nil {
		_, st.RPCReady = rpc.Addr()
	}
	if a.pool != nil {
		pool := a.pool.Status()
		st.Pool = &pool
	}
	b, e := backend.New(a.c.BackendURL)
	if e == nil {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		caps, e := b.Health(ctx)
		if e == nil {
			st.Backend = &caps
			st.BackendReady = true
		}
	}
	return st, nil
}
func (a *Agent) Generate(ctx context.Context, r backend.Request) (result Result, err error) {
	if err = a.begin(r); err != nil {
		return Result{}, err
	}
	completed := false
	defer func() {
		if !completed {
			a.end("failed", 0)
		}
	}()
	if err = a.registered(r.Model); err != nil {
		return Result{}, err
	}
	b, e := a.adapter()
	if e != nil {
		return Result{}, e
	}
	response, e := b.Generate(ctx, r)
	if e != nil {
		return Result{}, e
	}
	id, e := security.NodeID(a.dir)
	if e != nil {
		return Result{}, e
	}
	completed = true
	a.end("completed", response.TokensPerSecond)
	mode, evidence := "single_node", "Backend node verified; CPU/GPU participation is not instrumented. No distributed execution claim."
	if a.pool != nil {
		if p := a.pool.Status(); p.State == "ready" && len(p.Workers) > 0 {
			mode, evidence = "pooled", fmt.Sprintf("llama.cpp RPC pool across this computer and %d worker(s); per-device participation is reported by llama-server, not instrumented here.", len(p.Workers))
		}
	}
	return Result{Response: response, NodeID: id, Model: r.Model, Mode: mode, DeviceEvidence: evidence}, nil
}

// GenerateStream streams one completion through emit. It holds the same single
// inference slot as Generate and releases it on every exit path.
func (a *Agent) GenerateStream(ctx context.Context, r backend.Request, emit func(backend.Chunk) error) (err error) {
	if err = a.begin(r); err != nil {
		return err
	}
	completed := false
	rate := 0.0
	defer func() {
		if !completed {
			a.end("failed", 0)
		}
	}()
	if err = a.registered(r.Model); err != nil {
		return err
	}
	b, e := a.adapter()
	if e != nil {
		return e
	}
	var final *backend.Chunk
	err = b.GenerateStream(ctx, r, func(c backend.Chunk) error {
		if c.Done {
			// Release the slot before the client sees completion so an
			// immediate status refresh cannot show a stale busy flag.
			rate = c.TokensPerSecond
			doneChunk := c
			final = &doneChunk
			return nil
		}
		return emit(c)
	})
	if err != nil {
		return err
	}
	completed = true
	a.end("completed", rate)
	if final != nil {
		return emit(*final)
	}
	return nil
}
func encode(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// inferenceRequest reads one bounded, strict JSON inference request. On failure
// it writes the HTTP error and returns false.
func inferenceRequest(w http.ResponseWriter, r *http.Request) (backend.Request, bool) {
	defer r.Body.Close()
	const limit = backend.MaxPromptBytes + 4096
	b, e := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if e != nil {
		http.Error(w, "invalid inference request", 400)
		return backend.Request{}, false
	}
	if len(b) > limit {
		// Bounded drain lets a streaming client finish and read the error
		// instead of observing a connection reset from a half-written body.
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64<<10))
		http.Error(w, "inference request too large", 400)
		return backend.Request{}, false
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	var req backend.Request
	if e := d.Decode(&req); e != nil {
		http.Error(w, "invalid inference request", 400)
		return backend.Request{}, false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		http.Error(w, "invalid trailing data", 400)
		return backend.Request{}, false
	}
	return req, true
}

// decodeSmall reads one small strict JSON object, writing the error itself.
func decodeSmall(w http.ResponseWriter, r *http.Request, v any) bool {
	defer r.Body.Close()
	d := json.NewDecoder(io.LimitReader(r.Body, 4096))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		http.Error(w, "invalid request", 400)
		return false
	}
	return true
}

func (a *Agent) Handler() http.Handler {
	api := http.NewServeMux()
	api.Handle("/v1/ping", network.Handler())
	api.Handle("/v1/probe", network.Handler())
	api.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		st, e := a.Status(r.Context())
		if e != nil {
			http.Error(w, "resource discovery unavailable", 503)
			return
		}
		encode(w, st)
	})
	api.HandleFunc("/v1/run", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		req, ok := inferenceRequest(w, r)
		if !ok {
			return
		}
		result, e := a.Generate(r.Context(), req)
		if e != nil {
			http.Error(w, "inference failed: verify registered alias, backend health, context budget, and worker availability; no retry performed", 503)
			return
		}
		encode(w, result)
	})
	api.HandleFunc("/v1/run/stream", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		req, ok := inferenceRequest(w, r)
		if !ok {
			return
		}
		sse.Prepare(w)
		sw := sse.NewWriter(w)
		err := a.GenerateStream(r.Context(), req, func(c backend.Chunk) error {
			if c.Done {
				return sw.Event("done", map[string]any{"tokens_per_second": c.TokensPerSecond})
			}
			return sw.Event("token", map[string]any{"content": c.Content})
		})
		if err != nil {
			// The slot is already released. A generic message avoids leaking
			// backend internals, and no retry is attempted.
			_ = sw.Event("error", map[string]string{"error": "inference failed: verify registered alias, backend health, context budget, and worker availability; no retry performed"})
		}
	})
	root := http.NewServeMux()
	root.Handle("/enroll", security.EnrollHandler(a.dir))
	if a.broker != nil {
		root.Handle("/v1/tunnel", security.Authorize(a.dir, http.HandlerFunc(a.acceptWorker)))
		root.Handle("/v1/workers", security.SelfOnly(a.dir, http.HandlerFunc(a.listWorkers)))
		root.Handle("/v1/workers/", security.SelfOnly(a.dir, http.HandlerFunc(a.proxyWorker)))
		root.Handle("/v1/pool", security.SelfOnly(a.dir, http.HandlerFunc(a.handlePool)))
	}
	root.Handle("/", security.Authorize(a.dir, api))
	return root
}
func (a *Agent) Serve(ctx context.Context) error {
	listener, err := net.Listen("tcp", a.c.Listen)
	if err != nil {
		// A group worker only needs its outbound connection to the host; the
		// local listener is an optional status API. Do not fail because some
		// other program (or a group host on the same PC) already uses the port.
		if host, reverse, endpoint, gerr := security.Group(a.dir); gerr == nil && reverse && !host {
			fmt.Fprintf(a.log.Writer(), "Note: local port %s is busy (%v); continuing without the local status API.\n", a.c.Listen, err)
			a.log.Print("event=agent_start transport=mutual_tls mode=outbound_only")
			go a.resources.get(ctx, a.dir) // warm up: GPU discovery can be slow
			a.connectWorker(ctx, endpoint)
			return nil
		}
		return err
	}
	return a.ServeListener(ctx, listener)
}

// ServeListener owns listener and shuts down HTTP and reverse connections on
// cancellation. It also allows end-to-end tests to allocate ports atomically.
func (a *Agent) ServeListener(ctx context.Context, listener net.Listener) error {
	defer listener.Close()
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	if e := a.c.Validate(); e != nil {
		return e
	}
	tlsConfig, e := security.ServerTLS(a.dir)
	if e != nil {
		return fmt.Errorf("identity unavailable; cluster create or join first: %w", e)
	}
	host, reverse, endpoint, e := security.Group(a.dir)
	if e != nil {
		return e
	}
	if a.c.GroupHost != (host && reverse) {
		return errors.New("group host config does not match identity; use host to create a group")
	}
	var workers sync.WaitGroup
	if reverse && !host {
		workers.Add(1)
		go func() { defer workers.Done(); a.connectWorker(ctx, endpoint) }()
	}
	defer func() { stop(); workers.Wait() }()
	if a.broker != nil {
		defer a.broker.Close()
		// Every worker holds a few idle connections plus its open streams.
		listener = tunnel.LimitListener(listener, 1024)
		workers.Add(1)
		go func() { defer workers.Done(); a.pool.run(ctx) }()
	}
	server := &http.Server{Addr: a.c.Listen, Handler: a.Handler(), TLSConfig: tlsConfig, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: backend.Timeout + 10*time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384, ErrorLog: log.New(io.Discard, "", 0)}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = server.Close()
		case <-done:
		}
	}()
	a.log.Print("event=agent_start transport=mutual_tls")
	go a.resources.get(ctx, a.dir) // warm up: GPU discovery can be slow
	e = server.ServeTLS(listener, "", "")
	if errors.Is(e, http.ErrServerClosed) {
		return nil
	}
	return e
}
