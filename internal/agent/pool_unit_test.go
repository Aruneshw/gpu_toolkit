package agent

import (
	"meshcompute/internal/backend"
	"meshcompute/internal/config"
	"reflect"
	"strings"
	"testing"
)

func TestAssignWorkersToPools(t *testing.T) {
	sharing := []string{"worker-a", "worker-b", "worker-c", "worker-d"}
	pools := []config.PoolConfig{
		{Name: "named", Enabled: true, Workers: []string{"worker-b", "worker-x"}},
		{Name: "auto1", Enabled: true},
		{Name: "auto2", Enabled: true},
		{Name: "off", Enabled: false, Workers: []string{"worker-c"}},
	}
	got := assign(pools, sharing)
	// worker-x is not connected. worker-c is named by a disabled pool: a
	// worker named by any pool is never given away automatically.
	want := map[string][]string{
		"named": {"worker-b"},
		"auto1": {"worker-a", "worker-d"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if _, ok := got["auto2"]; ok {
		t.Fatal("only the first automatic pool takes unnamed workers")
	}
}

func TestDevicesAndSplit(t *testing.T) {
	w := []string{"worker-a", "worker-b"}
	cases := []struct {
		name    string
		c       config.PoolConfig
		local   []string
		workers []string
		devices string
		split   []float64
	}{
		{"cpu host alone", config.PoolConfig{}, nil, nil, "", nil},
		{"gpu host alone", config.PoolConfig{}, []string{"Vulkan0"}, nil, "", nil},
		{"gpu host excluded, no workers", config.PoolConfig{NoHostGPU: true}, []string{"Vulkan0"}, nil, "none", nil},
		{"workers, automatic split", config.PoolConfig{}, []string{"Vulkan0"}, w, "Vulkan0,RPC0,RPC1", nil},
		{"workers, host excluded", config.PoolConfig{NoHostGPU: true}, []string{"Vulkan0"}, w, "RPC0,RPC1", nil},
		{"manual split, missing weight is 1", config.PoolConfig{Split: map[string]float64{"local": 2, "worker-b": 3}}, []string{"Vulkan0", "Vulkan1"}, w, "Vulkan0,Vulkan1,RPC0,RPC1", []float64{1, 1, 1, 3}},
		{"manual split, cpu host", config.PoolConfig{Split: map[string]float64{"worker-a": 0, "worker-b": 2}}, nil, w, "RPC0,RPC1", []float64{0, 2}},
	}
	for _, tc := range cases {
		devices, split, err := devicesFor(tc.c, tc.local, tc.workers)
		if err != nil || strings.Join(devices, ",") != tc.devices || !reflect.DeepEqual(split, tc.split) {
			t.Errorf("%s: devices %v split %v err %v", tc.name, devices, split, err)
		}
		if len(split) > 0 {
			if _, err := backend.Args(backend.LaunchConfig{Endpoint: "http://127.0.0.1:1", Context: 512, Devices: devices, TensorSplit: split}, "/m", "m"); err != nil {
				t.Errorf("%s: llama-server arguments refused: %v", tc.name, err)
			}
		}
	}
	if _, _, err := devicesFor(config.PoolConfig{Split: map[string]float64{"local": 0, "worker-a": 0, "worker-b": 0}}, []string{"Vulkan0"}, w); err == nil {
		t.Fatal("all-zero split accepted")
	}
}

// route takes turns between ready pools serving a model and never exceeds
// a pool's request slots.
func TestRouteSlotsAndRoundRobin(t *testing.T) {
	s := &poolSet{rr: map[string]int{}, kick: make(chan struct{}, 1)}
	mk := func(name string, parallel int) *pool {
		p := s.newPool(config.PoolConfig{Name: name, Model: "m", Enabled: true, Parallel: parallel})
		p.state = "ready"
		p.proc = &poolProcess{endpoint: "http://127.0.0.1:1/" + name, done: make(chan struct{}), alloc: &backend.Allocation{}}
		return p
	}
	s.pools = []*pool{mk("a", 2), mk("b", 1)}
	var used []string
	var releases []func()
	for i := 0; i < 3; i++ {
		endpoint, st, release, ok, err := s.route("m", 512, "")
		if !ok || err != nil {
			t.Fatalf("request %d: %v %v", i, ok, err)
		}
		used = append(used, st.Name)
		_ = endpoint
		releases = append(releases, release)
	}
	if strings.Join(used, ",") != "a,b,a" {
		t.Fatalf("turns: %v", used)
	}
	if _, _, _, ok, err := s.route("m", 512, ""); !ok || err == nil {
		t.Fatal("a fourth request must be refused while all 3 slots are busy")
	}
	releases[1]()
	releases[1]() // releasing twice must not free two slots
	if _, st, release, _, err := s.route("m", 512, ""); err != nil || st.Name != "b" {
		t.Fatalf("freed slot not reused: %v %v", st.Name, err)
	} else {
		release()
	}
	if _, _, _, ok, _ := s.route("other", 512, ""); ok {
		t.Fatal("a model no pool serves must fall through to the plain backend")
	}
}

// A request goes only to pools whose slots hold its context.
func TestRouteByContext(t *testing.T) {
	s := &poolSet{rr: map[string]int{}, kick: make(chan struct{}, 1)}
	mk := func(name string, ctx, parallel int) *pool {
		p := s.newPool(config.PoolConfig{Name: name, Model: "m", Enabled: true, Context: ctx, Parallel: parallel})
		p.state = "ready"
		p.proc = &poolProcess{endpoint: "http://127.0.0.1:1/" + name, done: make(chan struct{}), alloc: &backend.Allocation{}}
		return p
	}
	s.pools = []*pool{mk("small", 1024, 1), mk("big", 4096, 2)}
	for i := 0; i < 4; i++ {
		_, st, release, ok, err := s.route("m", 2048, "")
		if !ok || err != nil || st.Name != "big" {
			t.Fatalf("a 2048-token request went to %q (%v)", st.Name, err)
		}
		release()
	}
	if _, _, _, ok, err := s.route("m", 4096, ""); !ok || err == nil || !strings.Contains(err.Error(), "2048 tokens") {
		t.Fatalf("too large a context: ok=%v err=%v", ok, err)
	}
	used := map[string]int{}
	for i := 0; i < 4; i++ {
		_, st, release, _, _ := s.route("m", 512, "")
		used[st.Name]++
		release()
	}
	if used["small"] != 2 || used["big"] != 2 {
		t.Fatalf("small requests must still alternate: %v", used)
	}
}

// A request naming a pool runs there or is refused with the reason; it never
// moves to another replica of the same model or to the plain backend.
func TestRouteNamedPool(t *testing.T) {
	s := &poolSet{rr: map[string]int{}, kick: make(chan struct{}, 1)}
	mk := func(name, model, state string, parallel int) *pool {
		p := s.newPool(config.PoolConfig{Name: name, Model: model, Enabled: true, Context: 2048, Parallel: parallel})
		p.state = state
		p.proc = &poolProcess{endpoint: "http://127.0.0.1:1/" + name, done: make(chan struct{}), alloc: &backend.Allocation{}}
		return p
	}
	s.pools = []*pool{mk("a", "m", "ready", 2), mk("b", "m", "ready", 1), mk("c", "other", "ready", 1), mk("d", "m", "starting", 1)}
	for i := 0; i < 3; i++ {
		_, st, release, ok, err := s.route("m", 512, "b")
		if !ok || err != nil || st.Name != "b" {
			t.Fatalf("named run %d went to %q: %v", i, st.Name, err)
		}
		release()
	}
	_, _, release, _, _ := s.route("m", 512, "b")
	if _, _, _, ok, err := s.route("m", 512, "b"); !ok || err == nil || !strings.Contains(err.Error(), `pool "b" is busy`) && !strings.Contains(err.Error(), `of pool "b" is busy`) {
		t.Fatalf("busy named pool must refuse, not spill to pool a: ok=%v err=%v", ok, err)
	}
	release()
	for name, want := range map[string]string{
		"nope": `no pool named "nope" (pools: a, b, c, d)`,
		"c":    `pool "c" serves other, not m`,
		"d":    `pool "d" is not ready (starting)`,
	} {
		if _, _, _, ok, err := s.route("m", 512, name); !ok || err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("pool %s: ok=%v err=%v, want %q", name, ok, err, want)
		}
	}
	if _, _, _, ok, err := s.route("m", 2048, "a"); !ok || err == nil || !strings.Contains(err.Error(), "1024 tokens") {
		t.Fatalf("context above pool a's slot: ok=%v err=%v", ok, err)
	}
}

func TestCanServeNamedPool(t *testing.T) {
	st := Status{BackendReady: true, Backend: &backend.Capabilities{Models: []string{"m"}, Context: 2048},
		Pools: []PoolStatus{
			{Name: "a", State: "ready", Model: "m", Settings: config.PoolConfig{Context: 2048, Parallel: 2}},
			{Name: "b", State: "starting", Model: "m", Settings: config.PoolConfig{Context: 2048}},
		}}
	r := backend.Request{Model: "m", Pool: "a", MaxTokens: 16, Context: 2048}
	if why := CanServe(st, &r, false); why != "" || r.Context != 1024 {
		t.Fatalf("implicit context must shrink to pool a's slot: %q %d", why, r.Context)
	}
	for _, tc := range []struct {
		r    backend.Request
		want string
	}{
		{backend.Request{Model: "m", Pool: "a", MaxTokens: 16, Context: 2048}, "use --context 1024"},
		{backend.Request{Model: "m", Pool: "b", MaxTokens: 16, Context: 512}, "pool b is not ready (starting)"},
		{backend.Request{Model: "m", Pool: "x", MaxTokens: 16, Context: 512}, "no pool named x there (pools: a, b)"},
	} {
		if why := CanServe(st, &tc.r, true); !strings.Contains(why, tc.want) {
			t.Fatalf("%+v: %q, want %q", tc.r, why, tc.want)
		}
	}
}
