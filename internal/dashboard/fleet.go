package dashboard

import (
	"context"
	"errors"
	"io"
	"meshcompute/internal/agent"
	"meshcompute/internal/security"
	"meshcompute/internal/task"
	"net/http"
	"sync"
	"time"
)

// historyLen is how many samples each computer keeps: three minutes at the
// page's two-second refresh.
const historyLen = 90

// Sample is one point of a computer's recent history.
type Sample struct {
	T     int64    `json:"t"`               // Unix milliseconds
	GPU   *float64 `json:"gpu,omitempty"`   // mean GPU utilization, %
	VRAM  *float64 `json:"vram,omitempty"`  // VRAM in use, %
	Temp  *float64 `json:"temp,omitempty"`  // hottest GPU, Celsius
	Power *float64 `json:"power,omitempty"` // all GPUs, watts
	CPU   *float64 `json:"cpu,omitempty"`   // %
	RAM   *float64 `json:"ram,omitempty"`   // RAM in use, %
	RTT   *float64 `json:"rtt,omitempty"`   // round trip from the host, ms
}

// FleetNode is the live picture of one computer: its last status, its link
// to the host, what the host is doing on it, and its recent history.
type FleetNode struct {
	Name      string        `json:"name"`
	Healthy   bool          `json:"healthy"`
	Error     string        `json:"error,omitempty"`
	Status    *agent.Status `json:"status,omitempty"`
	RTTMillis *float64      `json:"rtt_ms,omitempty"`
	LastSeen  time.Time     `json:"last_seen,omitempty"`
	// Tasks this dashboard sent to (host) or received on (worker) this node.
	TasksRunning int `json:"tasks_running"`
	TasksWaiting int `json:"tasks_waiting"`
	TasksDone    int `json:"tasks_done"`
	// Terminals open on it from this dashboard.
	Terminals int      `json:"terminals"`
	History   []Sample `json:"history"`
}

// FleetResponse is the body of GET /api/fleet.
type FleetResponse struct {
	Role      string      `json:"role"`
	SampledAt time.Time   `json:"sampled_at"`
	Nodes     []FleetNode `json:"nodes"`
}

// Pinger measures the round trip to a worker through its tunnel.
type Pinger interface {
	Ping(ctx context.Context, node string) (time.Duration, error)
}

type fleetEntry struct {
	node    FleetNode
	history []Sample
}

type fleet struct {
	mu       sync.Mutex
	entries  map[string]*fleetEntry
	order    []string
	at       time.Time
	sampling chan struct{} // non-nil while a sample runs
}

func (s *Server) fleetRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/fleet", s.handleFleet)
}

// handleFleet returns the latest picture. The page's polling drives
// sampling: at most one sample runs at a time, and a fresh one is taken when
// the last is older than 1.5 s, so nothing is polled while nobody watches.
func (s *Server) handleFleet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	f := &s.fleet
	f.mu.Lock()
	if f.entries == nil {
		f.entries = map[string]*fleetEntry{}
	}
	first := f.at.IsZero()
	wait := f.sampling
	if wait == nil && time.Since(f.at) > 1500*time.Millisecond {
		done := make(chan struct{})
		f.sampling, wait = done, done
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			s.sampleFleet(ctx)
			f.mu.Lock()
			f.sampling = nil
			f.mu.Unlock()
			close(done)
		}()
	}
	f.mu.Unlock()
	if first && wait != nil {
		select {
		case <-wait:
		case <-r.Context().Done():
			return
		}
	}
	writeJSON(w, s.fleetSnapshot())
}

func (s *Server) fleetSnapshot() FleetResponse {
	f := &s.fleet
	f.mu.Lock()
	defer f.mu.Unlock()
	out := FleetResponse{Role: s.role, SampledAt: f.at, Nodes: []FleetNode{}}
	for _, name := range f.order {
		e := f.entries[name]
		n := e.node
		n.History = append([]Sample{}, e.history...)
		out.Nodes = append(out.Nodes, n)
	}
	return out
}

// sampleFleet collects every node's status and round trip once.
func (s *Server) sampleFleet(ctx context.Context) {
	nodes, err := s.cluster.Statuses(ctx)
	if err != nil {
		return
	}
	rtts := make([]*float64, len(nodes))
	if p, ok := s.cluster.(Pinger); ok {
		var wg sync.WaitGroup
		for i, n := range nodes {
			if n.Name == "local" || !n.Healthy {
				continue
			}
			wg.Add(1)
			go func(i int, name string) {
				defer wg.Done()
				pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				if d, err := p.Ping(pctx, name); err == nil {
					ms := float64(d.Microseconds()) / 1000
					rtts[i] = &ms
				}
			}(i, n.Name)
		}
		wg.Wait()
	}
	running, waiting, done := map[string]int{}, map[string]int{}, map[string]int{}
	if list := s.taskList(); list != nil {
		for _, t := range list.List() {
			node := t.Node
			if node == "" {
				node = "local" // a worker's own tasks
			}
			switch {
			case t.State == task.StateRunning:
				running[node]++
			case t.State == task.StateApproval || t.State == task.StateSending:
				waiting[node]++
			case task.Final(t.State):
				done[node]++
			}
		}
	}
	terms := map[string]int{}
	if s.term != nil {
		for _, t := range s.term.List() {
			terms[t.Node]++
		}
	}
	now := time.Now().UTC()
	f := &s.fleet
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.entries == nil {
		f.entries = map[string]*fleetEntry{}
	}
	seen := map[string]bool{}
	f.order = f.order[:0]
	for i, n := range nodes {
		seen[n.Name] = true
		f.order = append(f.order, n.Name)
		e := f.entries[n.Name]
		if e == nil {
			e = &fleetEntry{}
			f.entries[n.Name] = e
		}
		last := e.node.LastSeen
		e.node = FleetNode{Name: n.Name, Healthy: n.Healthy, Error: n.Error, Status: n.Status, RTTMillis: rtts[i], LastSeen: last,
			TasksRunning: running[n.Name], TasksWaiting: waiting[n.Name], TasksDone: done[n.Name], Terminals: terms[n.Name]}
		if !n.Healthy || n.Status == nil {
			continue
		}
		e.node.LastSeen = now
		e.history = append(e.history, sample(now, n.Status, rtts[i]))
		if over := len(e.history) - historyLen; over > 0 {
			e.history = append(e.history[:0], e.history[over:]...)
		}
	}
	for name := range f.entries {
		if !seen[name] {
			delete(f.entries, name)
		}
	}
	f.at = now
}

func sample(now time.Time, st *agent.Status, rtt *float64) Sample {
	p := Sample{T: now.UnixMilli(), RTT: rtt, CPU: st.Resources.CPUPercent}
	var util, temp, power float64
	var nUtil, nTemp, nPower int
	var total, free uint64
	for _, g := range st.Resources.GPUs {
		if g.UtilizationPercent != nil {
			util += *g.UtilizationPercent
			nUtil++
		}
		if g.TemperatureC != nil {
			if nTemp == 0 || *g.TemperatureC > temp {
				temp = *g.TemperatureC
			}
			nTemp++
		}
		if g.PowerWatts != nil {
			power += *g.PowerWatts
			nPower++
		}
		if g.TotalBytes != nil && g.FreeBytes != nil && *g.FreeBytes <= *g.TotalBytes {
			total += *g.TotalBytes
			free += *g.FreeBytes
		}
	}
	if nUtil > 0 {
		v := util / float64(nUtil)
		p.GPU = &v
	}
	if nTemp > 0 {
		p.Temp = &temp
	}
	if nPower > 0 {
		p.Power = &power
	}
	if total > 0 {
		v := float64(total-free) * 100 / float64(total)
		p.VRAM = &v
	}
	r := st.Resources
	if r.RAMTotalBytes != nil && r.RAMAvailableBytes != nil && *r.RAMTotalBytes > 0 && *r.RAMAvailableBytes <= *r.RAMTotalBytes {
		v := float64(*r.RAMTotalBytes-*r.RAMAvailableBytes) * 100 / float64(*r.RAMTotalBytes)
		p.RAM = &v
	}
	return p
}

// Ping times one authenticated round trip to a worker through the host's
// agent and the worker's tunnel. The client is reused, so after the first
// call the time is the tunnel, not a new TLS handshake to the local agent.
func (r *Remote) Ping(ctx context.Context, node string) (time.Duration, error) {
	endpoint, err := r.endpoint(ctx, node)
	if err != nil {
		return 0, err
	}
	r.pingMu.Lock()
	if r.pingClient == nil {
		cl, err := security.Client(r.dir)
		if err != nil {
			r.pingMu.Unlock()
			return 0, err
		}
		r.pingClient = cl
	}
	cl := r.pingClient
	r.pingMu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/v1/ping", nil)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	res, err := cl.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return 0, errors.New("ping failed")
	}
	return time.Since(start), nil
}
