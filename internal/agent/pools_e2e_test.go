package agent_test

import (
	"context"
	"meshcompute/internal/agent"
	"meshcompute/internal/backend"
	"meshcompute/internal/config"
	"meshcompute/internal/model"
	"meshcompute/internal/security"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestSeveralPoolsEndToEnd: a host with its own (simulated) GPU and three
// real workers runs two pools of the same model. Pool "big" names two
// workers with a 1:3 split, two request slots, a q8_0 KV cache kept in host
// RAM and flash attention; pool "spare" takes the remaining worker
// automatically. Requests alternate between them. Moving a worker out of
// "big" hands it to "spare"; a worker cannot be in two pools; removing a
// pool frees its workers.
func TestSeveralPoolsEndToEnd(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	argsDir := t.TempDir()
	t.Setenv("MESHCOMPUTE_TEST_FAKE", "1")
	t.Setenv("MESHCOMPUTE_TEST_FAKE_GPUS", "Vulkan0")
	t.Setenv("MESHCOMPUTE_TEST_ARGS_DIR", argsDir)
	backend.ForgetDevices()
	t.Cleanup(backend.ForgetDevices)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hostDir := filepath.Join(t.TempDir(), "host")
	hl := listen(t)
	hc := config.Default()
	hc.Listen = hl.Addr().String()
	hc.Advertise = "https://" + hc.Listen
	hc.GroupHost = true
	hc.BackendURL = "http://" + freeAddr(t)
	hc.LlamaServer = exe
	if err := security.InitGroup(hostDir, hc.Listen, hc.Advertise, false); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(hostDir, hc); err != nil {
		t.Fatal(err)
	}
	gguf := filepath.Join(t.TempDir(), "tiny.gguf")
	if err := os.WriteFile(gguf, []byte("GGUFfixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := model.Add(filepath.Join(hostDir, "models"), "tiny", gguf); err != nil {
		t.Fatal(err)
	}
	startAgent(t, ctx, hostDir, hc, hl)
	var ws []testWorker
	for _, name := range []string{"w1", "w2", "w3"} {
		ws = append(ws, joinWorker(t, ctx, hostDir, hc, name, func(c *config.Config) { c.ShareGPU, c.RPCServer = "", exe }))
	}
	ids := []string{ws[0].id, ws[1].id, ws[2].id}
	sort.Strings(ids)
	client, err := security.Client(hostDir)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	client.Timeout = 20 * time.Second
	local := hc.LocalEndpoint()
	pools := func() agent.PoolsStatus {
		var st agent.PoolsStatus
		getJSON(t, client, local+"/v1/pools", nil, &st)
		return st
	}
	byName := func(st agent.PoolsStatus, name string) agent.PoolStatus {
		for _, p := range st.Pools {
			if p.Name == name {
				return p
			}
		}
		return agent.PoolStatus{}
	}
	wait := func(what string, ok func(agent.PoolsStatus) bool) agent.PoolsStatus {
		t.Helper()
		deadline := time.Now().Add(60 * time.Second)
		for {
			st := pools()
			if ok(st) {
				return st
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s: %+v", what, st)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	set := func(p config.PoolConfig) int {
		t.Helper()
		var out map[string]any
		return getJSON(t, client, local+"/v1/pools", map[string]any{"action": "set", "pool": p}, &out)
	}
	wait("three workers lending", func(st agent.PoolsStatus) bool { return len(st.Sharing) == 3 })

	big := config.PoolConfig{Name: "big", Model: "tiny", Enabled: true, Context: 1024, Parallel: 2, Workers: []string{ids[0], ids[1]},
		Split: map[string]float64{"local": 2, ids[0]: 1, ids[1]: 3}, CacheTypeK: "q8_0", CacheTypeV: "q8_0", KVOnHost: true, FlashAttn: "on"}
	if code := set(big); code != 200 {
		t.Fatalf("set big: HTTP %d", code)
	}
	if code := set(config.PoolConfig{Name: "spare", Model: "tiny", Enabled: true, Context: 512}); code != 200 {
		t.Fatalf("set spare: HTTP %d", code)
	}
	st := wait("both pools ready", func(st agent.PoolsStatus) bool {
		b, s := byName(st, "big"), byName(st, "spare")
		return b.State == "ready" && s.State == "ready" && len(b.Workers) == 2 && len(s.Workers) == 1 && len(b.Devices) == 3
	})
	b, s := byName(st, "big"), byName(st, "spare")
	if s.Workers[0] != ids[2] || b.Endpoint == s.Endpoint || b.Endpoint == "" {
		t.Fatalf("assignment or endpoints: big %+v spare %+v", b, s)
	}
	// The arguments llama-server got.
	port := b.Endpoint[strings.LastIndex(b.Endpoint, ":")+1:]
	raw, err := os.ReadFile(filepath.Join(argsDir, "llama-"+port+".args"))
	if err != nil {
		t.Fatal(err)
	}
	args := strings.ReplaceAll(string(raw), "\n", " ")
	for _, want := range []string{"--device Vulkan0,RPC0,RPC1", "--tensor-split 2,1,3", "--parallel 2", "--cache-type-k q8_0", "--cache-type-v q8_0", "--no-kv-offload", "--flash-attn on", "--ctx-size 1024", "--log-verbosity 4"} {
		if !strings.Contains(args, want) {
			t.Fatalf("big's llama-server lacks %q: %s", want, args)
		}
	}
	// Placement, from the load log, attributed to the right computers.
	mem := map[string]float64{}
	for _, d := range b.Devices {
		mem[d.Node] += d.WeightsMiB
	}
	if mem["local"] != 12.5 || mem[ids[0]] != 100 || mem[ids[1]] != 300 || b.Offloaded != "31/31" {
		t.Fatalf("placement %v offloaded %q devices %+v", mem, b.Offloaded, b.Devices)
	}

	// Requests for the model alternate between the two pools.
	req := backend.Request{Model: "tiny", Prompt: "hello", MaxTokens: 8, Context: 64}
	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		var result agent.Result
		if code := getJSON(t, client, local+"/v1/run", req, &result); code != 200 || result.Mode != "pooled" {
			t.Fatalf("request %d: HTTP %d %+v", i, code, result)
		}
		seen[result.Content]++
	}
	if seen["POOLED over 2 rpc"] != 2 || seen["POOLED over 1 rpc"] != 2 {
		t.Fatalf("requests did not alternate: %v", seen)
	}
	// The host's status lists both pools and the model for prompting.
	var hostStatus agent.Status
	getJSON(t, client, local+"/v1/status", nil, &hostStatus)
	if len(hostStatus.Pools) != 2 || hostStatus.Backend == nil || len(hostStatus.Backend.Models) != 1 || hostStatus.Backend.Models[0] != "tiny" {
		t.Fatalf("host status: %+v", hostStatus)
	}

	// A worker cannot be in two pools.
	if code := set(config.PoolConfig{Name: "spare", Model: "tiny", Enabled: true, Workers: []string{ids[0]}}); code != http.StatusBadRequest {
		t.Fatalf("worker in two pools: HTTP %d", code)
	}
	// Taking a worker out of "big" gives it to the automatic pool.
	big.Workers = []string{ids[0]}
	big.Split = nil
	if code := set(big); code != 200 {
		t.Fatalf("shrink big: HTTP %d", code)
	}
	wait("spare to take the freed worker", func(st agent.PoolsStatus) bool {
		b, s := byName(st, "big"), byName(st, "spare")
		return b.State == "ready" && s.State == "ready" && len(b.Workers) == 1 && b.Workers[0] == ids[0] && len(s.Workers) == 2
	})
	// Removing "big" frees its worker too, and survives a restart of the
	// saved configuration.
	var out map[string]any
	if code := getJSON(t, client, local+"/v1/pools", map[string]any{"action": "remove", "name": "big"}, &out); code != 200 {
		t.Fatalf("remove: HTTP %d", code)
	}
	wait("spare to use all three workers", func(st agent.PoolsStatus) bool {
		s := byName(st, "spare")
		return len(st.Pools) == 1 && s.State == "ready" && len(s.Workers) == 3
	})
	saved, err := config.Load(hostDir)
	if err != nil || len(saved.Pools) != 1 || saved.Pools[0].Name != "spare" {
		t.Fatalf("saved pools: %+v %v", saved.Pools, err)
	}
	// Only the host's own certificate may change pools.
	workerClient, err := security.Client(ws[0].dir)
	if err != nil {
		t.Fatal(err)
	}
	defer workerClient.CloseIdleConnections()
	if code := getJSON(t, workerClient, hc.Advertise+"/v1/pools", map[string]any{"action": "remove", "name": "spare"}, nil); code != http.StatusForbidden {
		t.Fatalf("a worker changed the pools: HTTP %d", code)
	}
}

// TestRemovedPoolLeavesNoServer creates and removes pools in quick
// succession, while the background loop may be starting their servers. No
// llama-server may survive its pool: one did, holding the backend port.
func TestRemovedPoolLeavesNoServer(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MESHCOMPUTE_TEST_FAKE", "1")
	backend.ForgetDevices()
	t.Cleanup(backend.ForgetDevices)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hostDir := filepath.Join(t.TempDir(), "host")
	hl := listen(t)
	hc := config.Default()
	hc.Listen = hl.Addr().String()
	hc.Advertise = "https://" + hc.Listen
	hc.GroupHost = true
	backendAddr := freeAddr(t)
	hc.BackendURL = "http://" + backendAddr
	hc.LlamaServer = exe
	if err := security.InitGroup(hostDir, hc.Listen, hc.Advertise, false); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(hostDir, hc); err != nil {
		t.Fatal(err)
	}
	gguf := filepath.Join(t.TempDir(), "tiny.gguf")
	os.WriteFile(gguf, []byte("GGUFfixture"), 0600)
	if _, err := model.Add(filepath.Join(hostDir, "models"), "tiny", gguf); err != nil {
		t.Fatal(err)
	}
	startAgent(t, ctx, hostDir, hc, hl)
	joinWorker(t, ctx, hostDir, hc, "w", func(c *config.Config) { c.ShareGPU, c.RPCServer = "", exe })
	client, err := security.Client(hostDir)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	client.Timeout = 20 * time.Second
	local := hc.LocalEndpoint()
	for round := 0; round < 15; round++ {
		name := "p" + string(rune('a'+round%3))
		if code := getJSON(t, client, local+"/v1/pools", map[string]any{"action": "set", "pool": config.PoolConfig{Name: name, Model: "tiny", Enabled: true, Context: 512}}, nil); code != 200 {
			t.Fatalf("round %d set: HTTP %d", round, code)
		}
		time.Sleep(time.Duration(round*40) * time.Millisecond) // catch the loop at different moments
		if code := getJSON(t, client, local+"/v1/pools", map[string]any{"action": "remove", "name": name}, nil); code != 200 {
			t.Fatalf("round %d remove: HTTP %d", round, code)
		}
	}
	// Give any stray start time to happen, then nothing may listen.
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", backendAddr, 200*time.Millisecond)
		if err != nil {
			break
		}
		c.Close()
		if time.Now().After(deadline) {
			t.Fatal("a removed pool's llama-server still listens on the backend port")
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(4 * time.Second) // two more reconcile rounds
	if c, err := net.DialTimeout("tcp", backendAddr, 200*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("a llama-server came back after its pool was removed")
	}
	var st agent.PoolsStatus
	getJSON(t, client, local+"/v1/pools", nil, &st)
	if len(st.Pools) != 0 {
		t.Fatalf("pools left: %+v", st.Pools)
	}
}
