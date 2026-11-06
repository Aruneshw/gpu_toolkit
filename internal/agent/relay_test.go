package agent_test

import (
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
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func startAgent(t *testing.T, ctx context.Context, dir string, c config.Config, l net.Listener) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- agent.New(dir, c, io.Discard).ServeListener(ctx, l) }()
	t.Cleanup(func() {
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("agent shutdown: %v", err)
			}
		case <-time.After(8 * time.Second):
			t.Error("agent failed to shut down")
		}
	})
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

// Real enrollment, outbound TLS, proxy routing, dashboard reads, SSE, reconnect
// and revocation. The backend is a fixture, not a real language model.
func TestGroupReverseEndToEnd(t *testing.T) {
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
	if err := security.JoinHost(ctx, filepath.Join(t.TempDir(), "replay"), code, "127.0.0.1"); err == nil {
		t.Fatal("invitation replay accepted")
	}
	var calls atomic.Int32
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			io.WriteString(w, `{"status":"ok"}`)
		case "/v1/models":
			io.WriteString(w, `{"data":[{"id":"tiny"}]}`)
		case "/props":
			io.WriteString(w, `{"default_generation_settings":{"n_ctx":2048}}`)
		case "/tokenize":
			io.WriteString(w, `{"tokens":[1,2,3]}`)
		case "/completion":
			calls.Add(1)
			var req struct {
				Stream bool
				Prompt string
			}
			json.NewDecoder(r.Body).Decode(&req)
			if req.Prompt == "BLOCK" {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"content\":\"started\",\"stop\":false}\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				return
			}
			if req.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"content\":\"FIXTURE\",\"stop\":false}\n\ndata: {\"content\":\"\",\"stop\":true,\"timings\":{\"predicted_per_second\":9}}\n\n")
			} else {
				io.WriteString(w, `{"content":"FIXTURE","timings":{"predicted_per_second":9}}`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer fixture.Close()
	defer cancel()
	wl := listen(t)
	wc := config.Default()
	wc.Listen = wl.Addr().String()
	wc.Advertise = "https://" + wc.Listen
	wc.BackendURL = fixture.URL
	path := filepath.Join(t.TempDir(), "test.gguf")
	if err := os.WriteFile(path, []byte("GGUFfixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := model.Add(filepath.Join(workerDir, "models"), "tiny", path); err != nil {
		t.Fatal(err)
	}
	startAgent(t, ctx, workerDir, wc, wl)
	// Cancel before waiting for either server in cleanup (LIFO).
	t.Cleanup(cancel)
	client, err := security.Client(hostDir)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	id, err := security.NodeID(workerDir)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := hc.Advertise + "/v1/workers/" + id
	deadline := time.Now().Add(45 * time.Second)
	for {
		r, e := client.Get(hc.Advertise + "/v1/workers")
		found := false
		if e == nil {
			var peers []config.Peer
			json.NewDecoder(r.Body).Decode(&peers)
			r.Body.Close()
			for _, p := range peers {
				if p.Name == id {
					found = true
				}
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker not discovered")
		}
		time.Sleep(20 * time.Millisecond)
	}
	remote := dashboard.NewRemote(hostDir, hc)
	nodes, err := remote.Statuses(ctx)
	if err != nil || len(nodes) != 2 {
		t.Fatalf("discovery: %+v %v", nodes, err)
	}
	for _, n := range nodes {
		if !n.Healthy {
			t.Fatalf("unhealthy node: %+v", n)
		}
	}
	req := backend.Request{Model: "tiny", Prompt: "test", MaxTokens: 8, Context: 32}
	var text strings.Builder
	done := false
	if err := remote.Stream(ctx, id, req, func(c backend.Chunk) error { text.WriteString(c.Content); done = done || c.Done; return nil }); err != nil {
		t.Fatal(err)
	}
	if text.String() != "FIXTURE" || !done || calls.Load() != 1 {
		t.Fatalf("bad stream: %s done=%v calls=%d", text.String(), done, calls.Load())
	}
	// Separate request over a replenished connection: no peer address registered.
	b, _ := json.Marshal(req)
	r, err := client.Post(endpoint+"/v1/run", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	var result agent.Result
	err = json.NewDecoder(r.Body).Decode(&result)
	r.Body.Close()
	if err != nil || result.NodeID != id || result.Content != "FIXTURE" || calls.Load() != 2 {
		t.Fatalf("JSON dispatch: %+v %v", result, err)
	}
	workerClient, err := security.Client(workerDir)
	if err != nil {
		t.Fatal(err)
	}
	defer workerClient.CloseIdleConnections()
	for _, p := range []string{"/v1/workers", "/v1/workers/" + id + "/v1/status"} {
		r, e := workerClient.Get(hc.Advertise + p)
		if e != nil {
			t.Fatal(e)
		}
		r.Body.Close()
		if r.StatusCode != 403 {
			t.Fatalf("worker accessed host routing: %d", r.StatusCode)
		}
	}
	// Unknown/administrative paths may not be tunneled.
	r, err = client.Post(endpoint+"/enroll", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 404 {
		t.Fatal("enrollment was forwarded")
	}
	// Revocation must cancel an in-flight stream and stop reconnecting workers.
	// First cancel from the caller, then confirm the inference slot is reusable.
	req.Prompt = "BLOCK"
	b, _ = json.Marshal(req)
	requestCtx, stopRequest := context.WithCancel(ctx)
	blocking, _ := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint+"/v1/run/stream", bytes.NewReader(b))
	r, err = client.Do(blocking)
	if err != nil {
		stopRequest()
		t.Fatal(err)
	}
	firstByte := make([]byte, 1)
	if _, err := r.Body.Read(firstByte); err != nil {
		stopRequest()
		t.Fatal(err)
	}
	stopRequest()
	r.Body.Close()
	until := time.Now().Add(10 * time.Second)
	for {
		r, e := client.Get(endpoint + "/v1/status")
		var st agent.Status
		if e == nil {
			json.NewDecoder(r.Body).Decode(&st)
			r.Body.Close()
		}
		if e == nil && st.NodeID == id && !st.Busy {
			break
		}
		if time.Now().After(until) {
			t.Fatal("cancellation did not release inference slot")
		}
		time.Sleep(25 * time.Millisecond)
	}
	req.Prompt = "BLOCK"
	b, _ = json.Marshal(req)
	r, err = client.Post(endpoint+"/v1/run/stream", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	first := make([]byte, 1)
	if _, err := r.Body.Read(first); err != nil {
		t.Fatal(err)
	}
	if err := security.Revoke(hostDir, id); err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	go func() { _, e := io.Copy(io.Discard, r.Body); r.Body.Close(); ended <- e }()
	select {
	case <-ended:
	case <-time.After(6 * time.Second):
		t.Fatal("revocation did not stop active stream")
	}
	r, err = client.Get(endpoint + "/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode == 200 {
		t.Fatal("revoked worker remained usable")
	}
	cancel()
}
