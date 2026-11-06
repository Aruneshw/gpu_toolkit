package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"meshcompute/internal/agent"
	"meshcompute/internal/backend"
	"meshcompute/internal/resource"
	"meshcompute/internal/sse"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type fakeCluster struct {
	nodes     []Node
	statusErr error
	streamErr error
	chunks    []backend.Chunk
	gotNode   string
	gotReq    backend.Request
	streams   int
}

func (f *fakeCluster) Statuses(context.Context) ([]Node, error) {
	return f.nodes, f.statusErr
}

func (f *fakeCluster) Stream(_ context.Context, node string, req backend.Request, emit func(backend.Chunk) error) error {
	f.streams++
	f.gotNode = node
	f.gotReq = req
	for _, c := range f.chunks {
		if err := emit(c); err != nil {
			return err
		}
	}
	return f.streamErr
}

func u64(v uint64) *uint64 { return &v }

func f64(v float64) *float64 { return &v }

func newServer(t *testing.T, cl Cluster) *Server {
	t.Helper()
	s, err := New(cl, "127.0.0.1:8081", testToken)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func request(server *Server, method, path string, body io.Reader, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://127.0.0.1:8081"+path, body)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	return rec
}

func TestNewValidation(t *testing.T) {
	cl := &fakeCluster{}
	for _, listen := range []string{"0.0.0.0:8081", "10.0.0.1:8081", "127.0.0.1", "127.0.0.1:0", "127.0.0.1:99999"} {
		if _, err := New(cl, listen, testToken); err == nil {
			t.Fatalf("accepted listen %q", listen)
		}
	}
	if _, err := New(cl, "127.0.0.1:8081", "short"); err == nil {
		t.Fatal("accepted short token")
	}
	if _, err := New(nil, "127.0.0.1:8081", testToken); err == nil {
		t.Fatal("accepted nil cluster")
	}
	s, err := New(cl, "[::1]:8081", "")
	if err != nil || !validToken(s.Token()) {
		t.Fatalf("%v token %q", err, s.Token())
	}
}

func TestAuthAndHost(t *testing.T) {
	s := newServer(t, &fakeCluster{})
	if rec := request(s, http.MethodGet, "/", nil, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("no token status %d", rec.Code)
	}
	if rec := request(s, http.MethodGet, "/", nil, map[string]string{"X-MeshCompute-Token": "wrong"}); rec.Code != http.StatusForbidden {
		t.Fatalf("wrong token status %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "http://evil.example:8081/", nil)
	req.Header.Set("X-MeshCompute-Token", testToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bad host status %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9999/api/status", nil)
	req.Header.Set("X-MeshCompute-Token", testToken)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bad port status %d", rec.Code)
	}

	rec = request(s, http.MethodGet, "/?token="+testToken, nil, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("token bootstrap status %d", rec.Code)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != cookieName || !cookies[0].HttpOnly {
		t.Fatalf("cookies %+v", cookies)
	}
	req = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8081/", nil)
	req.AddCookie(cookies[0])
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "MeshCompute") {
		t.Fatalf("cookie page status %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'none'") {
		t.Fatalf("csp %q", got)
	}
}

func TestStatusAggregate(t *testing.T) {
	cl := &fakeCluster{nodes: []Node{
		{Name: "local", Healthy: true, Status: &agent.Status{
			Resources: resource.Snapshot{
				RAMTotalBytes:     u64(16 << 30),
				RAMAvailableBytes: u64(10 << 30),
				GPUs: []resource.GPU{
					{Name: "RTX 3050", TotalBytes: u64(6 << 30), FreeBytes: u64(5 << 30), UtilizationPercent: f64(12)},
					{Name: "RTX 4090", TotalBytes: u64(24 << 30), FreeBytes: u64(20 << 30), UtilizationPercent: f64(0)},
				},
			},
			Backend: &backend.Capabilities{Models: []string{"lfm3"}, Context: 2048},
		}},
		{Name: "win", Healthy: true, Status: &agent.Status{
			Resources: resource.Snapshot{
				RAMTotalBytes:     u64(8 << 30),
				RAMAvailableBytes: u64(4 << 30),
				GPUs:              []resource.GPU{{Name: "RTX 4060", TotalBytes: u64(8 << 30), FreeBytes: u64(7 << 30)}},
			},
			Backend: &backend.Capabilities{Models: []string{"lfm3", "qwen"}, Context: 4096},
		}},
		{Name: "offline", Healthy: false, Error: "unreachable"},
	}}
	s := newServer(t, cl)
	rec := request(s, http.MethodGet, "/api/status", nil, map[string]string{"X-MeshCompute-Token": testToken})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var got StatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	a := got.Aggregate
	if a.Nodes != 3 || a.Healthy != 2 || a.GPUs != 3 {
		t.Fatalf("counts %+v", a)
	}
	if a.VRAMTotalBytes != 38<<30 || a.VRAMFreeBytes != 32<<30 {
		t.Fatalf("vram %+v", a)
	}
	if a.RAMTotalBytes != 24<<30 || a.RAMAvailableBytes != 14<<30 {
		t.Fatalf("ram %+v", a)
	}
	if strings.Join(a.Models, ",") != "lfm3,qwen" {
		t.Fatalf("models %v", a.Models)
	}
	if !strings.Contains(got.Scope, "not pooled") {
		t.Fatalf("scope %q", got.Scope)
	}
}

func TestStatusFailure(t *testing.T) {
	s := newServer(t, &fakeCluster{statusErr: errors.New("no identity")})
	rec := request(s, http.MethodGet, "/api/status", nil, map[string]string{"X-MeshCompute-Token": testToken})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestRunStream(t *testing.T) {
	cl := &fakeCluster{chunks: []backend.Chunk{{Content: "Hello"}, {Content: " world"}, {Done: true, TokensPerSecond: 7.5}}}
	s := newServer(t, cl)
	body := `{"node":"local","model":"lfm3","prompt":"hi","max_tokens":8,"context":32}`
	rec := request(s, http.MethodPost, "/api/run", strings.NewReader(body), map[string]string{
		"Content-Type":        "application/json",
		"X-MeshCompute-Token": testToken,
		"Origin":              "http://127.0.0.1:8081",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var content strings.Builder
	rate := 0.0
	err := sse.Read(rec.Body, func(event string, data []byte) error {
		switch event {
		case "token":
			var v struct {
				Content string `json:"content"`
			}
			if json.Unmarshal(data, &v) != nil {
				t.Fatal("bad token event")
			}
			content.WriteString(v.Content)
		case "done":
			var v struct {
				Rate float64 `json:"tokens_per_second"`
			}
			if json.Unmarshal(data, &v) != nil {
				t.Fatal("bad done event")
			}
			rate = v.Rate
			return sse.Done
		case "error":
			t.Fatalf("error event %s", data)
		}
		return nil
	})
	if err != nil || content.String() != "Hello world" || rate != 7.5 {
		t.Fatalf("content=%q rate=%v err=%v", content.String(), rate, err)
	}
	if cl.gotNode != "local" || cl.gotReq.Model != "lfm3" || cl.gotReq.MaxTokens != 8 || cl.gotReq.Context != 32 {
		t.Fatalf("dispatch %+v %+v", cl.gotNode, cl.gotReq)
	}
}

func TestRunRejects(t *testing.T) {
	cl := &fakeCluster{}
	s := newServer(t, cl)
	cases := []struct {
		name    string
		body    string
		headers map[string]string
		code    int
	}{
		{"origin", `{"node":"local","model":"lfm3","prompt":"hi","max_tokens":8,"context":32}`, map[string]string{"X-MeshCompute-Token": testToken, "Origin": "http://evil.example"}, http.StatusForbidden},
		{"unknown", `{"unexpected":true}`, map[string]string{"X-MeshCompute-Token": testToken}, http.StatusBadRequest},
		{"node", `{"node":"","model":"lfm3","prompt":"hi","max_tokens":8,"context":32}`, map[string]string{"X-MeshCompute-Token": testToken}, http.StatusBadRequest},
		{"budget", `{"node":"local","model":"lfm3","prompt":"hi","max_tokens":32,"context":32}`, map[string]string{"X-MeshCompute-Token": testToken}, http.StatusBadRequest},
		{"trailing", `{"node":"local","model":"lfm3","prompt":"hi","max_tokens":8,"context":32} {}`, map[string]string{"X-MeshCompute-Token": testToken}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := request(s, http.MethodPost, "/api/run", strings.NewReader(tc.body), tc.headers)
			if rec.Code != tc.code {
				t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
			}
		})
	}
	if cl.streams != 0 {
		t.Fatal("invalid request dispatched")
	}
	if rec := request(s, http.MethodGet, "/api/run", nil, map[string]string{"X-MeshCompute-Token": testToken}); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status %d", rec.Code)
	}
}

func TestRunStreamErrorEvent(t *testing.T) {
	s := newServer(t, &fakeCluster{streamErr: errors.New("backend down")})
	body := `{"node":"local","model":"lfm3","prompt":"hi","max_tokens":8,"context":32}`
	rec := request(s, http.MethodPost, "/api/run", strings.NewReader(body), map[string]string{"X-MeshCompute-Token": testToken})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	seen := false
	err := sse.Read(rec.Body, func(event string, data []byte) error {
		if event == "error" {
			seen = true
			return sse.Done
		}
		return nil
	})
	if err != nil || !seen {
		t.Fatalf("err=%v seen=%v", err, seen)
	}
}
