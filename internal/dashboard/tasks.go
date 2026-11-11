package dashboard

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"meshcompute/internal/agent"
	"meshcompute/internal/backend"
	"meshcompute/internal/security"
	"meshcompute/internal/sse"
	"meshcompute/internal/task"
	"meshcompute/internal/tunnel"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// WorkerConsole is what a worker's own dashboard controls: the tasks the
// host sent, the owner's settings and the connection to the host.
// *agent.Agent implements it.
type WorkerConsole interface {
	TaskRunner() *task.Worker
	Settings() agent.WorkerSettings
	UpdateSettings(agent.WorkerSettings) (agent.WorkerSettings, error)
	Link() agent.HostLink
	TerminalCount() int
	CloseTerminals()
	Status(context.Context) (agent.Status, error)
}

// tasks is the task list this dashboard shows: on a host the tasks it sent,
// on a worker the tasks it received.
type tasks interface {
	List() []task.Info
	Get(id string) (*task.Record, bool)
	Cancel(id string) error
}

type hostTasks struct{ *task.Host }

func (h hostTasks) List() []task.Info                  { return h.Book.List() }
func (h hostTasks) Get(id string) (*task.Record, bool) { return h.Book.Get(id) }

type workerTasks struct{ *task.Worker }

func (w workerTasks) List() []task.Info                  { return w.Book.List() }
func (w workerTasks) Get(id string) (*task.Record, bool) { return w.Book.Get(id) }

// TaskStreamDialer opens a task stream on a worker of a group this computer
// hosts.
type TaskStreamDialer interface {
	DialTask(ctx context.Context, node string) (io.ReadWriteCloser, error)
}

// DialTask reaches a worker's task runner through this host's agent.
func (r *Remote) DialTask(ctx context.Context, node string) (io.ReadWriteCloser, error) {
	if !r.c.GroupHost || !strings.HasPrefix(node, "worker-") {
		return nil, errors.New("tasks can be sent to workers of a group you host")
	}
	endpoint, err := r.endpoint(ctx, node)
	if err != nil {
		return nil, errors.New("that worker is not connected")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	cfg, err := security.ClientTLS(r.dir)
	if err != nil {
		return nil, err
	}
	conn, err := tunnel.DialStream(ctx, "https://"+u.Host, u.Path+agent.StreamTask, cfg)
	if err != nil {
		if strings.Contains(err.Error(), "HTTP 403") {
			return nil, errors.New("the owner of that computer has turned tasks off")
		}
		return nil, errors.New("that worker is offline or busy")
	}
	return conn, nil
}

// EnableTasks lets a group host's dashboard send tasks to its workers.
// Following stops when ctx ends.
func (s *Server) EnableTasks(ctx context.Context) {
	if d, ok := s.cluster.(TaskStreamDialer); ok {
		s.tasks = hostTasks{task.NewHost(ctx, d.DialTask)}
		s.role = "host"
	}
}

// localWorker adapts a worker's own agent to the Cluster interface, so the
// worker dashboard shows this computer only.
type localWorker struct{ c WorkerConsole }

func (l localWorker) Statuses(ctx context.Context) ([]Node, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	st, err := l.c.Status(ctx)
	if err != nil {
		return []Node{{Name: "local", Error: "resource discovery unavailable"}}, nil
	}
	return []Node{{Name: "local", Healthy: true, Status: &st}}, nil
}

func (l localWorker) Stream(context.Context, string, backend.Request, func(backend.Chunk) error) error {
	return errors.New("prompts are run from the group host's dashboard")
}

// NewWorker returns the dashboard a worker's owner uses: what the host is
// doing on this computer, approvals, and settings.
func NewWorker(c WorkerConsole, listen, token string) (*Server, error) {
	s, err := New(localWorker{c}, listen, token)
	if err != nil {
		return nil, err
	}
	s.worker, s.role = c, "worker"
	return s, nil
}

func (s *Server) taskList() tasks {
	if s.worker != nil {
		if w := s.worker.TaskRunner(); w != nil {
			return workerTasks{w}
		}
		return nil
	}
	return s.tasks
}

func (s *Server) taskRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/tasks", s.handleTasks)
	mux.HandleFunc("/api/tasks/stream", s.handleTaskStream)
	mux.HandleFunc("/api/tasks/output", s.handleTaskOutput)
	mux.HandleFunc("/api/tasks/cancel", s.handleTaskCancel)
	mux.HandleFunc("/api/tasks/decide", s.handleTaskDecide)
	mux.HandleFunc("/api/worker", s.handleWorker)
	mux.HandleFunc("/api/worker/terminals/close", s.handleWorkerTerminals)
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	list := s.taskList()
	switch r.Method {
	case http.MethodGet:
		infos := []task.Info{}
		if list != nil {
			infos = list.List()
		}
		writeJSON(w, map[string]any{"role": s.role, "tasks": infos})
	case http.MethodPost:
		host, ok := s.tasks.(hostTasks)
		if !ok || s.worker != nil {
			writeError(w, http.StatusNotFound, "only a group host sends tasks")
			return
		}
		var req struct {
			Nodes   []string `json:"nodes"`
			Command string   `json:"command"`
			Shell   string   `json:"shell"`
			Dir     string   `json:"dir"`
		}
		if !decode(w, r, task.MaxCommand+16<<10, &req) {
			return
		}
		for _, n := range req.Nodes {
			if !strings.HasPrefix(n, "worker-") || len(n) > 100 {
				writeError(w, http.StatusBadRequest, "tasks go to connected workers only")
				return
			}
		}
		infos, err := host.Send(req.Nodes, task.Spec{Command: req.Command, Shell: req.Shell, Dir: strings.TrimSpace(req.Dir)})
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, map[string]any{"tasks": infos})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleTaskCancel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if !decode(w, r, 1024, &req) {
		return
	}
	list := s.taskList()
	if list == nil {
		writeError(w, http.StatusNotFound, "no tasks here")
		return
	}
	if err := list.Cancel(req.ID); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTaskDecide(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID      string `json:"id"`
		Approve bool   `json:"approve"`
		// Always also switches this computer to run future tasks without asking.
		Always bool `json:"always"`
	}
	if !decode(w, r, 1024, &req) {
		return
	}
	if s.worker == nil || s.worker.TaskRunner() == nil {
		writeError(w, http.StatusNotFound, "only a worker approves tasks")
		return
	}
	if req.Approve && req.Always {
		st := s.worker.Settings()
		st.TaskApproval = "auto"
		if _, err := s.worker.UpdateSettings(st); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if err := s.worker.TaskRunner().Decide(req.ID, req.Approve); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTaskOutput returns a task's output from offset on (at most 256 KiB)
// and its state. The page polls it, which keeps the browser's few
// connections per site free for terminals.
func (s *Server) handleTaskOutput(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	list := s.taskList()
	if list == nil {
		writeError(w, http.StatusNotFound, "no tasks here")
		return
	}
	rec, ok := list.Get(r.URL.Query().Get("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown task")
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil {
		offset = 0
	}
	data, next, info, _ := rec.Read(offset)
	start := next - int64(len(data))
	if len(data) > 256<<10 {
		data = data[:256<<10]
		next = start + int64(len(data))
	}
	writeJSON(w, map[string]any{"data": base64.StdEncoding.EncodeToString(data), "offset": start, "next": next, "info": info})
}

// handleTaskStream sends a task's output and state as server-sent events,
// from offset on, until the task ends or the browser leaves.
func (s *Server) handleTaskStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	list := s.taskList()
	if list == nil {
		writeError(w, http.StatusNotFound, "no tasks here")
		return
	}
	rec, ok := list.Get(r.URL.Query().Get("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown task")
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil {
		offset = 0
	}
	sse.Prepare(w)
	sw := sse.NewWriter(w)
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	var last string
	for {
		data, next, info, wake := rec.Read(offset)
		for start := next - int64(len(data)); len(data) > 0; {
			chunk := data
			if len(chunk) > 48<<10 {
				chunk = chunk[:48<<10]
			}
			data = data[len(chunk):]
			if err := sw.Event("output", map[string]any{"data": base64.StdEncoding.EncodeToString(chunk), "offset": start}); err != nil {
				return
			}
			start += int64(len(chunk))
		}
		offset = next
		if key := info.State + "\x00" + info.Detail; key != last {
			if err := sw.Event("state", info); err != nil {
				return
			}
			last = key
		}
		if task.Final(info.State) {
			_ = sw.Event("end", info)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-wake:
		case <-heartbeat.C:
			if err := sw.Event("ping", map[string]bool{"ok": true}); err != nil {
				return
			}
		}
	}
}

// handleWorker reports (GET) or changes (POST) the worker's settings, its
// link to the host and the shells the host has open here.
func (s *Server) handleWorker(w http.ResponseWriter, r *http.Request) {
	if s.worker == nil {
		writeError(w, http.StatusNotFound, "this dashboard is not a worker's")
		return
	}
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		var req agent.WorkerSettings
		if !decode(w, r, 1024, &req) {
			return
		}
		if _, err := s.worker.UpdateSettings(req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, map[string]any{"settings": s.worker.Settings(), "link": s.worker.Link(), "terminals": s.worker.TerminalCount()})
}

func (s *Server) handleWorkerTerminals(w http.ResponseWriter, r *http.Request) {
	var req struct{}
	if !decode(w, r, 64, &req) {
		return
	}
	if s.worker == nil {
		writeError(w, http.StatusNotFound, "this dashboard is not a worker's")
		return
	}
	s.worker.CloseTerminals()
	w.WriteHeader(http.StatusNoContent)
}
