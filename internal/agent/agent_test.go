package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"meshcompute/internal/backend"
	"meshcompute/internal/config"
	"meshcompute/internal/model"
	"meshcompute/internal/network"
	"meshcompute/internal/security"
	"meshcompute/internal/sse"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func serveTest(t *testing.T, a *Agent) *httptest.Server {
	t.Helper()
	cfg, e := security.ServerTLS(a.dir)
	if e != nil {
		t.Fatal(e)
	}
	s := httptest.NewUnstartedServer(a.Handler())
	s.TLS = cfg
	s.StartTLS()
	t.Cleanup(s.Close)
	return s
}

// This test executes real HTTPS enrollment/dispatch but uses a fixture backend,
// not a language model, and must never be presented as real AI inference.
func TestTwoAgentTLSDispatchFixture(t *testing.T) {
	ctx := context.Background()
	coordinator := filepath.Join(t.TempDir(), "coordinator")
	worker := filepath.Join(t.TempDir(), "worker")
	c := config.Default()
	if e := security.Init(coordinator, "127.0.0.1"); e != nil {
		t.Fatal(e)
	}
	control := serveTest(t, New(coordinator, c, io.Discard))
	code, e := security.Pair(coordinator, control.URL, time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	if e = security.JoinHost(ctx, worker, code, "127.0.0.1"); e != nil {
		t.Fatal(e)
	}
	var calls atomic.Int32
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		responses := map[string]string{"/health": `{"status":"ok"}`, "/props": `{"default_generation_settings":{"n_ctx":2048}}`, "/v1/models": `{"data":[{"id":"tiny"}]}`, "/tokenize": `{"tokens":[1,2,3]}`, "/completion": `{"content":"FIXTURE ONLY","timings":{"predicted_per_second":12}}`}
		if r.URL.Path == "/completion" {
			calls.Add(1)
		}
		if body, ok := responses[r.URL.Path]; ok {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, body)
		} else {
			http.NotFound(w, r)
		}
	}))
	defer fixture.Close()
	c.BackendURL = fixture.URL
	path := filepath.Join(t.TempDir(), "fixture.gguf")
	if e = os.WriteFile(path, []byte("GGUFfixture-not-a-model"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = model.Add(filepath.Join(worker, "models"), "tiny", path); e != nil {
		t.Fatal(e)
	}
	var audit bytes.Buffer
	a := New(worker, c, &audit)
	remote := serveTest(t, a)
	client, e := security.Client(coordinator)
	if e != nil {
		t.Fatal(e)
	}
	defer client.CloseIdleConnections()
	q, e := network.Measure(ctx, client, remote.URL)
	if e != nil || !q.Available {
		t.Fatalf("probe %+v %v", q, e)
	}
	body, _ := json.Marshal(backend.Request{Model: "tiny", Prompt: "SECRET_PROMPT", MaxTokens: 8, Context: 32})
	res, e := client.Post(remote.URL+"/v1/run", "application/json", bytes.NewReader(body))
	if e != nil {
		t.Fatal(e)
	}
	var result Result
	e = json.NewDecoder(res.Body).Decode(&result)
	res.Body.Close()
	if e != nil || res.StatusCode != 200 || result.Content != "FIXTURE ONLY" || calls.Load() != 1 {
		t.Fatalf("%+v status=%d error=%v", result, res.StatusCode, e)
	}
	id, _ := security.NodeID(worker)
	if result.NodeID != id || result.Mode != "single_node" {
		t.Fatal("incorrect participant attribution")
	}
	if strings.Contains(audit.String(), "SECRET_PROMPT") {
		t.Fatal("prompt leaked into audit log")
	}
	for _, bad := range []string{`{"unexpected":true}`, string(body) + ` {}`, strings.Repeat("x", backend.MaxPromptBytes+4097)} {
		r, e := client.Post(remote.URL+"/v1/run", "application/json", strings.NewReader(bad))
		if e != nil {
			t.Fatal(e)
		}
		r.Body.Close()
		if r.StatusCode != 400 {
			t.Fatal(r.StatusCode)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("invalid request reached backend")
	}
	if e = security.Revoke(coordinator, id); e != nil {
		t.Fatal(e)
	}
	if r, e := client.Get(remote.URL + "/v1/ping"); e == nil {
		r.Body.Close()
		t.Fatal("revoked worker still reachable")
	}
}
func TestTwoAgentTLSStreamFixture(t *testing.T) {
	ctx := context.Background()
	coordinator := filepath.Join(t.TempDir(), "coordinator")
	worker := filepath.Join(t.TempDir(), "worker")
	c := config.Default()
	if e := security.Init(coordinator, "127.0.0.1"); e != nil {
		t.Fatal(e)
	}
	control := serveTest(t, New(coordinator, c, io.Discard))
	code, e := security.Pair(coordinator, control.URL, time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	if e = security.JoinHost(ctx, worker, code, "127.0.0.1"); e != nil {
		t.Fatal(e)
	}
	var streamCalls atomic.Int32
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
			var body map[string]any
			if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
				t.Error(e)
			}
			if body["stream"] != true {
				t.Error("agent did not request streaming")
			}
			streamCalls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"content\":\"FIXTURE\",\"stop\":false}\n\n")
			fmt.Fprint(w, "data: {\"content\":\" STREAM\",\"stop\":false}\n\n")
			fmt.Fprint(w, "data: {\"content\":\"\",\"stop\":true,\"timings\":{\"predicted_per_second\":9}}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer fixture.Close()
	c.BackendURL = fixture.URL
	path := filepath.Join(t.TempDir(), "fixture.gguf")
	if e = os.WriteFile(path, []byte("GGUFfixture-not-a-model"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = model.Add(filepath.Join(worker, "models"), "tiny", path); e != nil {
		t.Fatal(e)
	}
	var audit bytes.Buffer
	a := New(worker, c, &audit)
	remote := serveTest(t, a)
	client, e := security.Client(coordinator)
	if e != nil {
		t.Fatal(e)
	}
	defer client.CloseIdleConnections()

	body, _ := json.Marshal(backend.Request{Model: "tiny", Prompt: "SECRET_PROMPT", MaxTokens: 8, Context: 32})
	post := func() *http.Response {
		t.Helper()
		res, e := client.Post(remote.URL+"/v1/run/stream", "application/json", bytes.NewReader(body))
		if e != nil {
			t.Fatal(e)
		}
		return res
	}
	res := post()
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status=%d type=%s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	var content strings.Builder
	rate := 0.0
	e = sse.Read(res.Body, func(event string, data []byte) error {
		switch event {
		case "token":
			var v struct {
				Content string `json:"content"`
			}
			if json.Unmarshal(data, &v) != nil {
				t.Fatal("invalid token event")
			}
			content.WriteString(v.Content)
			return nil
		case "done":
			var v struct {
				Rate float64 `json:"tokens_per_second"`
			}
			if json.Unmarshal(data, &v) != nil {
				t.Fatal("invalid done event")
			}
			rate = v.Rate
			return sse.Done
		case "error":
			t.Fatalf("stream error: %s", data)
		}
		return nil
	})
	// Drain to EOF so the handler, including its audit write, has returned
	// before the test inspects shared state.
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if e != nil || content.String() != "FIXTURE STREAM" || rate != 9 || streamCalls.Load() != 1 {
		t.Fatalf("content=%q rate=%v err=%v calls=%d", content.String(), rate, e, streamCalls.Load())
	}
	a.mu.Lock()
	busy, last := a.busy, a.last
	a.mu.Unlock()
	if busy || last != "completed" {
		t.Fatalf("busy=%v last=%s", busy, last)
	}
	if strings.Contains(audit.String(), "SECRET_PROMPT") {
		t.Fatal("prompt leaked into audit log")
	}

	// A busy worker reports an SSE error event and does not call the backend.
	a.mu.Lock()
	a.busy = true
	a.mu.Unlock()
	res = post()
	hasError := false
	e = sse.Read(res.Body, func(event string, data []byte) error {
		if event == "error" {
			hasError = true
			return sse.Done
		}
		return nil
	})
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if e != nil || !hasError || streamCalls.Load() != 1 {
		t.Fatalf("busy stream err=%v hasError=%v calls=%d", e, hasError, streamCalls.Load())
	}
	a.mu.Lock()
	a.busy = false
	a.mu.Unlock()

	for _, bad := range []string{`{"unexpected":true}`, string(body) + ` {}`} {
		r, e := client.Post(remote.URL+"/v1/run/stream", "application/json", strings.NewReader(bad))
		if e != nil {
			t.Fatal(e)
		}
		r.Body.Close()
		if r.StatusCode != 400 {
			t.Fatalf("status %d", r.StatusCode)
		}
	}
	if streamCalls.Load() != 1 {
		t.Fatal("invalid request reached backend")
	}
}

func TestLocalAgentIdentity(t *testing.T) {
	d := filepath.Join(t.TempDir(), "identity")
	if e := security.Init(d, "127.0.0.1"); e != nil {
		t.Fatal(e)
	}
	s := serveTest(t, New(d, config.Default(), io.Discard))
	cl, e := security.Client(d)
	if e != nil {
		t.Fatal(e)
	}
	defer cl.CloseIdleConnections()
	res, e := cl.Get(s.URL + "/v1/ping")
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
}
func TestBusyAndFailure(t *testing.T) {
	a := New(t.TempDir(), config.Default(), io.Discard)
	a.busy = true
	if _, e := a.Generate(context.Background(), backend.Request{}); e == nil {
		t.Fatal("busy accepted")
	}
	a.busy = false
	if _, e := a.Generate(context.Background(), backend.Request{Model: "absent"}); e == nil {
		t.Fatal("missing model accepted")
	}
	if a.busy || a.last != "failed" {
		t.Fatal("failure state not cleared")
	}
}
