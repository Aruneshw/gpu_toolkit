package scheduler

import (
	"fmt"
	"math"
	"meshcompute/internal/network"
	"meshcompute/internal/resource"
	"time"
)

type Model struct {
	WeightBytes      uint64 `json:"weight_bytes"`
	KVBytesPerToken  uint64 `json:"kv_bytes_per_token"`
	ContextTokens    uint64 `json:"context_tokens"`
	OverheadBytes    uint64 `json:"overhead_bytes"`
	CPUOverheadBytes uint64 `json:"cpu_overhead_bytes"`
}
type Worker struct {
	ID        string            `json:"id"`
	Local     bool              `json:"local"`
	Available bool              `json:"available"`
	LastSeen  time.Time         `json:"last_seen"`
	Resources resource.Snapshot `json:"resources"`
	Network   *network.Quality  `json:"network"`
}
type Policy struct {
	Now                         time.Time     `json:"now"`
	MaxAge                      time.Duration `json:"max_age_ns"`
	SafetyFraction              float64       `json:"safety_fraction"`
	CPUReserveBytes             uint64        `json:"cpu_reserve_bytes"`
	GPUReserveBytes             uint64        `json:"gpu_reserve_bytes"`
	MaxLatency                  time.Duration `json:"max_latency_ns"`
	MinThroughputBytesPerSecond float64       `json:"min_throughput_bytes_per_second"`
}
type Candidate struct {
	WorkerID         string `json:"worker_id"`
	Device           string `json:"device"`
	GPUIndex         *int   `json:"gpu_index"`
	RequiredBytes    uint64 `json:"required_bytes"`
	CPURequiredBytes uint64 `json:"cpu_required_bytes"`
	BudgetBytes      uint64 `json:"budget_bytes"`
	CPUBudgetBytes   uint64 `json:"cpu_budget_bytes"`
	SlowNetwork      bool   `json:"slow_network"`
}
type Recommendation struct {
	Mode       string            `json:"mode"`
	Selected   *Candidate        `json:"selected"`
	Candidates []Candidate       `json:"candidates"`
	Rejected   map[string]string `json:"rejected"`
	Reasons    []string          `json:"reasons"`
	RPCReady   bool              `json:"rpc_ready"`
}

func DefaultPolicy() Policy {
	return Policy{MaxAge: 30 * time.Second, SafetyFraction: 0.2, MaxLatency: 50 * time.Millisecond, MinThroughputBytesPerSecond: 50 * 1024 * 1024}
}

// Admit checks planning eligibility only; it does not reserve memory or enable RPC.
func Admit(w Worker, m Model, p Policy) (Candidate, error) {
	r := Recommend([]Worker{w}, m, p)
	if r.Selected != nil {
		return *r.Selected, nil
	}
	if reason := r.Rejected[w.ID]; reason != "" {
		return Candidate{}, fmt.Errorf("worker %s rejected: %s", w.ID, reason)
	}
	return Candidate{}, fmt.Errorf("admission rejected: %v", r.Reasons)
}

type Simulation struct {
	Name           string         `json:"name"`
	Synthetic      bool           `json:"synthetic"`
	Recommendation Recommendation `json:"recommendation"`
}

// Simulate uses explicitly synthetic resources, never discovery or real execution.
func Simulate() []Simulation {
	p := DefaultPolicy()
	p.Now = time.Now().UTC()
	gib := uint64(1024 * 1024 * 1024)
	ram := 16 * gib
	vram := 8 * gib
	m := Model{WeightBytes: 4 * gib, KVBytesPerToken: 65536, ContextTokens: 4096, OverheadBytes: gib, CPUOverheadBytes: gib}
	w := Worker{ID: "synthetic-local", Local: true, Available: true, LastSeen: p.Now, Resources: resource.Snapshot{CollectedAt: p.Now, RAMAvailableBytes: &ram, GPUDiscoveryAvailable: true, GPUs: []resource.GPU{{Index: 0, FreeBytes: &vram}}}}
	out := []Simulation{{Name: "local-fit", Synthetic: true, Recommendation: Recommend([]Worker{w}, m, p)}}
	w.Local = false
	w.ID = "synthetic-remote"
	w.Network = &network.Quality{MeasuredAt: p.Now, Requests: 4, Available: true, Stable: true, Latency: 150 * time.Millisecond, ThroughputBytesPerSecond: 2 * 1024 * 1024}
	out = append(out, Simulation{Name: "slow-network-single-remote", Synthetic: true, Recommendation: Recommend([]Worker{w}, m, p)})
	w.Network.Stable = false
	w.Network.Failures = 1
	w.Network.RequestFailureRate = .25
	out = append(out, Simulation{Name: "unstable-network-rejected", Synthetic: true, Recommendation: Recommend([]Worker{w}, m, p)})
	w.Local = true
	ram = 2 * gib
	vram = 2 * gib
	w.Resources.GPUs = append(w.Resources.GPUs, resource.GPU{Index: 1, FreeBytes: &vram})
	return append(out, Simulation{Name: "no-memory-pooling", Synthetic: true, Recommendation: Recommend([]Worker{w}, m, p)})
}

func RequiredBytes(m Model) (uint64, error) {
	if m.WeightBytes == 0 || m.ContextTokens == 0 || m.KVBytesPerToken == 0 {
		return 0, fmt.Errorf("weights, context and KV bytes/token must be explicit positive estimates")
	}
	if m.ContextTokens > math.MaxUint64/m.KVBytesPerToken {
		return 0, fmt.Errorf("KV estimate overflow")
	}
	n := m.ContextTokens * m.KVBytesPerToken
	for _, v := range []uint64{m.WeightBytes, m.OverheadBytes} {
		if v > math.MaxUint64-n {
			return 0, fmt.Errorf("memory estimate overflow")
		}
		n += v
	}
	return n, nil
}
func budget(free *uint64, reserve uint64, safety float64) uint64 {
	if free == nil || *free <= reserve {
		return 0
	}
	n := *free - reserve
	// Round the withheld reserve up, never the usable budget up.
	withheld := math.Ceil(float64(n) * safety)
	if withheld >= float64(n) {
		return 0
	}
	return n - uint64(withheld)
}

// Recommend considers one device on one worker; RAM and VRAM are never pooled.
func Recommend(workers []Worker, m Model, p Policy) Recommendation {
	r := Recommendation{Mode: "unavailable", Candidates: []Candidate{}, Rejected: map[string]string{}, Reasons: []string{"Planning only: RPC execution is not implemented or ready."}}
	if p == (Policy{}) {
		p = DefaultPolicy()
	}
	if p.Now.IsZero() {
		p.Now = time.Now()
	}
	if p.MaxAge <= 0 || p.SafetyFraction <= 0 || p.SafetyFraction >= 1 || math.IsNaN(p.SafetyFraction) || p.MaxLatency <= 0 || p.MinThroughputBytesPerSecond <= 0 || math.IsNaN(p.MinThroughputBytesPerSecond) || math.IsInf(p.MinThroughputBytesPerSecond, 0) {
		r.Reasons = append(r.Reasons, "invalid scheduling policy")
		return r
	}
	required, e := RequiredBytes(m)
	if e != nil {
		r.Reasons = append(r.Reasons, e.Error())
		return r
	}
	fresh := func(t time.Time) bool { return !t.IsZero() && !t.After(p.Now) && p.Now.Sub(t) <= p.MaxAge }
	for _, w := range workers {
		reject := func(reason string) { r.Rejected[w.ID] = reason }
		if !w.Available {
			reject("worker unavailable")
			continue
		}
		if !fresh(w.LastSeen) || !fresh(w.Resources.CollectedAt) {
			reject("worker or resource measurements stale or missing")
			continue
		}
		slow := false
		if !w.Local {
			q := w.Network
			if q == nil || !q.Available || !q.Stable || q.Failures != 0 || q.RequestFailureRate != 0 || q.Requests < network.PingSamples+1 || q.Latency <= 0 || q.Jitter < 0 || q.Jitter > 100*time.Millisecond || q.ThroughputBytesPerSecond <= 0 || math.IsNaN(q.ThroughputBytesPerSecond) || math.IsInf(q.ThroughputBytesPerSecond, 0) || !fresh(q.MeasuredAt) {
				reject("network unavailable, unstable, stale or unmeasured")
				continue
			}
			slow = q.Latency > p.MaxLatency || q.ThroughputBytesPerSecond < p.MinThroughputBytesPerSecond
		}
		cpu := budget(w.Resources.RAMAvailableBytes, p.CPUReserveBytes, p.SafetyFraction)
		before := len(r.Candidates)
		if w.Resources.GPUDiscoveryAvailable && w.Resources.RAMAvailableBytes != nil && cpu >= m.CPUOverheadBytes {
			for _, g := range w.Resources.GPUs {
				b := budget(g.FreeBytes, p.GPUReserveBytes, p.SafetyFraction)
				if b >= required {
					index := g.Index
					r.Candidates = append(r.Candidates, Candidate{WorkerID: w.ID, Device: "gpu", GPUIndex: &index, RequiredBytes: required, CPURequiredBytes: m.CPUOverheadBytes, BudgetBytes: b, CPUBudgetBytes: cpu, SlowNetwork: slow})
				}
			}
		}
		if m.CPUOverheadBytes <= math.MaxUint64-required && cpu >= required+m.CPUOverheadBytes {
			r.Candidates = append(r.Candidates, Candidate{WorkerID: w.ID, Device: "cpu", RequiredBytes: required, CPURequiredBytes: required + m.CPUOverheadBytes, BudgetBytes: cpu, CPUBudgetBytes: cpu, SlowNetwork: slow})
		}
		if len(r.Candidates) == before {
			reject("insufficient known CPU or individual GPU memory budget")
		}
	}
	// Prefer local execution even when a remote device has more memory.
	for _, local := range []bool{true, false} {
		for i := range r.Candidates {
			c := &r.Candidates[i]
			for _, w := range workers {
				if w.ID == c.WorkerID && w.Local == local {
					r.Selected = c
					if local {
						r.Mode = "local"
					} else {
						r.Mode = "single_remote"
					}
					if c.SlowNetwork {
						r.Reasons = append(r.Reasons, "Slow network: use local execution or one remote worker; do not split a model across workers.")
					}
					return r
				}
			}
		}
	}
	r.Reasons = append(r.Reasons, "No single device fits; memory across workers or CPU/GPU is not summed.")
	return r
}
