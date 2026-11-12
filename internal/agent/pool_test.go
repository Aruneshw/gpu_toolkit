package agent_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"meshcompute/internal/agent"
	"meshcompute/internal/backend"
	"meshcompute/internal/config"
	"meshcompute/internal/dashboard"
	"meshcompute/internal/model"
	"meshcompute/internal/security"
	"meshcompute/internal/terminal"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The test binary doubles as stand-ins for llama.cpp's two programs, so the
// whole pooling path runs with real processes, sockets and TLS tunnels. They
// are not language models: the fake rpc-server echoes lines and the fake
// llama-server proves it can reach every --rpc endpoint before serving HTTP.
func TestMain(m *testing.M) {
	if os.Getenv("MESHCOMPUTE_TEST_FAKE_NVIDIA") == "1" {
		// A slow nvidia-smi, as on a laptop whose GPU is busy training.
		time.Sleep(3 * time.Second)
		fmt.Println("0, Slow GPU, 1024, 512, 5, 1.0, 40, 10, 50")
		os.Exit(0)
	}
	if os.Getenv("MESHCOMPUTE_TEST_FAKE") == "1" {
		args := strings.Join(os.Args[1:], " ")
		if strings.Contains(args, "--list-devices") {
			// MESHCOMPUTE_TEST_FAKE_GPUS simulates the host's own GPUs.
			fmt.Println("Available devices:")
			if gpus := os.Getenv("MESHCOMPUTE_TEST_FAKE_GPUS"); gpus != "" {
				for _, g := range strings.Split(gpus, ",") {
					fmt.Printf("  %s: Fake GPU (6144 MiB, 6000 MiB free)\n", g)
				}
			} else {
				fmt.Println("  (none)")
			}
			os.Exit(0)
		}
		if strings.Contains(args, "--model") {
			fakeLlamaServer(os.Args[1:])
		} else {
			fakeRPCServer(os.Args[1:])
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func fakeRPCServer(args []string) {
	l, err := net.Listen("tcp", net.JoinHostPort(flagValue(args, "--host"), flagValue(args, "--port")))
	if err != nil {
		fmt.Println("fake rpc-server:", err)
		os.Exit(2)
	}
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			s := bufio.NewScanner(c)
			for s.Scan() {
				fmt.Fprintf(c, "gpu-echo:%s\n", s.Text())
			}
		}()
	}
}

func fakeLlamaServer(args []string) {
	var conns []net.Conn
	var readers []*bufio.Reader
	if list := flagValue(args, "--rpc"); list != "" {
		for _, addr := range strings.Split(list, ",") {
			c, err := net.DialTimeout("tcp", addr, 5*time.Second)
			if err != nil {
				fmt.Println("fake llama-server: rpc dial:", err)
				os.Exit(3)
			}
			conns, readers = append(conns, c), append(readers, bufio.NewReader(c))
		}
	}
	// A real llama-server keeps its RPC sockets for its whole life.
	roundTrip := func() error {
		for i, c := range conns {
			c.SetDeadline(time.Now().Add(10 * time.Second))
			if _, err := fmt.Fprintln(c, "layers"); err != nil {
				return err
			}
			line, err := readers[i].ReadString('\n')
			if err != nil || line != "gpu-echo:layers\n" {
				return fmt.Errorf("rpc endpoint %d answered %q: %v", i, line, err)
			}
		}
		return nil
	}
	if err := roundTrip(); err != nil {
		fmt.Println("fake llama-server:", err)
		os.Exit(3)
	}
	fmt.Printf("fake llama-server: %d rpc endpoint(s) verified\n", len(conns))
	alias := flagValue(args, "--alias")
	// Record the arguments, and report placement like the real program's
	// load log: 100 MiB per unit of split weight on each RPC device.
	if dir := os.Getenv("MESHCOMPUTE_TEST_ARGS_DIR"); dir != "" {
		_ = os.WriteFile(filepath.Join(dir, "llama-"+flagValue(args, "--port")+".args"), []byte(strings.Join(args, "\n")), 0600)
	}
	weights := strings.Split(flagValue(args, "--tensor-split"), ",")
	devices := strings.Split(flagValue(args, "--device"), ",")
	fmt.Println("0.00.1 I load_tensors: offloaded 31/31 layers to GPU")
	fmt.Println("0.00.1 I load_tensors:   CPU_Mapped model buffer size =    12.50 MiB")
	rpcIndex := 0
	for i, d := range devices {
		if !strings.HasPrefix(d, "RPC") {
			continue
		}
		mib := 100.0
		if i < len(weights) && weights[0] != "" {
			fmt.Sscanf(weights[i], "%g", &mib)
			mib *= 100
		}
		list := strings.Split(flagValue(args, "--rpc"), ",")
		if rpcIndex < len(list) {
			fmt.Printf("0.00.2 I load_tensors: RPC0[%s] model buffer size = %8.2f MiB\n", list[rpcIndex], mib)
			fmt.Printf("0.00.3 I llama_kv_cache: RPC0[%s] KV buffer size = %8.2f MiB\n", list[rpcIndex], mib/10)
		}
		rpcIndex++
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"status":"ok"}`) })
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, `{"data":[{"id":%q}]}`, alias) })
	mux.HandleFunc("/props", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"default_generation_settings":{"n_ctx":%s}}`, flagValue(args, "--ctx-size"))
	})
	mux.HandleFunc("/tokenize", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"tokens":[1,2,3]}`) })
	mux.HandleFunc("/completion", func(w http.ResponseWriter, r *http.Request) {
		if err := roundTrip(); err != nil {
			fmt.Println("fake llama-server:", err)
			os.Exit(4)
		}
		fmt.Fprintf(w, `{"content":"POOLED over %d rpc","timings":{"predicted_per_second":7}}`, len(conns))
	})
	go func() {
		// Like the real program, die when a worker's GPU goes away.
		buf := make([]byte, 1)
		for len(conns) > 0 {
			time.Sleep(200 * time.Millisecond)
			conns[0].SetReadDeadline(time.Now().Add(time.Millisecond))
			if _, err := readers[0].Peek(1); err != nil && !os.IsTimeout(err) {
				_ = buf
				fmt.Println("fake llama-server: rpc connection lost")
				os.Exit(5)
			}
		}
	}()
	err := http.ListenAndServe(net.JoinHostPort(flagValue(args, "--host"), flagValue(args, "--port")), mux)
	fmt.Println("fake llama-server:", err)
	os.Exit(6)
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func getJSON(t *testing.T, cl *http.Client, url string, body, out any) int {
	t.Helper()
	var r *http.Response
	var err error
	if body == nil {
		r, err = cl.Get(url)
	} else {
		b, _ := json.Marshal(body)
		r, err = cl.Post(url, "application/json", bytes.NewReader(b))
	}
	if err != nil {
		return 0
	}
	defer r.Body.Close()
	if r.StatusCode == http.StatusOK && out != nil {
		if err := json.NewDecoder(r.Body).Decode(out); err != nil {
			t.Fatalf("%s: %v", url, err)
		}
	}
	return r.StatusCode
}

// A worker lends its "GPU" (rpc-server on loopback), the host starts one
// llama-server whose --rpc endpoints are loopback forwarders into the worker's
// authenticated tunnel, inference runs through the pool, the host can open a
// terminal on the worker that allowed it, and the pool recovers on its own
// when the worker leaves.
func TestPoolAndRemoteTerminalEndToEnd(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MESHCOMPUTE_TEST_FAKE", "1")
	t.Setenv("SHELL", "/bin/sh")
	ctx, cancel := context.WithCancel(context.Background())
	hostDir := filepath.Join(t.TempDir(), "host")
	workerDir := filepath.Join(t.TempDir(), "worker")
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
	t.Cleanup(cancel)
	code, err := security.Pair(hostDir, hc.Advertise, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := security.JoinHost(ctx, workerDir, code, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	wl := listen(t)
	wc := config.Default()
	wc.Listen = wl.Addr().String()
	wc.Advertise = "https://" + wc.Listen
	wc.BackendURL = "http://" + freeAddr(t) // nothing listens: the worker has no model of its own
	wc.RPCServer = exe
	wc.AllowTerminal = true
	workerCtx, stopWorker := context.WithCancel(ctx)
	startAgent(t, workerCtx, workerDir, wc, wl)
	t.Cleanup(cancel)
	id, err := security.NodeID(workerDir)
	if err != nil {
		t.Fatal(err)
	}
	client, err := security.Client(hostDir)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	client.Timeout = 20 * time.Second
	local := hc.LocalEndpoint()

	wait := func(what string, limit time.Duration, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(limit)
		for !ok() {
			if time.Now().After(deadline) {
				var st agent.PoolStatus
				getJSON(t, client, local+"/v1/pool", nil, &st)
				t.Fatalf("timed out waiting for %s; pool: %+v", what, st)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	wait("the worker to lend its GPU", 30*time.Second, func() bool {
		var st agent.Status
		return getJSON(t, client, local+"/v1/workers/"+id+"/v1/status", nil, &st) == 200 && st.RPCReady && st.Terminal
	})

	// Only the host's own certificate may control the pool.
	workerClient, err := security.Client(workerDir)
	if err != nil {
		t.Fatal(err)
	}
	defer workerClient.CloseIdleConnections()
	if code := getJSON(t, workerClient, hc.Advertise+"/v1/pool", map[string]any{"enabled": true, "model": "tiny"}, nil); code != http.StatusForbidden {
		t.Fatalf("worker reached pool control: HTTP %d", code)
	}
	if code := getJSON(t, client, local+"/v1/pool", map[string]any{"enabled": true, "model": "missing"}, nil); code != http.StatusBadRequest {
		t.Fatalf("unregistered model accepted: HTTP %d", code)
	}

	var pool agent.PoolStatus
	if code := getJSON(t, client, local+"/v1/pool", map[string]any{"enabled": true, "model": "tiny", "context": 512}, &pool); code != 200 || !pool.Enabled {
		t.Fatalf("pool start: HTTP %d %+v", code, pool)
	}
	wait("the pooled model across the worker", 40*time.Second, func() bool {
		return getJSON(t, client, local+"/v1/pool", nil, &pool) == 200 && pool.State == "ready" && len(pool.Workers) == 1 && pool.Workers[0] == id
	})
	if saved, err := config.Load(hostDir); err != nil || len(saved.Pools) != 1 || saved.Pools[0].Model != "tiny" || saved.Pools[0].Context != 512 || !saved.Pools[0].Enabled || saved.PoolModel != "" {
		t.Fatalf("pool choice not remembered: %+v %v", saved, err)
	}

	// Inference on the host now goes through the worker's rpc-server.
	var result agent.Result
	req := backend.Request{Model: "tiny", Prompt: "hello", MaxTokens: 8, Context: 64}
	if code := getJSON(t, client, local+"/v1/run", req, &result); code != 200 || result.Content != "POOLED over 1 rpc" || result.Mode != "pooled" {
		t.Fatalf("pooled inference: HTTP %d %+v", code, result)
	}
	// Status and the dashboard keep working while the RPC stream stays open.
	remote := dashboard.NewRemote(hostDir, hc)
	nodes, err := remote.Statuses(ctx)
	if err != nil || len(nodes) != 2 || !nodes[0].Healthy || !nodes[1].Healthy || nodes[0].Status.Pool == nil || nodes[0].Status.Pool.State != "ready" {
		t.Fatalf("statuses during pooling: %+v %v", nodes, err)
	}

	if runtime.GOOS == "linux" {
		// The host opens a shell on the worker through the same tunnel.
		stream, err := remote.DialTerminal(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		shells := terminal.NewManager(nil, "")
		defer shells.CloseAll()
		sess, err := shells.Attach(stream, 80, 24)
		if err != nil {
			t.Fatal(err)
		}
		if err := sess.Write([]byte("echo remote-$((40+2)); exit 3\n")); err != nil {
			t.Fatal(err)
		}
		select {
		case <-sess.Done():
		case <-time.After(15 * time.Second):
			t.Fatal("remote shell did not finish")
		}
		out, _, _, _ := sess.Read(0)
		if !strings.Contains(string(out), "remote-42") || sess.ExitCode() != 3 {
			t.Fatalf("remote shell: exit %d output %q", sess.ExitCode(), out)
		}
	}

	// The worker leaves: the pool reloads on this computer alone, unasked.
	stopWorker()
	wait("the pool to recover without the worker", 60*time.Second, func() bool {
		return getJSON(t, client, local+"/v1/pool", nil, &pool) == 200 && pool.State == "ready" && len(pool.Workers) == 0
	})
	if code := getJSON(t, client, local+"/v1/run", req, &result); code != 200 || result.Content != "POOLED over 0 rpc" || result.Mode != "single_node" {
		t.Fatalf("inference after the worker left: HTTP %d %+v", code, result)
	}
	if code := getJSON(t, client, local+"/v1/pool", map[string]any{"enabled": false}, &pool); code != 200 || pool.Enabled {
		t.Fatalf("pool stop: HTTP %d %+v", code, pool)
	}
	wait("the pooled server to stop", 15*time.Second, func() bool {
		c, err := net.DialTimeout("tcp", strings.TrimPrefix(hc.BackendURL, "http://"), 200*time.Millisecond)
		if err == nil {
			c.Close()
		}
		return err != nil
	})
}

// A worker that did not opt in refuses the host's terminal.
func TestRemoteTerminalNeedsWorkerConsent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	hostDir := filepath.Join(t.TempDir(), "host")
	workerDir := filepath.Join(t.TempDir(), "worker")
	hl := listen(t)
	hc := config.Default()
	hc.Listen = hl.Addr().String()
	hc.Advertise = "https://" + hc.Listen
	hc.GroupHost = true
	if err := security.InitGroup(hostDir, hc.Listen, hc.Advertise, false); err != nil {
		t.Fatal(err)
	}
	startAgent(t, ctx, hostDir, hc, hl)
	t.Cleanup(cancel)
	code, err := security.Pair(hostDir, hc.Advertise, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := security.JoinHost(ctx, workerDir, code, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	wl := listen(t)
	wc := config.Default()
	wc.Listen = wl.Addr().String()
	wc.Advertise = "https://" + wc.Listen
	wc.ShareGPU = "off"
	startAgent(t, ctx, workerDir, wc, wl)
	t.Cleanup(cancel)
	id, _ := security.NodeID(workerDir)
	remote := dashboard.NewRemote(hostDir, hc)
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err = remote.DialTerminal(ctx, id)
		if err == dashboard.ErrTerminalDenied {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected the worker to refuse the terminal, got: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	nodes, err := remote.Statuses(ctx)
	if err != nil || len(nodes) != 2 || nodes[1].Status == nil || nodes[1].Status.Terminal || nodes[1].Status.RPCReady {
		t.Fatalf("worker advertises a terminal or GPU sharing it did not allow: %+v %v", nodes, err)
	}
}
