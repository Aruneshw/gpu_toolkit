package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"meshcompute/internal/backend"
	"meshcompute/internal/config"
	"meshcompute/internal/tunnel"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

// A group host can run several pooled models ("pools"). Each pool is one
// llama-server on the host whose layers are spread, through llama.cpp RPC,
// over the host's own GPUs and the GPUs its workers lend. A worker's RPC
// server serves one client at a time, so a worker belongs to one pool.

// PoolDevice is the memory one computer holds for a pool.
type PoolDevice struct {
	backend.DeviceUse
	// Node is "local" (the host) or the worker ID behind an RPC device.
	Node string `json:"node"`
}

// PoolStatus describes one pool.
type PoolStatus struct {
	Name     string `json:"name"`
	Enabled  bool   `json:"enabled"`
	State    string `json:"state"` // off | starting | ready | failed
	Model    string `json:"model,omitempty"`
	Context  int    `json:"context,omitempty"`
	Parallel int    `json:"parallel,omitempty"`
	// Endpoint is the pool's llama-server on the host's loopback.
	Endpoint string `json:"endpoint,omitempty"`
	// Workers are the workers whose GPUs the running server uses.
	Workers []string `json:"workers"`
	// Sharing are the workers assigned to this pool that can lend a GPU now.
	Sharing  []string          `json:"sharing"`
	Detail   string            `json:"detail,omitempty"`
	Log      []string          `json:"log,omitempty"`
	Settings config.PoolConfig `json:"settings"`
	// Devices is where llama-server placed weights, KV cache and compute
	// buffers, from its own load log.
	Devices   []PoolDevice `json:"devices,omitempty"`
	Offloaded string       `json:"offloaded,omitempty"`
	InFlight  int          `json:"in_flight"`
	// Warnings name workers whose link slows this pool down.
	Warnings []string `json:"warnings,omitempty"`
}

// PoolsStatus is the host's whole pool picture.
type PoolsStatus struct {
	Pools []PoolStatus `json:"pools"`
	// Sharing lists every connected worker able to lend a GPU.
	Sharing []string `json:"sharing"`
}

// DefaultPoolContext is used when no context size was chosen.
const DefaultPoolContext = 4096

// DefaultPoolName is the pool the single-pool commands control.
const DefaultPoolName = "main"

type poolSet struct {
	a    *Agent
	kick chan struct{}

	mu      sync.Mutex
	pools   []*pool
	sharing []string
	misses  map[string]int
	rr      map[string]int
	// polled is when workers were last asked whether they can lend a GPU
	// while no pool ran; kicked forces the next check.
	polled time.Time
	kicked bool
	// connected is the set of connected workers at the last check; a
	// change (a laptop joined or left) triggers an immediate check.
	connected string
}

type pool struct {
	owner *poolSet

	mu       sync.Mutex
	cfg      config.PoolConfig
	state    string
	detail   string
	assigned []string
	log      *backend.LogTail
	proc     *poolProcess
	retryAt  time.Time
	backoff  time.Duration
	inflight int
	// removed is set when the pool is deleted; it never starts again.
	removed bool
}

// poolProcess is one running llama-server and its forwarders.
type poolProcess struct {
	cfg      config.PoolConfig
	workers  []string
	endpoint string
	forwards map[string]string // forwarder address -> worker ID
	alloc    *backend.Allocation
	cancel   context.CancelFunc
	done     chan struct{}
	err      error
	// note is shown with the ready status (e.g. an ignored split share).
	note string
}

func (p *poolProcess) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func newPools(a *Agent) *poolSet {
	s := &poolSet{a: a, kick: make(chan struct{}, 1), misses: map[string]int{}, rr: map[string]int{}}
	for _, c := range a.c.PoolList() {
		s.pools = append(s.pools, s.newPool(c))
	}
	return s
}

func (s *poolSet) newPool(c config.PoolConfig) *pool {
	return &pool{owner: s, cfg: c, state: "off", log: backend.NewLogTail(200)}
}

func (s *poolSet) list() []*pool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*pool(nil), s.pools...)
}

func (s *poolSet) find(name string) *pool {
	for _, p := range s.list() {
		if p.config().Name == name {
			return p
		}
	}
	return nil
}

func (p *pool) config() config.PoolConfig {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg
}

func (s *poolSet) wake() {
	s.mu.Lock()
	s.kicked = true
	s.mu.Unlock()
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Status reports every pool.
func (s *poolSet) Status() PoolsStatus {
	s.mu.Lock()
	sharing := append([]string{}, s.sharing...)
	s.mu.Unlock()
	out := PoolsStatus{Pools: []PoolStatus{}, Sharing: sharing}
	for _, p := range s.list() {
		out.Pools = append(out.Pools, p.Status())
	}
	return out
}

// First returns the status of the first pool, for single-pool callers.
func (s *poolSet) First() *PoolStatus {
	if pools := s.list(); len(pools) > 0 {
		st := pools[0].Status()
		return &st
	}
	return nil
}

func (p *pool) Status() PoolStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.cfg
	st := PoolStatus{Name: c.Name, Enabled: c.Enabled, State: p.state, Model: c.Model, Context: c.Context, Parallel: max(c.Parallel, 1), Detail: p.detail,
		Workers: []string{}, Sharing: append([]string{}, p.assigned...), Log: p.log.Lines(12), Settings: c, InFlight: p.inflight}
	if st.Context == 0 {
		st.Context = DefaultPoolContext
	}
	if p.proc != nil && !p.proc.exited() {
		st.Workers = append(st.Workers, p.proc.workers...)
		st.Endpoint = p.proc.endpoint
		devices, offloaded := p.proc.alloc.Devices()
		st.Offloaded = offloaded
		for _, d := range devices {
			node := "local"
			if d.Endpoint != "" {
				node = p.proc.forwards[d.Endpoint]
			}
			st.Devices = append(st.Devices, PoolDevice{DeviceUse: d, Node: node})
		}
	} else if c.Enabled && st.State == "ready" {
		st.State, st.Detail = "starting", "llama-server stopped; restarting"
	}
	if len(st.Workers) > 0 && p.owner != nil && p.owner.a != nil {
		st.Warnings = linkWarnings(st.Workers, p.owner.a.links.Status())
	}
	return st
}

func (p *pool) set(state, detail string) {
	p.mu.Lock()
	p.state, p.detail = state, detail
	p.mu.Unlock()
}

// SetPool adds or changes one pool and saves it in the host's configuration,
// so the next start resumes it without anyone asking.
func (s *poolSet) SetPool(c config.PoolConfig) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.Enabled {
		if err := s.a.registered(c.Model); err != nil {
			return errors.New("model " + c.Model + " is not registered on this computer; add it with: meshcompute model add NAME /path/model.gguf")
		}
	}
	return s.save(func(list []config.PoolConfig) ([]config.PoolConfig, error) {
		for i := range list {
			if list[i].Name == c.Name {
				list[i] = c
				return list, nil
			}
		}
		return append(list, c), nil
	})
}

// RemovePool stops and forgets a pool.
func (s *poolSet) RemovePool(name string) error {
	return s.save(func(list []config.PoolConfig) ([]config.PoolConfig, error) {
		for i := range list {
			if list[i].Name == name {
				return append(list[:i:i], list[i+1:]...), nil
			}
		}
		return nil, errors.New("no pool named " + name)
	})
}

// save applies change to the saved pool list, validates the result as a
// whole (a worker in two pools is refused) and updates the running set.
func (s *poolSet) save(change func([]config.PoolConfig) ([]config.PoolConfig, error)) error {
	var list []config.PoolConfig
	_, err := config.Update(s.a.dir, func(c *config.Config) error {
		var err error
		if list, err = change(c.PoolList()); err != nil {
			return err
		}
		c.Pools, c.PoolModel, c.PoolContext = list, "", 0
		return c.Validate()
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	byName := map[string]*pool{}
	for _, p := range s.pools {
		byName[p.config().Name] = p
	}
	var next, removed []*pool
	for _, pc := range list {
		if p := byName[pc.Name]; p != nil {
			p.mu.Lock()
			p.cfg = pc
			p.retryAt, p.backoff = time.Time{}, 0
			p.mu.Unlock()
			next = append(next, p)
			delete(byName, pc.Name)
		} else {
			next = append(next, s.newPool(pc))
		}
	}
	for _, p := range byName {
		removed = append(removed, p)
	}
	s.pools = next
	s.mu.Unlock()
	for _, p := range removed {
		// The reconcile loop may hold this pool right now; the flag stops
		// it from starting a server nobody would ever stop.
		p.mu.Lock()
		p.removed = true
		p.mu.Unlock()
		p.stop()
	}
	s.wake()
	return nil
}

func (s *poolSet) run(ctx context.Context) {
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		s.reconcile(ctx)
		select {
		case <-ctx.Done():
			for _, p := range s.list() {
				p.stop()
			}
			return
		case <-tick.C:
		case <-s.kick:
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
// that stops answering is only dropped after several misses while a pool
// uses it, so a brief network stall does not reload the model.
func (s *poolSet) sharingWorkers(ctx context.Context, inUse map[string]bool) []string {
	var ids []string
	seen := map[string]bool{}
	for _, id := range s.a.broker.Workers() {
		seen[id] = true
		st, err := s.a.workerStatus(ctx, id)
		switch {
		case err == nil && st.RPCReady:
			s.misses[id] = 0
			ids = append(ids, id)
		case err != nil && inUse[id] && s.misses[id] < 4:
			s.misses[id]++
			ids = append(ids, id)
		}
	}
	for id := range inUse {
		if !seen[id] && s.misses[id] < 4 {
			s.misses[id]++
			ids = append(ids, id)
		}
	}
	for id := range s.misses {
		if !seen[id] && !inUse[id] {
			delete(s.misses, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// assign gives each enabled pool its workers: the workers it names (that
// can lend now), and to the first enabled pool that names none, every other
// sharing worker no pool names.
func assign(pools []config.PoolConfig, sharing []string) map[string][]string {
	out := map[string][]string{}
	named := map[string]bool{}
	for _, c := range pools {
		for _, w := range c.Workers {
			named[w] = true
		}
	}
	can := map[string]bool{}
	for _, w := range sharing {
		can[w] = true
	}
	auto := ""
	for _, c := range pools {
		if !c.Enabled {
			continue
		}
		if len(c.Workers) == 0 {
			if auto == "" {
				auto = c.Name
			}
			continue
		}
		var mine []string
		for _, w := range c.Workers {
			if can[w] {
				mine = append(mine, w)
			}
		}
		sort.Strings(mine)
		out[c.Name] = mine
	}
	if auto != "" {
		var rest []string
		for _, w := range sharing {
			if !named[w] {
				rest = append(rest, w)
			}
		}
		out[auto] = rest
	}
	return out
}

func (s *poolSet) reconcile(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	pools := s.list()
	var configs []config.PoolConfig
	inUse := map[string]bool{}
	anyEnabled := false
	for _, p := range pools {
		c := p.config()
		configs = append(configs, c)
		anyEnabled = anyEnabled || c.Enabled
		p.mu.Lock()
		if p.proc != nil && !p.proc.exited() {
			for _, w := range p.proc.workers {
				inUse[w] = true
			}
		}
		p.mu.Unlock()
	}
	// While no pool runs, workers are asked at most every 15 seconds (or
	// right after a settings change), so the dashboard can offer them for a
	// new pool without polling every laptop constantly.
	connected := strings.Join(s.a.broker.Workers(), ",")
	s.mu.Lock()
	poll := anyEnabled || s.kicked || connected != s.connected || time.Since(s.polled) > 15*time.Second
	s.kicked, s.connected = false, connected
	sharing := s.sharing
	s.mu.Unlock()
	if poll {
		sharing = s.sharingWorkers(ctx, inUse)
		s.mu.Lock()
		s.sharing, s.polled = sharing, time.Now()
		s.mu.Unlock()
	}
	plan := assign(configs, sharing)
	for _, p := range pools {
		p.reconcile(ctx, plan[p.config().Name])
	}
}

// sameRun reports whether a running server already matches the settings.
func sameRun(a, b config.PoolConfig) bool {
	a.Enabled, b.Enabled = true, true
	return reflect.DeepEqual(a, b)
}

func (p *pool) reconcile(ctx context.Context, desired []string) {
	p.mu.Lock()
	c, proc, removed := p.cfg, p.proc, p.removed
	p.assigned = append([]string{}, desired...)
	p.mu.Unlock()
	if removed {
		p.stop()
		return
	}
	if !c.Enabled {
		p.stop()
		p.set("off", "")
		return
	}
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
		if sameRun(proc.cfg, c) && same(proc.workers, desired) {
			p.observe(ctx, proc)
			return
		}
		// Decide and take the pool out of rotation in one step: a request
		// that reserved a slot after an unlocked check would otherwise reach
		// a llama-server that is being stopped.
		p.mu.Lock()
		if p.inflight > 0 {
			p.state, p.detail = "ready", "the settings or the shared GPUs changed; reloading after the current requests"
			p.mu.Unlock()
			return
		}
		p.state, p.detail = "starting", "reloading with the new settings or shared GPUs"
		p.mu.Unlock()
		beforeReloadStop()
		p.stop()
	}
	p.mu.Lock()
	wait := time.Until(p.retryAt)
	p.mu.Unlock()
	if wait > 0 {
		return
	}
	p.start(ctx, c, desired)
}

// outputDevice is where a pool's output layer goes (see
// config.PoolConfig.OutputLayer): "" leaves llama.cpp's choice. Without
// workers nothing crosses the network, so nothing is overridden.
func outputDevice(c config.PoolConfig, local []string, workers int) string {
	if workers == 0 || c.OutputLayer == "worker" {
		return ""
	}
	if !c.NoHostGPU && len(local) > 0 {
		return local[0]
	}
	if c.OutputLayer == "host" {
		return "CPU"
	}
	return ""
}

// beforeReloadStop runs between deciding to reload a pool and stopping its
// llama-server; tests use it to send a request into that gap.
var beforeReloadStop = func() {}

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

// observe reports whether the running server has finished loading.
func (p *pool) observe(ctx context.Context, proc *poolProcess) {
	b, err := backend.New(proc.endpoint)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	caps, err := b.Health(ctx)
	if err == nil && len(caps.Models) == 1 && caps.Models[0] == proc.cfg.Model {
		p.mu.Lock()
		p.backoff = 0
		p.mu.Unlock()
		detail := "running on this computer only; no worker is lending a GPU to this pool"
		if n := len(proc.workers); n > 0 {
			detail = fmt.Sprintf("layers are spread over this computer and %d worker(s)", n)
		}
		if proc.note != "" {
			detail += "; " + proc.note
		}
		p.set("ready", detail)
		return
	}
	p.set("starting", "loading the model (weights are sent to each worker the first time; this can take minutes on a slow link)")
}

// endpointFor picks the pool's llama-server address: the configured backend
// URL if no other pool holds it, otherwise a free loopback port.
func (s *poolSet) endpointFor(self *pool) (string, error) {
	main := s.a.c.BackendURL
	taken := false
	for _, p := range s.list() {
		if p == self {
			continue
		}
		p.mu.Lock()
		if p.proc != nil && !p.proc.exited() && p.proc.endpoint == main {
			taken = true
		}
		p.mu.Unlock()
	}
	if !taken {
		u, err := url.Parse(main)
		if err != nil {
			return "", errors.New("invalid backend_url")
		}
		if c, err := net.DialTimeout("tcp", u.Host, time.Second); err == nil {
			c.Close()
			return "", errors.New("another program already listens on " + u.Host + "; stop that backend so the pool can start its own llama-server")
		}
		return main, nil
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := l.Addr().String()
	l.Close()
	return "http://" + addr, nil
}

// devicesFor returns the --device list and --tensor-split weights: the
// host's own accelerators first (unless excluded), then one RPC device per
// worker in order.
func devicesFor(c config.PoolConfig, local []string, workers []string) ([]string, []float64, error) {
	var devices []string
	if !c.NoHostGPU {
		devices = append(devices, local...)
	}
	for i := range workers {
		devices = append(devices, fmt.Sprintf("RPC%d", i))
	}
	if len(devices) == 0 {
		if len(local) > 0 {
			return []string{"none"}, nil, nil // host GPU excluded, no workers: CPU only
		}
		return nil, nil, nil
	}
	if len(c.Split) == 0 {
		if !c.NoHostGPU && len(workers) == 0 {
			return nil, nil, nil // llama.cpp's default device choice is the same
		}
		return devices, nil, nil
	}
	weight := func(node string) float64 {
		if w, ok := c.Split[node]; ok {
			return w
		}
		return 1
	}
	var split []float64
	if !c.NoHostGPU {
		for range local {
			split = append(split, weight("local")/float64(len(local)))
		}
	}
	positive := false
	for _, w := range workers {
		split = append(split, weight(w))
	}
	for _, w := range split {
		positive = positive || w > 0
	}
	if !positive {
		return nil, nil, errors.New("every computer in this pool has split weight 0; give at least one a weight above 0")
	}
	return devices, split, nil
}

func (p *pool) start(ctx context.Context, c config.PoolConfig, workers []string) {
	fail := func(detail string) {
		p.mu.Lock()
		p.state, p.detail = "failed", detail
		p.retryAt = time.Now().Add(15 * time.Second)
		p.mu.Unlock()
	}
	a := p.owner.a
	binary, err := backend.FindTool(a.c.LlamaServer, []string{"llama-server"}, backend.ToolDirs(a.dir))
	if err != nil {
		fail("llama-server was not found on this computer; install llama.cpp (./linux.sh gpu, or win.bat option 5) or run: meshcompute config set llama_server /path/llama-server")
		return
	}
	local, listErr := backend.LocalDevices(ctx, binary)
	var note string
	var devices []string
	var split []float64
	if listErr != nil {
		// Without device names llama.cpp still uses every device by
		// itself; only settings that name devices need the list.
		if len(c.Split) > 0 || c.NoHostGPU {
			fail("cannot apply the split or host-GPU setting: " + listErr.Error())
			return
		}
		fmt.Fprintf(p.log, "note: %v; letting llama.cpp choose devices\n", listErr)
	} else {
		devices, split, err = devicesFor(c, local, workers)
		if err != nil {
			fail(err.Error())
			return
		}
		if w, ok := c.Split["local"]; ok && w > 0 && len(local) == 0 && !c.NoHostGPU {
			note = fmt.Sprintf("this computer has no GPU that %s can use, so its split share (local=%g) is ignored; the workers share the layers by their weights", filepath.Base(binary), w)
			fmt.Fprintf(p.log, "note: %s\n", note)
		}
	}
	outputOn := outputDevice(c, local, len(workers))
	if outputOn != "" {
		fmt.Fprintf(p.log, "note: the output layer runs on this computer (%s), so workers send it a small hidden state per token instead of the whole vocabulary's logits\n", outputOn)
	}
	endpoint, err := p.owner.endpointFor(p)
	if err != nil {
		fail(err.Error())
		return
	}
	procCtx, cancel := context.WithCancel(ctx)
	var listeners []net.Listener
	var addrs []string
	forwards := map[string]string{}
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
		forwards[l.Addr().String()] = id
		go p.forward(procCtx, l, id)
	}
	contextSize := c.Context
	if contextSize == 0 {
		contextSize = DefaultPoolContext
	}
	alloc := &backend.Allocation{}
	proc := &poolProcess{cfg: c, workers: workers, endpoint: endpoint, forwards: forwards, alloc: alloc, cancel: cancel, done: make(chan struct{}), note: note}
	fmt.Fprintf(p.log, "--- pool %q: starting %s for %q, context %d, %d slot(s), %d worker GPU endpoint(s), devices %s ---\n",
		c.Name, filepath.Base(binary), c.Model, contextSize, max(c.Parallel, 1), len(workers), strings.Join(append([]string{}, devices...), ","))
	a.log.Printf("event=pool name=%s state=starting workers=%d", c.Name, len(workers))
	p.mu.Lock()
	if p.removed || !p.cfg.Enabled {
		p.mu.Unlock()
		cancel()
		for _, l := range listeners {
			l.Close()
		}
		return
	}
	p.proc, p.state, p.detail = proc, "starting", "loading the model"
	p.mu.Unlock()
	out := io.MultiWriter(p.log, alloc)
	go func() {
		// All layers are offloaded; llama.cpp divides them over the devices
		// by free memory, or by the pool's split weights.
		proc.err = backend.Run(procCtx, backend.LaunchConfig{Binary: binary, RegistryDir: filepath.Join(a.dir, "models"), Model: c.Model, Endpoint: endpoint,
			Context: contextSize, GPULayers: 999, RPC: addrs, Devices: devices, TensorSplit: split, Parallel: c.Parallel,
			CacheTypeK: c.CacheTypeK, CacheTypeV: c.CacheTypeV, NoKVOffload: c.KVOnHost, FlashAttn: c.FlashAttn, OutputOn: outputOn, Verbose: true,
			Stdout: out, Stderr: out})
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
	a := p.owner.a
	for {
		local, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			remote, err := a.broker.OpenStream(ctx, id, StreamRPC, a.verifyWorker)
			if err != nil {
				fmt.Fprintf(p.log, "cannot reach the GPU of %s: %v\n", short(id), err)
				local.Close()
				return
			}
			unhook := context.AfterFunc(ctx, func() { remote.Close(); local.Close() })
			defer unhook()
			tunnel.MeteredPipe(local, remote, a.links.meter(id))
		}()
	}
}

func short(id string) string {
	if len(id) > 15 {
		return id[:15]
	}
	return id
}

// slotContext is the context one request slot of the pool holds.
func slotContext(c config.PoolConfig) int {
	ctx := c.Context
	if ctx == 0 {
		ctx = DefaultPoolContext
	}
	return ctx / max(c.Parallel, 1)
}

// route reserves a request slot on a ready pool serving model whose slots
// hold contextSize tokens, taking turns between such pools. ok is false
// when no ready pool serves the model; err is set when none can take the
// request now (slots busy, or the context does not fit).
func (s *poolSet) route(model string, contextSize int, name string) (endpoint string, st PoolStatus, release func(), ok bool, err error) {
	if name != "" {
		// A request for a named pool goes there or is refused; it never
		// falls back to another pool or to the plain backend.
		p := s.find(name)
		if p == nil {
			var names []string
			for _, q := range s.list() {
				names = append(names, q.config().Name)
			}
			return "", PoolStatus{}, nil, true, fmt.Errorf("there is no pool named %q (pools: %s)", name, strings.Join(names, ", "))
		}
		p.mu.Lock()
		served, state := p.cfg.Model, p.state
		p.mu.Unlock()
		if served != model {
			return "", PoolStatus{}, nil, true, fmt.Errorf("pool %q serves %s, not %s", name, served, model)
		}
		if state != "ready" {
			return "", PoolStatus{}, nil, true, fmt.Errorf("pool %q is not ready (%s); retry when it is, or leave out the pool to use any ready one", name, state)
		}
	}
	var ready []*pool
	largest := 0
	for _, p := range s.list() {
		if name != "" && p.config().Name != name {
			continue
		}
		p.mu.Lock()
		if p.cfg.Enabled && p.cfg.Model == model && p.state == "ready" && p.proc != nil && !p.proc.exited() {
			if per := slotContext(p.cfg); per >= contextSize {
				ready = append(ready, p)
			} else if per > largest {
				largest = per
			}
		}
		p.mu.Unlock()
	}
	if len(ready) == 0 {
		if largest > 0 {
			return "", PoolStatus{}, nil, true, fmt.Errorf("the pools serving %s hold at most %d tokens per request; ask for a smaller context", model, largest)
		}
		return "", PoolStatus{}, nil, false, nil
	}
	s.mu.Lock()
	start := s.rr[model]
	s.rr[model] = start + 1
	s.mu.Unlock()
	for i := range ready {
		p := ready[(start+i)%len(ready)]
		p.mu.Lock()
		if p.proc != nil && p.inflight < max(p.cfg.Parallel, 1) {
			p.inflight++
			endpoint = p.proc.endpoint
			p.mu.Unlock()
			var once sync.Once
			return endpoint, p.Status(), func() {
				once.Do(func() {
					p.mu.Lock()
					p.inflight--
					p.mu.Unlock()
					s.wake()
				})
			}, true, nil
		}
		p.mu.Unlock()
	}
	if name != "" {
		return "", PoolStatus{}, nil, true, fmt.Errorf("every request slot of pool %q is busy; retry shortly, or leave out the pool to use any ready one", name)
	}
	return "", PoolStatus{}, nil, true, fmt.Errorf("every request slot of the pools serving %s is busy; retry shortly", model)
}

// models lists the models ready pools serve, with their largest slot
// context.
func (s *poolSet) models() map[string]int {
	out := map[string]int{}
	for _, st := range s.Status().Pools {
		if st.State == "ready" {
			if per := st.Context / max(st.Parallel, 1); per > out[st.Model] {
				out[st.Model] = per
			}
		}
	}
	return out
}

func (s *poolSet) inflight() int {
	n := 0
	for _, p := range s.list() {
		p.mu.Lock()
		n += p.inflight
		p.mu.Unlock()
	}
	return n
}

// handlePools reads (GET) or changes (POST) the pools. Only this host's own
// certificate reaches it (SelfOnly): the dashboard and CLI on this computer.
func (a *Agent) handlePools(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		var req struct {
			Action string            `json:"action"` // set | remove
			Pool   config.PoolConfig `json:"pool"`
			Name   string            `json:"name"`
		}
		if !decodeSmall(w, r, &req) {
			return
		}
		var err error
		switch req.Action {
		case "set":
			err = a.pools.SetPool(req.Pool)
		case "remove":
			err = a.pools.RemovePool(req.Name)
		default:
			err = errors.New("action must be set or remove")
		}
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			encode(w, map[string]string{"error": err.Error()})
			return
		}
	default:
		http.Error(w, "method not allowed", 405)
		return
	}
	encode(w, a.pools.Status())
}

// handlePool is the single-pool form used by `pool start/stop/status` and
// older dashboards: it controls the first pool (created as "main").
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
		c := config.PoolConfig{Name: DefaultPoolName}
		first := a.pools.First()
		if first != nil {
			c = first.Settings
		} else if !req.Enabled {
			break // nothing to stop
		}
		c.Enabled = req.Enabled
		if req.Enabled {
			c.Model = strings.TrimSpace(req.Model)
			if req.Context != 0 {
				c.Context = req.Context
			}
		}
		var err error
		if c.Model == "" {
			err = errors.New("that model is not registered on this computer; add it with: meshcompute model add NAME /path/model.gguf")
		} else {
			err = a.pools.SetPool(c)
		}
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			encode(w, map[string]string{"error": err.Error()})
			return
		}
	default:
		http.Error(w, "method not allowed", 405)
		return
	}
	if first := a.pools.First(); first != nil {
		encode(w, first)
		return
	}
	encode(w, PoolStatus{Name: DefaultPoolName, State: "off", Workers: []string{}, Sharing: a.pools.Status().Sharing})
}
