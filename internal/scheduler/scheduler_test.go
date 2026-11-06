package scheduler

import (
	"math"
	"meshcompute/internal/network"
	"meshcompute/internal/resource"
	"testing"
	"time"
)

func ptr(n uint64) *uint64 { return &n }
func fixture() (Worker, Model, Policy) {
	p := DefaultPolicy()
	p.Now = time.Now()
	return Worker{ID: "w", Local: true, Available: true, LastSeen: p.Now, Resources: resource.Snapshot{CollectedAt: p.Now, RAMAvailableBytes: ptr(1000), GPUDiscoveryAvailable: true, GPUs: []resource.GPU{{Index: 0, FreeBytes: ptr(1000)}}}, Network: &network.Quality{MeasuredAt: p.Now, Available: true, Stable: true, Requests: 4, Latency: time.Millisecond, ThroughputBytesPerSecond: 100 * 1024 * 1024}}, Model{WeightBytes: 400, KVBytesPerToken: 10, ContextTokens: 10, OverheadBytes: 100, CPUOverheadBytes: 50}, p
}
func TestRecommend(t *testing.T) {
	w, m, p := fixture()
	r := Recommend([]Worker{w}, m, p)
	if r.Mode != "local" || r.Selected == nil || r.Selected.Device != "gpu" || r.Selected.RequiredBytes != 600 || r.Selected.CPURequiredBytes != 50 || r.Selected.BudgetBytes != 800 || r.RPCReady {
		t.Fatalf("%+v", r)
	}
}
func TestNoPooledMemory(t *testing.T) {
	w, m, p := fixture()
	w.Resources.RAMAvailableBytes = ptr(500)
	w.Resources.GPUs = []resource.GPU{{FreeBytes: ptr(500)}, {FreeBytes: ptr(500)}}
	w2 := w
	w2.ID = "w2"
	r := Recommend([]Worker{w, w2}, m, p)
	if r.Selected != nil {
		t.Fatal("pooled memory")
	}
}
func TestReject(t *testing.T) {
	cases := map[string]func(*Worker){"offline": func(w *Worker) { w.Available = false }, "stale worker": func(w *Worker) { w.LastSeen = w.LastSeen.Add(-time.Minute) }, "stale resource": func(w *Worker) { w.Resources.CollectedAt = time.Time{} }, "future": func(w *Worker) { w.LastSeen = w.LastSeen.Add(time.Minute) }, "network missing": func(w *Worker) { w.Local = false; w.Network = nil }, "unstable": func(w *Worker) { w.Local = false; w.Network.Stable = false }, "request failures": func(w *Worker) { w.Local = false; w.Network.Failures = 1 }, "unmeasured": func(w *Worker) { w.Local = false; w.Network.Requests = 0 }, "network stale": func(w *Worker) { w.Local = false; w.Network.MeasuredAt = time.Time{} }, "nan": func(w *Worker) { w.Local = false; w.Network.ThroughputBytesPerSecond = math.NaN() }, "unknown memory": func(w *Worker) { w.Resources.RAMAvailableBytes = nil; w.Resources.GPUs = nil }}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			w, m, p := fixture()
			mutate(&w)
			r := Recommend([]Worker{w}, m, p)
			if r.Selected != nil || r.Rejected[w.ID] == "" {
				t.Fatalf("%+v", r)
			}
		})
	}
}
func TestSlowAndLocalPreference(t *testing.T) {
	w, m, p := fixture()
	w.Local = false
	w.Network.Latency = time.Second
	r := Recommend([]Worker{w}, m, p)
	if r.Mode != "single_remote" || !r.Selected.SlowNetwork || r.RPCReady {
		t.Fatalf("%+v", r)
	}
	local, _, _ := fixture()
	local.ID = "local"
	local.LastSeen = p.Now
	local.Resources.CollectedAt = p.Now
	r = Recommend([]Worker{w, local}, m, p)
	if r.Mode != "local" || r.Selected.WorkerID != "local" {
		t.Fatal(r)
	}
}
func TestCPUFallbackAndOverhead(t *testing.T) {
	w, m, p := fixture()
	w.Resources.GPUDiscoveryAvailable = false
	r := Recommend([]Worker{w}, m, p)
	if r.Selected == nil || r.Selected.Device != "cpu" || r.Selected.CPURequiredBytes != 650 {
		t.Fatal(r)
	}
	w.Resources.GPUDiscoveryAvailable = true
	m.CPUOverheadBytes = 900
	if Recommend([]Worker{w}, m, p).Selected != nil {
		t.Fatal("ignored host overhead")
	}
}
func TestOverflowAndPolicy(t *testing.T) {
	w, m, p := fixture()
	for _, bad := range []Model{{}, {WeightBytes: 1, KVBytesPerToken: math.MaxUint64, ContextTokens: 2}, {WeightBytes: math.MaxUint64, KVBytesPerToken: 1, ContextTokens: 1}} {
		if _, e := RequiredBytes(bad); e == nil {
			t.Fatal("accepted invalid model")
		}
	}
	for _, s := range []float64{-1, 0, 1, math.NaN(), math.Inf(1)} {
		p.SafetyFraction = s
		if Recommend([]Worker{w}, m, p).Selected != nil {
			t.Fatal("accepted invalid safety")
		}
	}
}
func TestBudget(t *testing.T) {
	if budget(ptr(101), 1, .2) != 80 || budget(nil, 0, .2) != 0 || budget(ptr(10), 10, .2) != 0 || budget(ptr(1), 0, .2) != 0 {
		t.Fatal("budget rounding")
	}
}

func TestAdmitAndSimulate(t *testing.T) {
	w, m, p := fixture()
	if _, e := Admit(w, m, p); e != nil {
		t.Fatal(e)
	}
	w.Available = false
	if _, e := Admit(w, m, p); e == nil {
		t.Fatal("admitted offline worker")
	}
	if _, e := Admit(w, Model{}, p); e == nil {
		t.Fatal("admitted invalid model")
	}
	examples := Simulate()
	want := []string{"local", "single_remote", "unavailable", "unavailable"}
	if len(examples) != len(want) {
		t.Fatal(examples)
	}
	for i, e := range examples {
		if !e.Synthetic || e.Recommendation.RPCReady || e.Recommendation.Mode != want[i] {
			t.Fatalf("%+v", e)
		}
	}
}
