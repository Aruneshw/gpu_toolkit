package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fixture(t *testing.T, override map[string]string) (*Llama, *int) {
	t.Helper()
	calls := new(int)
	responses := map[string]string{"/health": `{"status":"ok"}`, "/v1/models": `{"data":[{"id":"tiny"}]}`, "/props": `{"default_generation_settings":{"n_ctx":2048}}`, "/tokenize": `{"tokens":[1,2,3]}`, "/completion": `{"content":"fixture only","timings":{"predicted_per_second":12.5}}`}
	for k, v := range override {
		responses[k] = v
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/completion" {
			*calls++
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body["n_predict"] != float64(8) || body["stream"] != false {
				t.Errorf("bad completion request %v", body)
			}
		}
		v, ok := responses[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(v))
	}))
	t.Cleanup(s.Close)
	l, err := New(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	return l, calls
}
func TestGenerateFixture(t *testing.T) {
	l, calls := fixture(t, nil)
	r, err := l.Generate(context.Background(), Request{Model: "tiny", Prompt: "hello", MaxTokens: 8, Context: 32})
	if err != nil || r.Content != "fixture only" || r.TokensPerSecond != 12.5 || *calls != 1 {
		t.Fatalf("%+v %v calls %d", r, err, *calls)
	}
}
func TestGenerateRejects(t *testing.T) {
	for _, tc := range []struct {
		name      string
		overrides map[string]string
		request   Request
	}{
		{"alias", nil, Request{"wrong", "hello", 8, 32}},
		{"context", nil, Request{"tiny", "hello", 8, 4096}},
		{"budget", nil, Request{"tiny", "hello", 8, 10}},
		{"input", nil, Request{"tiny", strings.Repeat("x", MaxPromptBytes+1), 8, 32}},
		{"models", map[string]string{"/v1/models": `{"data":[]}`}, Request{"tiny", "hello", 8, 32}},
		{"notready", map[string]string{"/health": `{"status":"loading"}`}, Request{"tiny", "hello", 8, 32}},
		{"malformed", map[string]string{"/props": "{"}, Request{"tiny", "hello", 8, 32}},
		{"unknowncontext", map[string]string{"/props": "{}"}, Request{"tiny", "hello", 8, 32}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, calls := fixture(t, tc.overrides)
			if _, err := l.Generate(context.Background(), tc.request); err == nil {
				t.Fatal("accepted invalid request")
			}
			if *calls != 0 {
				t.Fatal("inference called")
			}
		})
	}
}
func TestEndpoint(t *testing.T) {
	for _, s := range []string{"https://127.0.0.1:1", "http://example.com", "http://localhost:1", "http://192.168.0.1:1", "http://127.0.0.1/a", "http://user@127.0.0.1"} {
		if _, err := New(s); err == nil {
			t.Fatalf("accepted %s", s)
		}
	}
	if _, err := New("http://[::1]:8080"); err != nil {
		t.Fatal(err)
	}
}
func TestCanceled(t *testing.T) {
	l, _ := fixture(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.Generate(ctx, Request{"tiny", "hello", 8, 32}); err == nil {
		t.Fatal("ignored cancellation")
	}
}
func TestHTTPFailures(t *testing.T) {
	for _, mode := range []string{"redirect", "status", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "redirect":
					http.Redirect(w, r, "http://192.0.2.1", 302)
				case "status":
					w.WriteHeader(503)
				case "oversized":
					w.Write([]byte(strings.Repeat("x", MaxResponseBytes+1)))
				}
			}))
			defer s.Close()
			l, _ := New(s.URL)
			if _, err := l.Health(context.Background()); err == nil {
				t.Fatal("accepted failed response")
			}
		})
	}
}
