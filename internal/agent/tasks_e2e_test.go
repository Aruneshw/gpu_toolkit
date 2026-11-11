package agent_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"meshcompute/internal/agent"
	"meshcompute/internal/config"
	"meshcompute/internal/dashboard"
	"meshcompute/internal/security"
	"meshcompute/internal/task"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// panelClient talks to a dashboard the way its page does.
type panelClient struct {
	t     *testing.T
	base  string
	token string
}

func (p panelClient) do(method, path string, body, out any) int {
	p.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, p.base+path, rd)
	req.Header.Set("X-MeshCompute-Token", p.token)
	req.Header.Set("Content-Type", "application/json")
	if method == http.MethodPost {
		req.Header.Set("Origin", p.base)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		p.t.Fatal(err)
	}
	defer res.Body.Close()
	if out != nil {
		_ = json.NewDecoder(res.Body).Decode(out)
	}
	return res.StatusCode
}

func servePanel(t *testing.T, ctx context.Context, s *dashboard.Server) panelClient {
	t.Helper()
	go func() { _ = s.Serve(ctx) }()
	p := panelClient{t: t, base: "http://" + s.Listen(), token: s.Token()}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if res, err := http.Get(p.base + "/api/info"); err == nil {
			res.Body.Close()
			return p
		}
		if time.Now().After(deadline) {
			t.Fatal("dashboard did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestTasksEndToEnd: the host's dashboard sends a command to a worker over
// the real TLS tunnel, the worker's own dashboard shows it waiting, the
// owner approves it there, and the host sees the output and exit code. A
// denied task never runs; with tasks off the host is refused.
func TestTasksEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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
	if err := config.Save(hostDir, hc); err != nil {
		t.Fatal(err)
	}
	startAgent(t, ctx, hostDir, hc, hl)
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
	if err := config.Save(workerDir, wc); err != nil {
		t.Fatal(err)
	}
	worker := agent.New(workerDir, wc, io.Discard)
	go func() { _ = worker.ServeListener(ctx, wl) }()
	id, _ := security.NodeID(workerDir)

	hostPanel, err := dashboard.New(dashboard.NewRemote(hostDir, hc), freeAddr(t), "")
	if err != nil {
		t.Fatal(err)
	}
	hostPanel.EnableTasks(ctx)
	host := servePanel(t, ctx, hostPanel)
	workerPanel, err := dashboard.NewWorker(worker, freeAddr(t), "")
	if err != nil {
		t.Fatal(err)
	}
	owner := servePanel(t, ctx, workerPanel)

	var info struct {
		Role  string `json:"role"`
		Tasks bool   `json:"tasks"`
	}
	if host.do("GET", "/api/info", nil, &info); info.Role != "host" || !info.Tasks {
		t.Fatalf("host info %+v", info)
	}
	if owner.do("GET", "/api/info", nil, &info); info.Role != "worker" || !info.Tasks {
		t.Fatalf("worker info %+v", info)
	}
	// Wait until the worker is connected and reports its task policy.
	deadline := time.Now().Add(30 * time.Second)
	for {
		var st dashboard.StatusResponse
		host.do("GET", "/api/status", nil, &st)
		if len(st.Nodes) == 2 && st.Nodes[1].Healthy && st.Nodes[1].Status.Tasks == "ask" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker never connected: %+v", st)
		}
		time.Sleep(100 * time.Millisecond)
	}
	var link struct {
		Link agent.HostLink `json:"link"`
	}
	if owner.do("GET", "/api/worker", nil, &link); !link.Link.Connected {
		t.Fatalf("worker dashboard does not show the link: %+v", link)
	}

	send := func(cmd string) string {
		t.Helper()
		var out struct {
			Tasks []task.Info `json:"tasks"`
		}
		if code := host.do("POST", "/api/tasks", map[string]any{"nodes": []string{id}, "command": cmd, "shell": "sh"}, &out); code != 200 || len(out.Tasks) != 1 {
			t.Fatalf("send: HTTP %d %+v", code, out)
		}
		return out.Tasks[0].ID
	}
	state := func(p panelClient, taskID string) (task.Info, string) {
		var out struct {
			Data string    `json:"data"`
			Info task.Info `json:"info"`
		}
		if code := p.do("GET", "/api/tasks/output?id="+taskID+"&offset=0", nil, &out); code != 200 {
			return task.Info{State: "missing"}, ""
		}
		b, _ := base64.StdEncoding.DecodeString(out.Data)
		return out.Info, string(b)
	}
	waitFor := func(p panelClient, taskID string, want ...string) (task.Info, string) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for {
			info, out := state(p, taskID)
			for _, w := range want {
				if info.State == w {
					return info, out
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("task %s stayed %s (%s), want %v", taskID, info.State, info.Detail, want)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	approved := send("echo ran-on-$((6*7)); pwd")
	denied := send("echo must-not-run")
	waitFor(host, approved, task.StateApproval)
	waitFor(owner, approved, task.StateApproval)
	waitFor(owner, denied, task.StateApproval)
	// The host cannot approve its own task.
	if code := host.do("POST", "/api/tasks/decide", map[string]any{"id": approved, "approve": true}, nil); code != http.StatusNotFound {
		t.Fatalf("host approved a task: HTTP %d", code)
	}
	if code := owner.do("POST", "/api/tasks/decide", map[string]any{"id": approved, "approve": true}, nil); code != http.StatusNoContent {
		t.Fatalf("approve: HTTP %d", code)
	}
	if code := owner.do("POST", "/api/tasks/decide", map[string]any{"id": denied, "approve": false}, nil); code != http.StatusNoContent {
		t.Fatalf("deny: HTTP %d", code)
	}
	info2, out := waitFor(host, approved, task.StateSucceeded, task.StateFailed)
	if info2.State != task.StateSucceeded || !strings.Contains(out, "ran-on-42") || info2.Node != id {
		t.Fatalf("approved task on host: %+v %q", info2, out)
	}
	if _, wout := waitFor(owner, approved, task.StateSucceeded); wout != out {
		t.Fatalf("worker and host output differ: %q vs %q", wout, out)
	}
	if info3, out := waitFor(host, denied, task.StateDenied, task.StateSucceeded); info3.State != task.StateDenied || strings.Contains(out, "must-not-run") {
		t.Fatalf("denied task: %+v %q", info3, out)
	}

	// "Always allow" switches the worker to auto: the next task runs alone,
	// and the host can cancel it while it runs.
	third := send("echo third")
	waitFor(owner, third, task.StateApproval)
	if code := owner.do("POST", "/api/tasks/decide", map[string]any{"id": third, "approve": true, "always": true}, nil); code != http.StatusNoContent {
		t.Fatalf("always: HTTP %d", code)
	}
	waitFor(host, third, task.StateSucceeded)
	long := send("echo started; sleep 30")
	waitFor(host, long, task.StateRunning)
	if code := host.do("POST", "/api/tasks/cancel", map[string]any{"id": long}, nil); code != http.StatusNoContent {
		t.Fatalf("cancel: HTTP %d", code)
	}
	waitFor(host, long, task.StateCancelled)
	if c, err := config.Load(workerDir); err != nil || c.TaskApproval != "auto" {
		t.Fatalf("always allow not saved: %+v %v", c.TaskApproval, err)
	}

	// Tasks off: the host is refused and nothing runs.
	var settings struct {
		Settings agent.WorkerSettings `json:"settings"`
	}
	if code := owner.do("POST", "/api/worker", map[string]any{"allow_terminal": false, "share_gpu": false, "task_approval": "off"}, &settings); code != 200 || settings.Settings.TaskApproval != "off" {
		t.Fatalf("settings: HTTP %d %+v", code, settings)
	}
	off := send("echo refused")
	if info4, _ := waitFor(host, off, task.StateFailed); !strings.Contains(info4.Detail, "turned tasks off") {
		t.Fatalf("tasks off: %+v", info4)
	}

	// A worker certificate cannot open a task stream on the host's routes.
	workerClient, err := security.Client(workerDir)
	if err != nil {
		t.Fatal(err)
	}
	defer workerClient.CloseIdleConnections()
	req, _ := http.NewRequest("GET", hc.Advertise+"/v1/workers/"+id+agent.StreamTask, nil)
	if res, err := workerClient.Do(req); err == nil {
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("worker reached the task route: HTTP %d", res.StatusCode)
		}
	}
}
