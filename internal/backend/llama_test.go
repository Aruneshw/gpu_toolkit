package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
func streamFixture(t *testing.T, events ...string) (*Llama, *int, *bool) {
	t.Helper()
	calls := new(int)
	streamed := new(bool)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			*calls++
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body["stream"] != true || body["n_predict"] != float64(8) {
				t.Errorf("bad stream request %v", body)
			}
			*streamed = true
			w.Header().Set("Content-Type", "text/event-stream")
			f, _ := w.(http.Flusher)
			for _, event := range events {
				fmt.Fprintf(w, "data: %s\n\n", event)
				if f != nil {
					f.Flush()
				}
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	l, err := New(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	return l, calls, streamed
}

func TestGenerateStreamFixture(t *testing.T) {
	l, calls, streamed := streamFixture(t,
		`{"content":"Hello","stop":false}`,
		`{"content":" world","stop":false}`,
		`{"content":"","stop":true,"timings":{"predicted_per_second":7.5}}`,
	)
	var chunks []Chunk
	err := l.GenerateStream(context.Background(), Request{"tiny", "hello", 8, 32}, func(c Chunk) error {
		chunks = append(chunks, c)
		return nil
	})
	if err != nil || !*streamed || *calls != 1 {
		t.Fatalf("%v streamed=%v calls=%d", err, *streamed, *calls)
	}
	if len(chunks) != 3 || chunks[0].Content != "Hello" || chunks[1].Content != " world" {
		t.Fatalf("chunks %+v", chunks)
	}
	if !chunks[2].Done || chunks[2].TokensPerSecond != 7.5 || chunks[2].Content != "" {
		t.Fatalf("final chunk %+v", chunks[2])
	}
}

func TestGenerateStreamFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []string
		match  string
	}{
		{"nostop", []string{`{"content":"partial","stop":false}`}, "without completion"},
		{"malformed", []string{`{`}, "invalid backend stream chunk"},
		{"truncated", []string{`{"content":"x","truncated":true}`}, "truncated"},
		{"negative", []string{`{"content":"x","stop":true,"timings":{"predicted_per_second":-1}}`}, "timing"},
		{"doneearly", []string{`[DONE]`}, "without final chunk"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, calls, _ := streamFixture(t, tc.events...)
			err := l.GenerateStream(context.Background(), Request{"tiny", "hello", 8, 32}, func(Chunk) error { return nil })
			if err == nil || !strings.Contains(err.Error(), tc.match) {
				t.Fatalf("error %v", err)
			}
			if *calls != 1 {
				t.Fatalf("calls %d", *calls)
			}
		})
	}
}

func TestGenerateStreamEmitError(t *testing.T) {
	l, _, _ := streamFixture(t,
		`{"content":"one","stop":false}`,
		`{"content":"two","stop":true,"timings":{"predicted_per_second":3}}`,
	)
	stop := errors.New("client gone")
	err := l.GenerateStream(context.Background(), Request{"tiny", "hello", 8, 32}, func(Chunk) error { return stop })
	if !errors.Is(err, stop) {
		t.Fatalf("error %v", err)
	}
}

func TestGenerateStreamRejects(t *testing.T) {
	l, calls, streamed := streamFixture(t)
	for _, request := range []Request{
		{"", "hello", 8, 32},
		{"tiny", " ", 8, 32},
		{"tiny", "hello", 0, 32},
		{"tiny", "hello", 8, 8},
		{"tiny", strings.Repeat("x", MaxPromptBytes+1), 8, 32},
	} {
		if err := l.GenerateStream(context.Background(), request, func(Chunk) error { return nil }); err == nil {
			t.Fatalf("accepted %+v", request)
		}
	}
	if *calls != 0 || *streamed {
		t.Fatal("invalid request reached backend")
	}
}

func TestGenerateStreamCanceled(t *testing.T) {
	l, calls, _ := streamFixture(t, `{"content":"x","stop":true,"timings":{"predicted_per_second":1}}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.GenerateStream(ctx, Request{"tiny", "hello", 8, 32}, func(Chunk) error { return nil }); err == nil {
		t.Fatal("ignored cancellation")
	}
	if *calls != 0 {
		t.Fatal("canceled request reached backend")
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
