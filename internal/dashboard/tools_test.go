package dashboard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

type invitingCluster struct {
	fakeCluster
	ttl time.Duration
	err error
}

func (c *invitingCluster) Invite(_ context.Context, ttl time.Duration) (Invitation, error) {
	c.ttl = ttl
	if c.err != nil {
		return Invitation{}, c.err
	}
	return Invitation{Code: "CODE123", Endpoint: "https://[2001:db8::1]:7443", ExpiresAt: time.Now().Add(ttl), Commands: JoinCommands("CODE123"), Download: ReleasesURL}, nil
}

var auth = map[string]string{"X-MeshCompute-Token": testToken, "Origin": "http://127.0.0.1:8081", "Content-Type": "application/json"}

func TestInviteAPI(t *testing.T) {
	cl := &invitingCluster{}
	s := newServer(t, cl)
	rec := request(s, http.MethodPost, "/api/invite", strings.NewReader(`{"ttl_minutes":60}`), auth)
	if rec.Code != http.StatusOK {
		t.Fatalf("invite: %d %s", rec.Code, rec.Body)
	}
	var inv Invitation
	if err := json.Unmarshal(rec.Body.Bytes(), &inv); err != nil {
		t.Fatal(err)
	}
	if cl.ttl != time.Hour || inv.Code != "CODE123" || len(inv.Commands) != 3 {
		t.Fatalf("unexpected invitation %+v ttl %s", inv, cl.ttl)
	}
	for _, c := range inv.Commands {
		if !strings.HasSuffix(c.Command, " CODE123") || !strings.Contains(c.Command, "join") {
			t.Fatalf("command does not embed the code: %q", c.Command)
		}
	}
	for _, body := range []string{`{"ttl_minutes":0}`, `{"ttl_minutes":1441}`, `{"ttl":5}`, `nope`} {
		if rec := request(s, http.MethodPost, "/api/invite", strings.NewReader(body), auth); rec.Code != http.StatusBadRequest {
			t.Fatalf("accepted %s: %d", body, rec.Code)
		}
	}
	// Cross-origin and unauthenticated requests are refused by the guard.
	bad := map[string]string{"X-MeshCompute-Token": testToken, "Origin": "http://evil.example"}
	if rec := request(s, http.MethodPost, "/api/invite", strings.NewReader(`{"ttl_minutes":5}`), bad); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin invite: %d", rec.Code)
	}
	if rec := request(s, http.MethodPost, "/api/invite", strings.NewReader(`{"ttl_minutes":5}`), map[string]string{"Origin": "http://127.0.0.1:8081"}); rec.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated invite: %d", rec.Code)
	}
	cl.err = errors.New("boom")
	if rec := request(s, http.MethodPost, "/api/invite", strings.NewReader(`{"ttl_minutes":5}`), auth); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "boom") {
		t.Fatalf("error not reported: %d %s", rec.Code, rec.Body)
	}
	plain := newServer(t, &fakeCluster{})
	if rec := request(plain, http.MethodPost, "/api/invite", strings.NewReader(`{"ttl_minutes":5}`), auth); rec.Code != http.StatusNotFound {
		t.Fatalf("invite without inviter: %d", rec.Code)
	}
}

func TestStaticAndInfo(t *testing.T) {
	s := newServer(t, &fakeCluster{})
	rec := request(s, http.MethodGet, "/static/xterm.js", nil, auth)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") || rec.Body.Len() < 100000 {
		t.Fatalf("xterm.js: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	for _, p := range []string{"/static/../index.html", "/static/missing.js", "/static/index.html", "/static/"} {
		if rec := request(s, http.MethodGet, p, nil, auth); rec.Code == http.StatusOK {
			t.Fatalf("%s: %d", p, rec.Code)
		}
	}
	if rec := request(s, http.MethodGet, "/static/xterm.js", nil, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("static without token: %d", rec.Code)
	}
	rec = request(s, http.MethodGet, "/api/info", nil, auth)
	var info map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil || info["terminal"] != false || info["invites"] != false {
		t.Fatalf("info: %s", rec.Body)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("csp: %s", csp)
	}
	if rec := request(s, http.MethodPost, "/api/term/open", strings.NewReader(`{}`), auth); rec.Code != http.StatusNotFound {
		t.Fatalf("terminal enabled by default: %d", rec.Code)
	}
}

func TestTerminalOverHTTP(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("pseudo-terminal backend is Linux only")
	}
	t.Setenv("SHELL", "/bin/sh")
	s := newServer(t, &fakeCluster{})
	s.EnableTerminal(t.TempDir())
	defer s.term.CloseAll()
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	// The guard validates Host against the configured port; rewrite it.
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
	res := do(http.MethodPost, "/api/term/open", `{"cols":90,"rows":20}`)
	var opened struct{ ID string }
	_ = json.NewDecoder(res.Body).Decode(&opened)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || len(opened.ID) != 32 {
		t.Fatalf("open: %d %q", res.StatusCode, opened.ID)
	}
	stream := do(http.MethodGet, "/api/term/stream?id="+opened.ID+"&offset=-1", "")
	defer stream.Body.Close()
	if res := do(http.MethodPost, "/api/term/input", `{"id":"`+opened.ID+`","data":"echo mark-$((40+2)); echo home=$MESHCOMPUTE_HOME; stty size; exit 7\n"}`); res.StatusCode != http.StatusNoContent {
		t.Fatalf("input: %d", res.StatusCode)
	}
	var output strings.Builder
	code := -1
	done := make(chan error, 1)
	go func() {
		done <- readEvents(stream.Body, func(event string, data []byte) bool {
			var v struct {
				Data string `json:"data"`
				Code int    `json:"code"`
			}
			_ = json.Unmarshal(data, &v)
			switch event {
			case "output":
				b, _ := base64.StdEncoding.DecodeString(v.Data)
				output.Write(b)
			case "exit":
				code = v.Code
				return false
			}
			return true
		})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("terminal stream timed out")
	}
	out := output.String()
	if !strings.Contains(out, "mark-42") || !strings.Contains(out, "home=/") || !strings.Contains(out, "20 90") || code != 7 {
		t.Fatalf("unexpected terminal output %q exit %d", out, code)
	}
	time.Sleep(50 * time.Millisecond)
	if res := do(http.MethodPost, "/api/term/input", `{"id":"`+opened.ID+`","data":"x"}`); res.StatusCode != http.StatusGone {
		t.Fatalf("input after exit: %d", res.StatusCode)
	}
}

func readEvents(r io.Reader, fn func(string, []byte) bool) error {
	b := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		b = append(b, buf[:n]...)
		for {
			i := strings.Index(string(b), "\n\n")
			if i < 0 {
				break
			}
			block := string(b[:i])
			b = b[i+2:]
			event, data := "", ""
			for _, line := range strings.Split(block, "\n") {
				if strings.HasPrefix(line, "event: ") {
					event = line[7:]
				} else if strings.HasPrefix(line, "data: ") {
					data = line[6:]
				}
			}
			if !fn(event, []byte(data)) {
				return nil
			}
		}
		if err != nil {
			return err
		}
	}
}
