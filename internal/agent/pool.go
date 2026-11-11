package agent

import (
	"context"
	"errors"
	"fmt"
	"meshcompute/internal/backend"
	"meshcompute/internal/config"
	"meshcompute/internal/tunnel"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// PoolStatus describes the group host's pooled model: one llama-server on
// the host whose layers are spread over the host and every worker that lends
// its GPU through llama.cpp RPC.
type PoolStatus struct {
	Enabled bool   `json:"enabled"`
	State   string `json:"state"` // off | starting | ready | failed
	Model   string `json:"model,omitempty"`
	Context int    `json:"context,omitempty"`
	// Workers are the workers whose GPUs the running server uses.
	Workers []string `json:"workers"`
	// Sharing are connected workers currently able to lend a GPU.
	Sharing []string `json:"sharing"`
	Detail  string   `json:"detail,omitempty"`
	Log     []string `json:"log,omitempty"`
}

// DefaultPoolContext is used when no context size was chosen.
const DefaultPoolContext = 4096

type pool struct {
	a    *Agent
	kick chan struct{}

	mu      sync.Mutex
	enabled bool
	model   string
	context int
	state   string
	detail  string
	sharing []string
	misses  map[string]int
	log     *backend.LogTail
	proc    *poolProcess
	retryAt time.Time
	backoff time.Duration
}

// poolProcess is one running llama-server and its forwarders.
type poolProcess struct {
	model   string
	context int
	workers []string
	cancel  context.CancelFunc
	done    chan struct{}
	err     error
}

func (p *poolProcess) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func newPool(a *Agent) *pool {
	p := &pool{a: a, kick: make(chan struct{}, 1), state: "off", misses: map[string]int{}, log: backend.NewLogTail(200)}
	if a.c.PoolModel != "" {
		p.enabled, p.model, p.context = true, a.c.PoolModel, a.c.PoolContext
		if p.context == 0 {
			p.context = DefaultPoolContext
		}
	}
	return p
}

func (p *pool) Status() PoolStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := PoolStatus{Enabled: p.enabled, State: p.state, Model: p.model, Context: p.context, Detail: p.detail, Workers: []string{}, Sharing: append([]string{}, p.sharing...), Log: p.log.Lines(12)}
	if p.proc != nil && !p.proc.exited() {
		st.Workers = append(st.Workers, p.proc.workers...)
	} else if p.enabled && st.State == "ready" {
		// The server just went away; the next reconcile restarts it.
		st.State, st.Detail = "starting", "llama-server stopped; restarting"
	}
	return st
}

func (p *pool) set(state, detail string) {
	p.mu.Lock()
	p.state, p.detail = state, detail
	p.mu.Unlock()
}

// Configure turns the pool on or off and remembers the choice in the host's
// configuration, so the next start resumes it without anyone asking.
func (p *pool) Configure(enabled bool, name string, contextSize int) error {
	if enabled {
		if err := p.a.registered(name); err != nil {
			return errors.New("that model is not registered on this computer; add it with: meshcompute model add NAME /path/model.gguf")
		}
		if contextSize == 0 {
			contextSize = DefaultPoolContext
		}
		if contextSize < 256 || contextSize > 1048576 {
			return errors.New("context must be between 256 and 1048576")
		}
	} else {
		name, contextSize = "", 0
	}
	c, err := config.Load(p.a.dir)
	if err != nil {
		return err
	}
	c.PoolModel, c.PoolContext = name, contextSize
	if err := config.Save(p.a.dir, c); err != nil {
		return err
	}
	p.mu.Lock()
	p.enabled, p.model, p.context = enabled, name, contextSize
	p.retryAt, p.backoff = time.Time{}, 0
	p.mu.Unlock()
	select {
	case p.kick <- struct{}{}:
	default:
	}
	return nil
}

func (p *pool) run(ctx context.Context) {
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		p.reconcile(ctx)
		select {
		case <-ctx.Done():
			p.stop()
			return
		case <-tick.C:
		case <-p.kick:
		}
	}
}

func (p *pool) stop() {
	p.mu.Lock()
	proc := p.proc
	p.proc = nil
	p.mu.Unlock()
	if proc != nil {
		proc.cancel()
		<-proc.done
	}
}

// sharingWorkers returns the connected workers able to lend a GPU. A worker
// that stops answering is only dropped after several misses, so a brief
// network stall does not reload the model.
func (p *pool) sharingWorkers(ctx context.Context, current []string) []string {
	using := map[string]bool{}
	for _, id := range current {
		using[id] = true
	}
	var ids []string
	seen := map[string]bool{}
	for _, id := range p.a.broker.Workers() {
		seen[id] = true
		st, err := p.a.workerStatus(ctx, id)
		switch {
		case err == nil && st.RPCReady:
			p.misses[id] = 0
			ids = append(ids, id)
		case err != nil && using[id] && p.misses[id] < 4:
			p.misses[id]++
			ids = append(ids, id)
		}
	}
	for _, id := range current {
		if !seen[id] && p.misses[id] < 4 {
			p.misses[id]++
			ids = append(ids, id)
		}
	}
	for id := range p.misses {
		if !seen[id] && !using[id] {
			delete(p.misses, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func same(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (p *pool) reconcile(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	p.mu.Lock()
	enabled, name, contextSize, proc := p.enabled, p.model, p.context, p.proc
	p.mu.Unlock()
	if !enabled {
		p.stop()
		// While off, do not poll workers: reuse what the dashboard last saw.
		var sharing []string
		connected := p.a.broker.Workers()
		p.a.mu.Lock()
		for _, id := range connected {
			if p.a.workerCache[id].RPCReady {
				sharing = append(sharing, id)
			}
		}
		p.a.mu.Unlock()
		p.mu.Lock()
		p.state, p.detail, p.sharing = "off", "", sharing
		p.mu.Unlock()
		return
	}
	var current []string
	if proc != nil && !proc.exited() {
		current = proc.workers
	}
	desired := p.sharingWorkers(ctx, current)
	p.mu.Lock()
	p.sharing = desired
	p.mu.Unlock()
	if proc != nil && proc.exited() {
		p.mu.Lock()
		if p.backoff < time.Minute {
			p.backoff = p.backoff*2 + 5*time.Second
		}
		p.retryAt = time.Now().Add(p.backoff)
		p.proc = nil
		p.state = "failed"
		p.detail = "llama-server stopped"
		if proc.err != nil {
			p.detail += ": " + proc.err.Error()
		}
		if last := p.log.Lines(1); len(last) == 1 {
			p.detail += " (" + last[0] + ")"
		}
		p.detail += fmt.Sprintf("; retrying in %s", p.backoff)
		p.mu.Unlock()
		return
	}
	if proc != nil {
		if proc.model == name && proc.context == contextSize && same(proc.workers, desired) {
			p.observe(ctx, proc)
			return
		}
		if p.a.isBusy() {
			p.set("ready", "the set of shared GPUs changed; reloading after the current request")
			return
		}
		p.stop()
	}
	p.mu.Lock()
	wait := time.Until(p.retryAt)
	p.mu.Unlock()
	if wait > 0 {
		return
	}
	p.start(ctx, name, contextSize, desired)
}

// observe reports whether the running server has finished loading.
func (p *pool) observe(ctx context.Context, proc *poolProcess) {
	b, err := backend.New(p.a.c.BackendURL)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	caps, err := b.Health(ctx)
	if err == nil && len(caps.Models) == 1 && caps.Models[0] == proc.model {
		p.mu.Lock()
		p.backoff = 0
		p.mu.Unlock()
		detail := "running on this computer only; no worker is lending a GPU yet"
		if n := len(proc.workers); n > 0 {
			detail = fmt.Sprintf("layers are spread over this computer and %d worker(s)", n)
		}
		p.set("ready", detail)
		return
	}
	p.set("starting", "loading the model (weights are sent to each worker the first time; this can take minutes on a slow link)")
}

func (p *pool) start(ctx context.Context, name string, contextSize int, workers []string) {
	fail := func(detail string) {
		p.mu.Lock()
		p.state, p.detail = "failed", detail
		p.retryAt = time.Now().Add(15 * time.Second)
		p.mu.Unlock()
	}
	binary, err := backend.FindTool(p.a.c.LlamaServer, []string{"llama-server"}, backend.ToolDirs(p.a.dir))
	if err != nil {
		fail("llama-server was not found on this computer; install llama.cpp (./linux.sh gpu, or win.bat option 5) or run: meshcompute config set llama_server /path/llama-server")
		return
	}
	u, err := url.Parse(p.a.c.BackendURL)
	if err != nil {
		fail("invalid backend_url")
		return
	}
	if c, err := net.DialTimeout("tcp", u.Host, time.Second); err == nil {
		c.Close()
		fail("another program already listens on " + u.Host + "; stop that backend so the pool can start its own llama-server")
		return
	}
	procCtx, cancel := context.WithCancel(ctx)
	var listeners []net.Listener
	var addrs []string
	for _, id := range workers {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			cancel()
			for _, open := range listeners {
				open.Close()
			}
			fail("cannot open a local forwarding port: " + err.Error())
			return
		}
		listeners = append(listeners, l)
		addrs = append(addrs, l.Addr().String())
		go p.forward(procCtx, l, id)
	}
	proc := &poolProcess{model: name, context: contextSize, workers: workers, cancel: cancel, done: make(chan struct{})}
	fmt.Fprintf(p.log, "--- starting %s for %q, context %d, %d worker GPU endpoint(s) ---\n", filepath.Base(binary), name, contextSize, len(workers))
	p.a.log.Printf("event=pool state=starting workers=%d", len(workers))
	p.mu.Lock()
	p.proc, p.state, p.detail = proc, "starting", "loading the model"
	p.mu.Unlock()
	go func() {
		// All layers are offloaded; llama.cpp divides them over the local GPU
		// (if any) and the RPC devices by free memory.
		proc.err = backend.Run(procCtx, backend.LaunchConfig{Binary: binary, RegistryDir: filepath.Join(p.a.dir, "models"), Model: name, Endpoint: p.a.c.BackendURL,
			Context: contextSize, GPULayers: 999, RPC: addrs, Stdout: p.log, Stderr: p.log})
		cancel()
		for _, l := range listeners {
			l.Close()
		}
		close(proc.done)
	}()
}

// forward carries each connection llama-server makes to one loopback port
// through that worker's authenticated tunnel to its rpc-server.
func (p *pool) forward(ctx context.Context, l net.Listener, id string) {
	stop := context.AfterFunc(ctx, func() { l.Close() })
	defer stop()
	for {
		local, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			remote, err := p.a.broker.OpenStream(ctx, id, StreamRPC, p.a.verifyWorker)
			if err != nil {
				fmt.Fprintf(p.log, "cannot reach the GPU of %s: %v\n", short(id), err)
				local.Close()
				return
			}
			unhook := context.AfterFunc(ctx, func() { remote.Close(); local.Close() })
			defer unhook()
			tunnel.Pipe(local, remote)
		}()
	}
}

func short(id string) string {
	if len(id) > 15 {
		return id[:15]
	}
	return id
}

func (a *Agent) isBusy() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.busy
}

// handlePool reads or changes the pooled model. Only this host's own
// certificate reaches it (SelfOnly): the dashboard and CLI on this computer.
func (a *Agent) handlePool(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		var req struct {
			Enabled bool   `json:"enabled"`
			Model   string `json:"model"`
			Context int    `json:"context"`
		}
		if !decodeSmall(w, r, &req) {
			return
		}
		if err := a.pool.Configure(req.Enabled, strings.TrimSpace(req.Model), req.Context); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			encode(w, map[string]string{"error": err.Error()})
			return
		}
	default:
		http.Error(w, "method not allowed", 405)
		return
	}
	encode(w, a.pool.Status())
}
