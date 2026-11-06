package network

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMeasure(t *testing.T) {
	s := httptest.NewServer(Handler())
	defer s.Close()
	q, e := Measure(context.Background(), s.Client(), s.URL)
	if e != nil || !q.Available || !q.Stable || q.Requests != 4 || q.Failures != 0 || q.ThroughputBytesPerSecond <= 0 || q.Latency <= 0 || q.MeasuredAt.IsZero() {
		t.Fatalf("%+v %v", q, e)
	}
}
func TestFailures(t *testing.T) {
	for _, kind := range []string{"status", "wrong", "oversized", "redirect", "partial"} {
		t.Run(kind, func(t *testing.T) {
			h := Handler()
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch kind {
				case "status":
					w.WriteHeader(503)
				case "wrong":
					_, _ = w.Write([]byte("bad"))
				case "oversized":
					_, _ = w.Write([]byte(strings.Repeat("x", ProbeBytes+1)))
				case "redirect":
					http.Redirect(w, r, "/v1/ping", 302)
				case "partial":
					if r.URL.Path == "/v1/ping" {
						w.WriteHeader(500)
					} else {
						h.ServeHTTP(w, r)
					}
				}
			}))
			defer s.Close()
			q, e := Measure(context.Background(), s.Client(), s.URL)
			if e == nil || q.Stable || q.Failures == 0 || q.RequestFailureRate <= 0 {
				t.Fatalf("%+v %v", q, e)
			}
		})
	}
}
func TestCancelled(t *testing.T) {
	s := httptest.NewServer(Handler())
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	q, e := Measure(ctx, s.Client(), s.URL)
	if e == nil || q.Available || q.Failures != 4 {
		t.Fatalf("%+v %v", q, e)
	}
}
func TestInvalidURL(t *testing.T) {
	for _, s := range []string{"", "file:///tmp/probe", "http://user:pass@localhost", "http://localhost?q=1", "http://localhost#f", "://"} {
		if _, e := Measure(context.Background(), nil, s); e == nil {
			t.Errorf("accepted %q", s)
		}
	}
}
func TestHandlerContract(t *testing.T) {
	h := Handler()
	for _, tc := range []struct {
		method, path string
		status, size int
	}{{"GET", "/v1/ping", 200, 5}, {"GET", "/v1/probe", 200, ProbeBytes}, {"POST", "/v1/probe", 405, -1}} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status || (tc.size >= 0 && w.Body.Len() != tc.size) {
			t.Fatalf("%+v: %d %d", tc, w.Code, w.Body.Len())
		}
	}
}
