package agent_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"meshcompute/internal/agent"
	"meshcompute/internal/config"
	"meshcompute/internal/dashboard"
	"meshcompute/internal/security"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type testWorker struct {
	dir string
	id  string
	a   *agent.Agent
}

func joinWorker(t *testing.T, ctx context.Context, hostDir string, hc config.Config, name string, tweak func(*config.Config)) testWorker {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	code, err := security.Pair(hostDir, hc.Advertise, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := security.JoinHost(ctx, dir, code, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	l := listen(t)
	c := config.Default()
	c.Listen = l.Addr().String()
	c.Advertise = "https://" + c.Listen
	c.ShareGPU = "off"
	if tweak != nil {
		tweak(&c)
	}
	if err := config.Save(dir, c); err != nil {
		t.Fatal(err)
	}
	a := agent.New(dir, c, io.Discard)
	go func() { _ = a.ServeListener(ctx, l) }()
	id, _ := security.NodeID(dir)
	return testWorker{dir: dir, id: id, a: a}
}

// TestFleetAndTerminalGridEndToEnd: a host with two real workers. The fleet
// view shows all three computers with a measured round trip to each worker
// and the tasks running on them; the terminal grid opens one shell per
// worker, input typed into worker A's pane reaches only worker A, "type into
// all" reaches both, and all output arrives over one multiplexed stream.
func TestFleetAndTerminalGridEndToEnd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("remote shells in this test use the Linux pseudo-terminal")
	}
	t.Setenv("SHELL", "/bin/sh")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hostDir := filepath.Join(t.TempDir(), "host")
	hl := listen(t)
	hc := config.Default()
	hc.Listen = hl.Addr().String()
	hc.Advertise = "https://" + hc.Listen
	hc.GroupHost = true
	if err := security.InitGroup(hostDir, hc.Listen, hc.Advertise, false); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(hostDir, hc); err != nil {
		t.Fatal(err)
	}
	startAgent(t, ctx, hostDir, hc, hl)
	allow := func(c *config.Config) { c.AllowTerminal, c.TaskApproval = true, "auto" }
	wa := joinWorker(t, ctx, hostDir, hc, "worker-a", allow)
	wb := joinWorker(t, ctx, hostDir, hc, "worker-b", allow)

	panel, err := dashboard.New(dashboard.NewRemote(hostDir, hc), freeAddr(t), "")
	if err != nil {
		t.Fatal(err)
	}
	panel.EnableTerminal(hostDir)
	panel.EnableTasks(ctx)
	host := servePanel(t, ctx, panel)

	fleet := func() dashboard.FleetResponse {
		var out dashboard.FleetResponse
		if code := host.do("GET", "/api/fleet", nil, &out); code != 200 {
			t.Fatalf("fleet: HTTP %d", code)
		}
		return out
	}
	byName := func(f dashboard.FleetResponse, name string) dashboard.FleetNode {
		for _, n := range f.Nodes {
			if n.Name == name {
				return n
			}
		}
		return dashboard.FleetNode{}
	}
	waitFleet := func(what string, ok func(dashboard.FleetResponse) bool) dashboard.FleetResponse {
		t.Helper()
		deadline := time.Now().Add(40 * time.Second)
		for {
			f := fleet()
			if ok(f) {
				return f
			}
			if time.Now().After(deadline) {
				b, _ := json.MarshalIndent(f, "", " ")
				t.Fatalf("timed out waiting for %s:\n%s", what, b)
			}
			time.Sleep(400 * time.Millisecond)
		}
	}
	f := waitFleet("both workers online with a measured link", func(f dashboard.FleetResponse) bool {
		a, b := byName(f, wa.id), byName(f, wb.id)
		return len(f.Nodes) == 3 && f.Role == "host" && a.Healthy && b.Healthy && a.RTTMillis != nil && b.RTTMillis != nil &&
			a.Status.Terminal && a.Status.Tasks == "auto" && len(a.History) >= 2
	})
	if f.Nodes[0].Name != "local" || f.Nodes[0].RTTMillis != nil {
		t.Fatalf("host entry: %+v", f.Nodes[0])
	}
	if rtt := *byName(f, wa.id).RTTMillis; rtt <= 0 || rtt > 2000 {
		t.Fatalf("implausible loopback round trip %v ms", rtt)
	}

	// A running task shows on worker A's card only.
	var sent struct {
		Tasks []struct{ ID string } `json:"tasks"`
	}
	if code := host.do("POST", "/api/tasks", map[string]any{"nodes": []string{wa.id}, "command": "sleep 20", "shell": "sh"}, &sent); code != 200 {
		t.Fatalf("send: %d", code)
	}
	waitFleet("the running task on worker A's card", func(f dashboard.FleetResponse) bool {
		return byName(f, wa.id).TasksRunning == 1 && byName(f, wb.id).TasksRunning == 0
	})
	host.do("POST", "/api/tasks/cancel", map[string]any{"id": sent.Tasks[0].ID}, nil)

	// Terminal grid: one shell per worker.
	open := func(node string) string {
		t.Helper()
		var out struct{ ID, Node string }
		if code := host.do("POST", "/api/term/open", map[string]any{"node": node, "cols": 100, "rows": 30}, &out); code != 200 || out.Node != node {
			t.Fatalf("open on %s: HTTP %d %+v", node, code, out)
		}
		return out.ID
	}
	ida, idb := open(wa.id), open(wb.id)
	deadline := time.Now().Add(10 * time.Second)
	for wa.a.TerminalCount() != 1 || wb.a.TerminalCount() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("each worker must run exactly one shell: a=%d b=%d", wa.a.TerminalCount(), wb.a.TerminalCount())
		}
		time.Sleep(50 * time.Millisecond)
	}
	var listed struct {
		Sessions []struct{ ID, Node string } `json:"sessions"`
	}
	host.do("GET", "/api/term/list", nil, &listed)
	nodes := map[string]string{}
	for _, s := range listed.Sessions {
		nodes[s.ID] = s.Node
	}
	if nodes[ida] != wa.id || nodes[idb] != wb.id {
		t.Fatalf("session list %+v", listed)
	}
	waitFleet("terminal counts on the cards", func(f dashboard.FleetResponse) bool {
		return byName(f, wa.id).Terminals == 1 && byName(f, wb.id).Terminals == 1
	})

	req, _ := http.NewRequest("GET", host.base+"/api/term/multi?s="+ida+":0,"+idb+":0", nil)
	req.Header.Set("X-MeshCompute-Token", host.token)
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("multi stream: %v %v", err, res)
	}
	defer res.Body.Close()
	var mu sync.Mutex
	out := map[string]*strings.Builder{ida: {}, idb: {}}
	exits := map[string]int{}
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		event := ""
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event:"):
				event = strings.TrimSpace(line[6:])
			case strings.HasPrefix(line, "data:"):
				var v struct {
					ID   string `json:"id"`
					Data string `json:"data"`
					Code int    `json:"code"`
				}
				_ = json.Unmarshal([]byte(strings.TrimSpace(line[5:])), &v)
				mu.Lock()
				if event == "output" && out[v.ID] != nil {
					b, _ := base64.StdEncoding.DecodeString(v.Data)
					out[v.ID].Write(b)
				}
				if event == "exit" {
					exits[v.ID] = v.Code
				}
				done := len(exits) == 2
				mu.Unlock()
				if done {
					return
				}
			}
		}
	}()
	input := func(body map[string]any) {
		t.Helper()
		if code := host.do("POST", "/api/term/input", body, nil); code != http.StatusNoContent {
			t.Fatalf("input %v: HTTP %d", body, code)
		}
	}
	input(map[string]any{"ids": []string{ida, idb}, "data": "echo both-$((20+22))\n"})
	input(map[string]any{"id": ida, "data": "echo only-on-a; exit 5\n"})
	// Worker A's shell ends; worker B's must still be open.
	deadline = time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		_, aDone := exits[ida]
		_, bDone := exits[idb]
		mu.Unlock()
		if aDone {
			if bDone {
				t.Fatal("exit typed into worker A ended worker B's shell")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker A's shell did not exit")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if wb.a.TerminalCount() != 1 {
		t.Fatalf("worker B lost its shell: %d", wb.a.TerminalCount())
	}
	input(map[string]any{"id": idb, "data": "echo only-on-b; exit 6\n"})
	select {
	case <-ended:
	case <-time.After(15 * time.Second):
		t.Fatal("multi stream did not report both exits")
	}
	mu.Lock()
	defer mu.Unlock()
	a, b := out[ida].String(), out[idb].String()
	if exits[ida] != 5 || exits[idb] != 6 {
		t.Fatalf("exit codes %v", exits)
	}
	if !strings.Contains(a, "both-42") || !strings.Contains(b, "both-42") {
		t.Fatalf("broadcast missing: a=%q b=%q", a, b)
	}
	if !strings.Contains(a, "only-on-a\r\n") || strings.Contains(b, "only-on-a") || !strings.Contains(b, "only-on-b\r\n") || strings.Contains(a, "only-on-b") {
		t.Fatalf("input reached the wrong worker: a=%q b=%q", a, b)
	}
}
