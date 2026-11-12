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
	"sort"
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
	// Pool is the first pooled model on a group host (older dashboards);
	// Pools lists all of them.
	Pool  *PoolStatus  `json:"pool,omitempty"`
	Pools []PoolStatus `json:"pools,omitempty"`
	// Sharing lists the workers able to lend a GPU (group host only).
	Sharing []string `json:"sharing,omitempty"`
	// Links is the host's measured link to each connected worker.
	Links map[string]LinkStatus `json:"links,omitempty"`
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
	pools       *poolSet
	links       *links
	meshPaths   []string   // https endpoints workers may use instead of the relay
	hostPaths   *hostPaths // worker: which address of the group host to use
	// cfgMu serializes read-modify-write of the saved configuration.
	cfgMu     sync.Mutex
	resources resourceCache
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
		a.pools = newPools(a)
		a.links = newLinks(a)
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
	st.Busy = a.busy || a.pools != nil && a.pools.inflight() > 0
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
	var pooled map[string]int
	if a.pools != nil {
		all := a.pools.Status()
		st.Pools, st.Sharing = all.Pools, all.Sharing
		st.Links = a.links.Status()
		st.Pool = a.pools.First()
		pooled = a.pools.models()
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
	// Every model a ready pool serves can be prompted on this computer.
	if len(pooled) > 0 {
		if st.Backend == nil {
			st.Backend = &backend.Capabilities{Models: []string{}}
		}
		for m, ctxSize := range pooled {
			found := false
			for _, have := range st.Backend.Models {
				found = found || have == m
			}
			if !found {
				st.Backend.Models = append(st.Backend.Models, m)
			}
			if ctxSize > st.Backend.Context {
				st.Backend.Context = ctxSize
			}
		}
		sort.Strings(st.Backend.Models)
		st.BackendReady = true
	}
	return st, nil
}

// target is where one request runs: a pool's server, or the configured
// backend through the single inference slot.
type target struct {
	endpoint string
	pool     *PoolStatus
	release  func()
}

// admissionError is a refusal before any server was asked (no free slot,
// context too large). Its message is safe to show the caller.
type admissionError struct{ error }

// Reason returns the message to show for err: an admission refusal as is,
// anything else as a generic failure that does not leak backend internals.
func Reason(err error) string {
	var a admissionError
	if errors.As(err, &a) {
		return a.Error()
	}
	return "inference failed: verify registered alias, backend health, context budget, and worker availability; no retry performed"
}

// acquire reserves a place to run r: a request slot of a ready pool that
// serves r.Model, otherwise the single slot of the configured backend.
func (a *Agent) acquire(r backend.Request) (target, error) {
	if a.pools != nil {
		endpoint, st, release, ok, err := a.pools.route(r.Model, r.Context, r.Pool)
		if err != nil {
			return target{}, admissionError{err}
		}
		if ok {
			a.mu.Lock()
			a.model, a.last = r.Model, "running"
			a.mu.Unlock()
			return target{endpoint: endpoint, pool: &st, release: release}, nil
		}
	}
	if err := a.begin(r); err != nil {
		return target{}, admissionError{err}
	}
	return target{endpoint: a.c.BackendURL}, nil
}

func (a *Agent) finish(t target, outcome string, rate float64) {
	if t.pool == nil {
		a.end(outcome, rate)
		return
	}
	t.release()
	a.mu.Lock()
	a.last = outcome
	if rate > 0 {
		a.rate = rate
	}
	a.mu.Unlock()
	a.log.Printf("event=inference pool=%s outcome=%s", t.pool.Name, outcome)
}

func evidence(t target) (string, string) {
	if t.pool != nil && len(t.pool.Workers) > 0 {
		return "pooled", fmt.Sprintf("llama.cpp RPC pool %q across this computer and %d worker(s); per-device memory is in the pool status (from llama-server's own log).", t.pool.Name, len(t.pool.Workers))
	}
	return "single_node", "Backend node verified; CPU/GPU participation is not instrumented. No distributed execution claim."
}

// rejected records a request refused before it reached any server.
func (a *Agent) rejected() {
	a.mu.Lock()
	a.last = "failed"
	a.mu.Unlock()
	a.log.Print("event=inference outcome=failed")
}

func (a *Agent) Generate(ctx context.Context, r backend.Request) (result Result, err error) {
	if err = a.registered(r.Model); err != nil {
		if !a.isBusy() {
			a.rejected()
		}
		return Result{}, err
	}
	t, err := a.acquire(r)
	if err != nil {
		return Result{}, err
	}
	completed := false
	defer func() {
		if !completed {
			if err != nil {
				// This computer's own log keeps the cause; callers get Reason.
				a.log.Printf("event=inference_error error=%q", err.Error())
			}
			a.finish(t, "failed", 0)
		}
	}()
	b, e := backend.New(t.endpoint)
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
	a.finish(t, "completed", response.TokensPerSecond)
	mode, why := evidence(t)
	return Result{Response: response, NodeID: id, Model: r.Model, Mode: mode, DeviceEvidence: why}, nil
}

// GenerateStream streams one completion through emit. It holds the same single
// inference slot as Generate and releases it on every exit path.
func (a *Agent) GenerateStream(ctx context.Context, r backend.Request, emit func(backend.Chunk) error) (err error) {
	if err = a.registered(r.Model); err != nil {
		if !a.isBusy() {
			a.rejected()
		}
		return err
	}
	t, err := a.acquire(r)
	if err != nil {
		return err
	}
	completed := false
	rate := 0.0
	defer func() {
		if !completed {
			if err != nil {
				a.log.Printf("event=inference_error error=%q", err.Error())
			}
			a.finish(t, "failed", 0)
		}
	}()
	b, e := backend.New(t.endpoint)
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
	a.finish(t, "completed", rate)
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
			http.Error(w, Reason(e), 503)
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
			_ = sw.Event("error", map[string]string{"error": Reason(err)})
		}
	})
	root := http.NewServeMux()
	root.Handle("/enroll", security.EnrollHandler(a.dir))
	if a.broker != nil {
		root.Handle("/v1/tunnel", security.Authorize(a.dir, http.HandlerFunc(a.acceptWorker)))
		root.Handle("/v1/workers", security.SelfOnly(a.dir, http.HandlerFunc(a.listWorkers)))
		root.Handle("/v1/workers/", security.SelfOnly(a.dir, http.HandlerFunc(a.proxyWorker)))
		root.Handle("/v1/pool", security.SelfOnly(a.dir, http.HandlerFunc(a.handlePool)))
		root.Handle("/v1/pools", security.SelfOnly(a.dir, http.HandlerFunc(a.handlePools)))
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
		go func() { defer workers.Done(); a.pools.run(ctx) }()
		workers.Add(1)
		go func() { defer workers.Done(); a.links.run(ctx) }()
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
	if a.broker != nil {
		a.serveMeshPaths(ctx, server, &workers)
	}
	e = server.ServeTLS(listener, "", "")
	if errors.Is(e, http.ErrServerClosed) {
		return nil
	}
	return e
}

func (a *Agent) isBusy() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.busy
}
