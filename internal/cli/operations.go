package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"meshcompute/internal/agent"
	"meshcompute/internal/backend"
	"meshcompute/internal/config"
	"meshcompute/internal/dashboard"
	"meshcompute/internal/model"
	"meshcompute/internal/network"
	"meshcompute/internal/resource"
	"meshcompute/internal/scheduler"
	"meshcompute/internal/security"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func request(ctx context.Context, cl *http.Client, endpoint, path string, body any, result any) error {
	var data []byte
	var e error
	if body != nil {
		data, e = json.Marshal(body)
		if e != nil {
			return e
		}
	}
	method := http.MethodGet
	if body != nil {
		method = http.MethodPost
	}
	req, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(endpoint, "/")+path, bytes.NewReader(data))
	if e != nil {
		return e
	}
	req.Header.Set("Content-Type", "application/json")
	res, e := cl.Do(req)
	if e != nil {
		return errors.New("agent unreachable or authentication failed; check start, VPN, identity, revocation and certificate expiry")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		if why := backend.ErrorText(res); why != "" {
			return fmt.Errorf("agent refused the request (HTTP %d): %s", res.StatusCode, why)
		}
		return fmt.Errorf("agent returned HTTP %d; check backend readiness, registered model, context and permissions; request was not retried", res.StatusCode)
	}
	b, e := io.ReadAll(io.LimitReader(res.Body, backend.MaxResponseBytes+1))
	if e != nil {
		return e
	}
	if len(b) > backend.MaxResponseBytes {
		return errors.New("agent response too large")
	}
	return json.Unmarshal(b, result)
}
func peer(c config.Config, name string) (string, error) {
	if name == "local" {
		return c.LocalEndpoint(), nil
	}
	for _, p := range c.Peers {
		if p.Name == name {
			return p.Endpoint, nil
		}
	}
	return "", errors.New("unknown peer; use peer add NAME HTTPS_ENDPOINT")
}

func statuses(ctx context.Context, dir string, c config.Config) ([]dashboard.Node, error) {
	return dashboard.NewRemote(dir, c).Statuses(ctx)
}
func operational(ctx context.Context, dir string, c config.Config, command string, args []string, in io.Reader, out io.Writer) error {
	registry := filepath.Join(dir, "models")
	switch command {
	case "start":
		if len(args) > 1 || len(args) == 1 && args[0] != "--no-open" {
			return errors.New("usage: start [--no-open]")
		}
		if host, reverse, _, err := security.Group(dir); err == nil && reverse && !host {
			// A group worker: also serve this computer's own dashboard.
			return serveWorker(ctx, dir, c, out, len(args) == 0)
		}
		return agent.New(dir, c, out).Serve(ctx)
	case "status":
		if len(args) != 0 {
			return errors.New("status takes no arguments")
		}
		s, e := statuses(ctx, dir, c)
		if e != nil {
			return e
		}
		return writeJSON(out, s)
	case "ui":
		listen := "127.0.0.1:8081"
		open := false
		if e := flags("ui", args, func(f *flag.FlagSet) {
			f.StringVar(&listen, "listen", listen, "loopback listen address")
			f.BoolVar(&open, "open", false, "open the dashboard in the default browser")
		}); e != nil {
			return e
		}
		s, e := dashboard.New(dashboard.NewRemote(dir, c), listen, "")
		if e != nil {
			return e
		}
		s.EnableTerminal(dir)
		if _, e = fmt.Fprintf(out, "MeshCompute dashboard: http://%s/?token=%s\n", s.Listen(), s.Token()); e != nil {
			return e
		}
		if _, e = fmt.Fprintln(out, "Loopback only, token protected. Press Ctrl-C to stop."); e != nil {
			return e
		}
		if open {
			openBrowser(fmt.Sprintf("http://%s/?token=%s", s.Listen(), s.Token()))
		}
		return s.Serve(ctx)
	case "monitor":
		interval := 5 * time.Second
		if e := flags("monitor", args, func(f *flag.FlagSet) { f.DurationVar(&interval, "interval", interval, "refresh") }); e != nil {
			return e
		}
		if interval < time.Second {
			return errors.New("interval must be at least 1s")
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			s, e := statuses(ctx, dir, c)
			if e != nil {
				return e
			}
			if e = writeJSON(out, s); e != nil {
				return e
			}
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
			}
		}
	case "doctor":
		if len(args) != 0 {
			return errors.New("doctor takes no arguments")
		}
		s, e := resource.Discover(ctx, dir)
		if e != nil {
			return e
		}
		_, identityErr := security.NodeID(dir)
		_, binaryErr := exec.LookPath("llama-server")
		b, _ := backend.New(c.BackendURL)
		bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		_, backendErr := b.Health(bounded)
		provider, _ := network.DetectTailscale(bounded)
		// Explicit allowlist: no raw configuration, addresses, paths, aliases, or errors.
		return writeJSON(out, map[string]any{"os": s.OS, "arch": s.Arch, "logical_cpus": s.LogicalCPUs, "ram_total_bytes": s.RAMTotalBytes, "ram_available_bytes": s.RAMAvailableBytes, "gpu_count": len(s.GPUs), "gpu_discovery_available": s.GPUDiscoveryAvailable, "identity_present": identityErr == nil, "llama_server_on_path": binaryErr == nil, "backend_ready": backendErr == nil, "tailscale_available": provider.Available, "mesh_interfaces": len(meshAddresses()), "distributed_verified": false, "guidance": "Missing backend: follow docs/backend.md. Missing identity: cluster create or join. VPN setup is explicit. CPU availability and unknown hardware metrics are not inferred."})
	case "model":
		if len(args) == 1 && args[0] == "list" {
			entries, e := model.List(registry)
			if e != nil {
				return e
			}
			return writeJSON(out, entries)
		}
		if len(args) == 3 && args[0] == "add" {
			entry, e := model.Add(registry, args[1], args[2])
			if e != nil {
				return e
			}
			return writeJSON(out, entry)
		}
		if len(args) == 2 && args[0] == "scan" {
			found, e := model.Scan(args[1])
			if e != nil {
				return e
			}
			return writeJSON(out, found)
		}
		return errors.New("usage: model add NAME LOCAL_GGUF_PATH | model list | model scan DIR (downloads are not automatic)")
	case "backend":
		if len(args) == 1 && args[0] == "list" {
			b, e := backend.New(c.BackendURL)
			if e != nil {
				return e
			}
			bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			caps, e := b.Health(bounded)
			return writeJSON(out, map[string]any{"adapter": "llama.cpp", "ready": e == nil, "capabilities": caps, "guidance": "Use backend serve with a compatible llama-server binary, or on a group host: pool start MODEL. See docs/backend.md."})
		}
		if len(args) >= 2 && args[0] == "serve" {
			binary := ""
			contextSize := 2048
			gpuLayers := 0
			var kv uint64
			overhead := uint64(512 << 20)
			if e := flags("backend serve", args[2:], func(f *flag.FlagSet) {
				f.StringVar(&binary, "binary", "", "llama-server path")
				f.IntVar(&contextSize, "context", contextSize, "slot context")
				f.IntVar(&gpuLayers, "gpu-layers", 0, "GPU layers")
				f.Uint64Var(&kv, "kv-bytes-per-token", 0, "architecture-derived KV estimate, required")
				f.Uint64Var(&overhead, "overhead-bytes", overhead, "runtime and temporary buffers")
			}); e != nil {
				return e
			}
			if contextSize < 1 || gpuLayers < 0 {
				return errors.New("context must be positive and GPU layers nonnegative")
			}
			entry, e := model.Get(registry, args[1])
			if e != nil {
				return e
			}
			s, e := resource.Discover(ctx, dir)
			if e != nil {
				return e
			}
			r := scheduler.Recommend([]scheduler.Worker{{ID: "local", Local: true, Available: true, LastSeen: time.Now(), Resources: s}}, scheduler.Model{WeightBytes: uint64(entry.SizeBytes), KVBytesPerToken: kv, ContextTokens: uint64(contextSize), OverheadBytes: overhead, CPUOverheadBytes: 256 << 20}, scheduler.DefaultPolicy())
			fits := false
			for _, candidate := range r.Candidates {
				if (gpuLayers == 0 && candidate.Device == "cpu") || (gpuLayers > 0 && candidate.Device == "gpu") {
					fits = true
				}
			}
			if !fits {
				_ = writeJSON(out, r)
				return errors.New("memory admission failed; provide architecture-derived --kv-bytes-per-token, reduce context/model size, or use a suitable device; partial offload planning is not implemented")
			}
			if binary == "" {
				binary, e = exec.LookPath("llama-server")
				if e != nil {
					return errors.New("llama-server missing; see docs/backend.md or pass --binary")
				}
			}
			binary, e = filepath.Abs(binary)
			if e != nil {
				return e
			}
			return backend.Run(ctx, backend.LaunchConfig{Binary: binary, RegistryDir: registry, Model: args[1], Endpoint: c.BackendURL, Context: contextSize, GPULayers: gpuLayers, Stdout: out, Stderr: out})
		}
		return errors.New("usage: backend list | backend serve NAME --kv-bytes-per-token N [--binary PATH] [--context 2048] [--gpu-layers 0]")
	case "run":
		return run(ctx, dir, c, args, in, out)
	case "network":
		if len(args) != 2 || args[0] != "probe" {
			return errors.New("usage: network probe PEER_NAME")
		}
		endpoint, e := dashboard.NewRemote(dir, c).Endpoint(ctx, args[1])
		if e != nil {
			return e
		}
		cl, e := security.Client(dir)
		if e != nil {
			return e
		}
		defer cl.CloseIdleConnections()
		q, e := network.Measure(ctx, cl, endpoint)
		if e != nil {
			return e
		}
		return writeJSON(out, map[string]any{"measurement": q, "recommendation": networkAdvice(q)})
	case "mesh":
		if len(args) != 0 {
			return errors.New("mesh takes no arguments")
		}
		found := meshAddresses()
		if found == nil {
			found = []meshAddress{}
		}
		guidance := "No mesh VPN interface is up. Connect Tailscale (tailscale up) or WireGuard (wg-quick up wg0) on every computer, then host with: meshcompute host --mesh auto"
		if len(found) > 0 {
			guidance = "Host on the mesh with: meshcompute host --mesh auto   (or --mesh " + found[0].Interface + "). Friends on the same VPN then join with the invitation as usual."
		}
		return writeJSON(out, map[string]any{"mesh": found, "guidance": guidance})
	case "pool":
		return poolCommand(ctx, dir, c, args, out)
	case "simulate":
		if len(args) != 0 {
			return errors.New("simulate takes no arguments")
		}
		return simulation(out)
	default:
		return fmt.Errorf("unknown command %q; run meshcompute help", command)
	}
}

// poolCommand controls the pooled model through this host's running agent.
func poolCommand(ctx context.Context, dir string, c config.Config, args []string, out io.Writer) error {
	const usage = `usage: pool start MODEL [--name main] [--context 4096] [--slots 1] [--workers ID,ID]
                  [--split local=1,ID=3] [--kv q8_0] [--kv-k TYPE] [--kv-v TYPE] [--kv-on-host]
                  [--flash auto|on|off] [--no-host-gpu]
       pool stop [NAME] | pool remove NAME | pool status`
	if len(args) == 0 {
		return errors.New(usage)
	}
	if !c.GroupHost {
		return errors.New("pools run on the group host; start one with: meshcompute host")
	}
	cl, e := security.Client(dir)
	if e != nil {
		return e
	}
	defer cl.CloseIdleConnections()
	bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var st agent.PoolsStatus
	if e := request(bounded, cl, c.LocalEndpoint(), "/v1/pools", nil, &st); e != nil {
		return fmt.Errorf("the group host is not running, or refused: %w", e)
	}
	find := func(name string) (config.PoolConfig, bool) {
		for _, p := range st.Pools {
			if p.Name == name || name == "" {
				return p.Settings, true
			}
		}
		return config.PoolConfig{}, false
	}
	var body any
	switch args[0] {
	case "status":
		if len(args) != 1 {
			return errors.New(usage)
		}
		return writeJSON(out, st)
	case "stop":
		if len(args) > 2 {
			return errors.New(usage)
		}
		name := ""
		if len(args) == 2 {
			name = args[1]
		}
		p, ok := find(name)
		if !ok {
			return errors.New("no such pool; see: meshcompute pool status")
		}
		p.Enabled = false
		body = map[string]any{"action": "set", "pool": p}
	case "remove":
		if len(args) != 2 {
			return errors.New(usage)
		}
		body = map[string]any{"action": "remove", "name": args[1]}
	case "start":
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return errors.New(usage)
		}
		p := config.PoolConfig{Name: agent.DefaultPoolName, Model: args[1], Enabled: true, Context: agent.DefaultPoolContext}
		var workers, split, kv, kvK, kvV string
		var noHost, kvHost bool
		if e := flags("pool start", args[2:], func(f *flag.FlagSet) {
			f.StringVar(&p.Name, "name", p.Name, "pool name")
			f.IntVar(&p.Context, "context", p.Context, "context size")
			f.IntVar(&p.Parallel, "slots", 0, "requests answered at once")
			f.StringVar(&workers, "workers", "", "worker IDs or unique prefixes, comma-separated")
			f.StringVar(&split, "split", "", "relative layer weights, e.g. local=1,worker-ab=3")
			f.StringVar(&kv, "kv", "", "KV cache type for K and V")
			f.StringVar(&kvK, "kv-k", "", "KV cache type for K")
			f.StringVar(&kvV, "kv-v", "", "KV cache type for V")
			f.BoolVar(&kvHost, "kv-on-host", false, "keep the KV cache in host RAM")
			f.StringVar(&p.FlashAttn, "flash", "", "flash attention: auto, on or off")
			f.BoolVar(&noHost, "no-host-gpu", false, "do not use this computer's GPU")
		}); e != nil {
			return fmt.Errorf("%w\n%s", e, usage)
		}
		p.NoHostGPU, p.KVOnHost = noHost, kvHost
		p.CacheTypeK, p.CacheTypeV = kv, kv
		if kvK != "" {
			p.CacheTypeK = kvK
		}
		if kvV != "" {
			p.CacheTypeV = kvV
		}
		known := append([]string{}, st.Sharing...)
		for _, pool := range st.Pools {
			known = append(known, pool.Settings.Workers...)
		}
		for _, w := range splitList(workers) {
			id, err := resolveWorker(w, known)
			if err != nil {
				return err
			}
			p.Workers = append(p.Workers, id)
		}
		if split != "" {
			p.Split = map[string]float64{}
			for _, part := range splitList(split) {
				k, v, ok := strings.Cut(part, "=")
				w, err := strconv.ParseFloat(v, 64)
				if !ok || err != nil {
					return errors.New("--split takes NODE=WEIGHT pairs, for example local=1,worker-ab=3")
				}
				if k != "local" {
					if k, err = resolveWorker(k, known); err != nil {
						return err
					}
				}
				p.Split[k] = w
			}
		}
		body = map[string]any{"action": "set", "pool": p}
	default:
		return errors.New(usage)
	}
	if e := request(bounded, cl, c.LocalEndpoint(), "/v1/pools", body, &st); e != nil {
		return fmt.Errorf("refused: %w", e)
	}
	return writeJSON(out, st)
}

// admitRun checks that st can answer r now (see agent.CanServe).
func admitRun(st agent.Status, statusErr error, r *backend.Request, explicitContext bool) string {
	if statusErr != nil {
		return "that computer's agent is not reachable: " + statusErr.Error()
	}
	return agent.CanServe(st, r, explicitContext)
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// resolveWorker expands a unique prefix (with or without "worker-") of a
// known worker ID.
func resolveWorker(prefix string, known []string) (string, error) {
	if !strings.HasPrefix(prefix, "worker-") {
		prefix = "worker-" + prefix
	}
	var match []string
	seen := map[string]bool{}
	for _, id := range known {
		if strings.HasPrefix(id, prefix) && !seen[id] {
			match = append(match, id)
			seen[id] = true
		}
	}
	switch len(match) {
	case 1:
		return match[0], nil
	case 0:
		if len(prefix) == 71 {
			return prefix, nil // a full ID of a worker that is offline now
		}
		return "", fmt.Errorf("no lending worker starts with %s; see: meshcompute pool status", prefix)
	default:
		return "", fmt.Errorf("%s matches %d workers; type more of the ID", prefix, len(match))
	}
}

func networkAdvice(q network.Quality) string {
	if !q.Available || !q.Stable {
		return "Unstable/unavailable: do not start remote inference; no automatic generation retry."
	}
	if q.Latency > 50*time.Millisecond || q.ThroughputBytesPerSecond < 50*1024*1024 {
		return "Slow link: a pooled model works but generates slowly and takes long to load; prefer one worker's own backend, or a faster link such as a direct mesh VPN connection."
	}
	return "Link is fast enough for pooled (llama.cpp RPC) inference."
}
func run(ctx context.Context, dir string, c config.Config, args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: run MODEL [--node local|auto|PEER] [--prompt TEXT]")
	}
	r := backend.Request{Model: args[0], MaxTokens: 128}
	node := "local"
	if e := flags("run", args[1:], func(f *flag.FlagSet) {
		f.StringVar(&node, "node", node, "placement")
		f.StringVar(&r.Prompt, "prompt", "", "prompt (stdin if absent)")
		f.IntVar(&r.MaxTokens, "max-tokens", 128, "output cap")
		f.IntVar(&r.Context, "context", 0, "context budget (default: up to 2048, within what the model's server allows)")
		f.StringVar(&r.Pool, "pool", "", "run on this named pool of the host (default: take turns over every ready pool serving the model)")
	}); e != nil {
		return e
	}
	explicitContext := r.Context != 0
	if !explicitContext {
		r.Context = 2048
	}
	if r.Prompt == "" {
		b, e := io.ReadAll(io.LimitReader(in, backend.MaxPromptBytes+1))
		if e != nil {
			return e
		}
		r.Prompt = string(b)
	}
	if strings.TrimSpace(r.Prompt) == "" || len(r.Prompt) > backend.MaxPromptBytes || r.MaxTokens < 1 || r.Context < 1 || r.MaxTokens >= r.Context {
		return errors.New("invalid prompt or token/context limits")
	}
	cl, e := security.Client(dir)
	if e != nil {
		return e
	}
	defer cl.CloseIdleConnections()
	cl.Timeout = backend.Timeout + 10*time.Second
	candidates := []string{node}
	if node == "auto" {
		candidates = []string{"local"}
		peers, err := dashboard.NewRemote(dir, c).Peers(ctx)
		if err != nil {
			return err
		}
		for _, p := range peers {
			candidates = append(candidates, p.Name)
		}
	}
	for _, name := range candidates {
		endpoint, e := dashboard.NewRemote(dir, c).Endpoint(ctx, name)
		if e != nil {
			return e
		}
		var st agent.Status
		bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
		e = request(bounded, cl, endpoint, "/v1/status", nil, &st)
		cancel()
		why := admitRun(st, e, &r, explicitContext)
		if why != "" {
			if node == "auto" {
				continue
			}
			return errors.New(why)
		}
		advice := "Local execution"
		if name != "local" {
			q, e := network.Measure(ctx, cl, endpoint)
			if e != nil || !q.Available || !q.Stable {
				if node == "auto" {
					continue
				}
				return errors.New("remote network failed admission; no inference started")
			}
			advice = networkAdvice(q)
		}
		var result agent.Result
		if e = request(ctx, cl, endpoint, "/v1/run", r, &result); e != nil {
			return e
		}
		mode := "local"
		if name != "local" {
			mode = "single_remote"
		}
		result.Mode = mode
		return writeJSON(out, map[string]any{"result": result, "network_policy": advice})
	}
	return errors.New("no healthy node has the requested model and context; no inference started")
}
func simulation(out io.Writer) error {
	now := time.Now()
	ram := uint64(8 << 30)
	s := resource.Snapshot{RAMAvailableBytes: &ram, CollectedAt: now}
	m := scheduler.Model{WeightBytes: 1 << 30, KVBytesPerToken: 65536, ContextTokens: 2048, OverheadBytes: 512 << 20, CPUOverheadBytes: 256 << 20}
	p := scheduler.DefaultPolicy()
	p.Now = now
	q := network.Quality{Available: true, Stable: true, MeasuredAt: now, Latency: 200 * time.Millisecond, ThroughputBytesPerSecond: 1 << 20, Requests: 4}
	w := scheduler.Worker{ID: "SIMULATED_remote", Available: true, LastSeen: now, Resources: s, Network: &q}
	slow := scheduler.Recommend([]scheduler.Worker{w}, m, p)
	w.Available = false
	failed := scheduler.Recommend([]scheduler.Worker{w}, m, p)
	return writeJSON(out, map[string]any{"simulation": true, "inference_performed": false, "slow_network": slow, "disconnected_worker": failed})
}
