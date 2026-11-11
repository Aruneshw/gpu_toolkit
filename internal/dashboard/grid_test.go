package dashboard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"meshcompute/internal/agent"
	"meshcompute/internal/backend"
	"meshcompute/internal/resource"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fleetCluster reports a host and two workers; one worker goes offline.
type fleetCluster struct {
	calls   atomic.Int32
	offline atomic.Bool
	mu      sync.Mutex
	pings   []string
}

func (f *fleetCluster) Statuses(context.Context) ([]Node, error) {
	n := float64(f.calls.Add(1))
	gpu := func(util, temp, power float64) resource.GPU {
		return resource.GPU{Name: "RTX", TotalBytes: u64(8 << 30), FreeBytes: u64(2 << 30), UtilizationPercent: f64(util), TemperatureC: f64(temp), PowerWatts: f64(power)}
	}
	st := func(id string, gpus ...resource.GPU) *agent.Status {
		return &agent.Status{NodeID: id, Resources: resource.Snapshot{GPUs: gpus, CPUPercent: f64(10 * n), RAMTotalBytes: u64(16 << 30), RAMAvailableBytes: u64(12 << 30)}}
	}
	nodes := []Node{
		{Name: "local", Healthy: true, Status: st("local", gpu(n, 50, 30))},
		{Name: "worker-a", Healthy: true, Status: st("worker-a", gpu(20, 60, 40), gpu(40, 70, 60))},
		{Name: "worker-b", Healthy: true, Status: st("worker-b")},
	}
	if f.offline.Load() {
		nodes[2] = Node{Name: "worker-b", Error: "offline"}
	}
	return nodes, nil
}

func (f *fleetCluster) Stream(context.Context, string, backend.Request, func(backend.Chunk) error) error {
	return errors.New("unused")
}

func (f *fleetCluster) Ping(_ context.Context, node string) (time.Duration, error) {
	f.mu.Lock()
	f.pings = append(f.pings, node)
	f.mu.Unlock()
	return 12500 * time.Microsecond, nil
}

func getFleet(t *testing.T, s *Server) FleetResponse {
	t.Helper()
	rec := request(s, http.MethodGet, "/api/fleet", nil, auth)
	if rec.Code != http.StatusOK {
		t.Fatalf("fleet: %d %s", rec.Code, rec.Body)
	}
	var out FleetResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFleetSamplesHistoryAndLinks(t *testing.T) {
	cl := &fleetCluster{}
	s := newServer(t, cl)
	first := getFleet(t, s)
	if len(first.Nodes) != 3 || first.Role != "local" {
		t.Fatalf("%+v", first)
	}
	a := first.Nodes[1]
	if a.Name != "worker-a" || !a.Healthy || a.RTTMillis == nil || *a.RTTMillis != 12.5 || len(a.History) != 1 {
		t.Fatalf("worker-a %+v", a)
	}
	h := a.History[0]
	if *h.GPU != 30 || *h.Temp != 70 || *h.Power != 100 || *h.VRAM != 75 || *h.RAM != 25 || *h.RTT != 12.5 {
		t.Fatalf("sample %+v gpu=%v temp=%v power=%v vram=%v ram=%v", h, *h.GPU, *h.Temp, *h.Power, *h.VRAM, *h.RAM)
	}
	if first.Nodes[0].RTTMillis != nil {
		t.Fatal("the host itself must not be pinged")
	}
	if b := first.Nodes[2]; b.History[0].GPU != nil || b.History[0].VRAM != nil {
		t.Fatalf("a GPU-less worker got GPU readings: %+v", b.History[0])
	}
	// Polls closer than 1.5 s reuse the sample; later ones add history.
	if again := getFleet(t, s); len(again.Nodes[1].History) != 1 || cl.calls.Load() != 1 {
		t.Fatalf("resampled too soon: calls=%d", cl.calls.Load())
	}
	s.fleet.mu.Lock()
	s.fleet.at = time.Now().Add(-2 * time.Second)
	s.fleet.mu.Unlock()
	idle := func(calls int32) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			s.fleet.mu.Lock()
			busy := s.fleet.sampling != nil
			s.fleet.mu.Unlock()
			if cl.calls.Load() >= calls && !busy {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("background sample never finished")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	getFleet(t, s) // starts a background sample
	idle(2)
	cl.offline.Store(true)
	s.fleet.mu.Lock()
	s.fleet.at = time.Now().Add(-2 * time.Second)
	s.fleet.mu.Unlock()
	getFleet(t, s)
	idle(3)
	out := getFleet(t, s)
	if len(out.Nodes[1].History) != 3 || *out.Nodes[0].History[2].GPU != 3 {
		t.Fatalf("history %+v", out.Nodes[1].History)
	}
	b := out.Nodes[2]
	if b.Healthy || b.Error != "offline" || len(b.History) != 2 || b.LastSeen.IsZero() {
		t.Fatalf("offline worker must keep its history and last-seen time: %+v", b)
	}
	// Three rounds: both workers twice, then only the one still online. The
	// host itself is never pinged.
	cl.mu.Lock()
	defer cl.mu.Unlock()
	count := map[string]int{}
	for _, p := range cl.pings {
		count[p]++
	}
	if count["local"] != 0 || count["worker-a"] != 3 || count["worker-b"] != 2 {
		t.Fatalf("pings %v", cl.pings)
	}
}

func TestFleetHistoryIsBounded(t *testing.T) {
	cl := &fleetCluster{}
	s := newServer(t, cl)
	for i := 0; i < historyLen+15; i++ {
		s.sampleFleet(context.Background())
	}
	out := s.fleetSnapshot()
	if got := len(out.Nodes[0].History); got != historyLen {
		t.Fatalf("history length %d", got)
	}
	if last := out.Nodes[0].History[historyLen-1]; *last.GPU != float64(historyLen+15) {
		t.Fatalf("history must keep the newest samples, last GPU %v", *last.GPU)
	}
}

// TestTerminalGridOverOneStream opens three shells, follows all of them over
// one multiplexed stream, types into two at once, and checks that each
// shell's output arrives tagged with its own ID and that exits are reported.
func TestTerminalGridOverOneStream(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("pseudo-terminal backend is Linux only")
	}
	t.Setenv("SHELL", "/bin/sh")
	s := newServer(t, &fakeCluster{})
	s.EnableTerminal(t.TempDir())
	defer s.term.CloseAll()
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	do := func(method, path, body string) *http.Response {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Host = "127.0.0.1:8081"
		for k, v := range auth {
			req.Header.Set(k, v)
		}
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	var ids []string
	for i := 0; i < 3; i++ {
		res := do(http.MethodPost, "/api/term/open", `{"cols":80,"rows":24}`)
		var opened struct{ ID, Node string }
		_ = json.NewDecoder(res.Body).Decode(&opened)
		res.Body.Close()
		if res.StatusCode != 200 || opened.Node != "local" {
			t.Fatalf("open %d: %d %+v", i, res.StatusCode, opened)
		}
		ids = append(ids, opened.ID)
	}
	res := do(http.MethodGet, "/api/term/list", "")
	var listed struct {
		Sessions []struct{ ID, Node string } `json:"sessions"`
	}
	_ = json.NewDecoder(res.Body).Decode(&listed)
	res.Body.Close()
	if len(listed.Sessions) != 3 || listed.Sessions[0].Node != "local" {
		t.Fatalf("list %+v", listed)
	}
	stream := do(http.MethodGet, "/api/term/multi?s="+ids[0]+":0,"+ids[1]+":0,"+ids[2]+":0,ffffffffffffffffffffffffffffffff:0", "")
	defer stream.Body.Close()
	out := map[string]*strings.Builder{}
	for _, id := range ids {
		out[id] = &strings.Builder{}
	}
	codes := map[string]int{}
	var mu sync.Mutex
	done := make(chan error, 1)
	go func() {
		done <- readEvents(stream.Body, func(event string, data []byte) bool {
			var v struct {
				ID   string `json:"id"`
				Data string `json:"data"`
				Code int    `json:"code"`
				Gone bool   `json:"gone"`
			}
			_ = json.Unmarshal(data, &v)
			mu.Lock()
			defer mu.Unlock()
			switch event {
			case "output":
				b, _ := base64.StdEncoding.DecodeString(v.Data)
				out[v.ID].Write(b)
			case "exit":
				codes[v.ID] = v.Code
				if len(codes) == 4 {
					return false
				}
			}
			return true
		})
	}()
	// Broadcast to the first two, then a private command to each.
	if res := do(http.MethodPost, "/api/term/input", `{"ids":["`+ids[0]+`","`+ids[1]+`"],"data":"echo both-$((1+1))\n"}`); res.StatusCode != 204 {
		t.Fatalf("broadcast: %d", res.StatusCode)
	}
	for i, id := range ids {
		body := `{"id":"` + id + `","data":"echo only-` + string(rune('a'+i)) + `; exit ` + string(rune('3'+i)) + `\n"}`
		if res := do(http.MethodPost, "/api/term/input", body); res.StatusCode != 204 {
			t.Fatalf("input %d: %d", i, res.StatusCode)
		}
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		mu.Lock()
		t.Fatalf("multi stream timed out; exits %v", codes)
	}
	mu.Lock()
	defer mu.Unlock()
	if codes["ffffffffffffffffffffffffffffffff"] != -1 {
		t.Fatalf("unknown session not reported: %v", codes)
	}
	for i, id := range ids {
		text := out[id].String()
		mine := "only-" + string(rune('a'+i))
		if !strings.Contains(text, mine) || codes[id] != 3+i {
			t.Fatalf("session %d: exit %d output %q", i, codes[id], text)
		}
		for j := range ids {
			if j != i && strings.Contains(text, "only-"+string(rune('a'+j))+"\r\n") {
				t.Fatalf("session %d received session %d's output: %q", i, j, text)
			}
		}
		if got := strings.Contains(text, "both-2"); got != (i < 2) {
			t.Fatalf("session %d broadcast=%v: %q", i, got, text)
		}
	}
}
