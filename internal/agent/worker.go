package agent

import (
	"context"
	"errors"
	"fmt"
	"meshcompute/internal/config"
	"meshcompute/internal/task"
	"meshcompute/internal/tunnel"
	"net/http"
	"strings"
	"time"
)

// StreamTask carries one task (a command the host asks this worker to run)
// and its live output. See package task.
const StreamTask = "/v1/stream/task"

// WorkerSettings are the choices a worker's owner can change while the
// worker runs, from the worker's own dashboard.
type WorkerSettings struct {
	// AllowTerminal lets the host open a shell here.
	AllowTerminal bool `json:"allow_terminal"`
	// TaskApproval is ask, auto or off.
	TaskApproval string `json:"task_approval"`
	// ShareGPU reports whether this computer lends its GPU (changing it
	// takes effect on the next reconnect).
	ShareGPU bool `json:"share_gpu"`
}

// HostLink is this worker's connection to its group host.
type HostLink struct {
	Endpoint  string    `json:"endpoint"`
	Connected bool      `json:"connected"`
	Since     time.Time `json:"since,omitempty"`
}

func taskPolicy(p string) string {
	if p == "" {
		return "ask"
	}
	return p
}

func (a *Agent) allowTerminal() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.c.AllowTerminal
}

func (a *Agent) autoTasks() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.c.TaskApproval == "auto"
}

func (a *Agent) tasksOff() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.c.TaskApproval == "off"
}

// TaskRunner returns the worker's task runner, or nil before the worker has
// connected to a group host.
func (a *Agent) TaskRunner() *task.Worker {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tasks
}

// IsWorker reports whether this agent belongs to a group as a worker.
func (a *Agent) IsWorker() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.worker
}

// Link reports the connection to the group host.
func (a *Agent) Link() HostLink {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.link
}

func (a *Agent) setLink(endpoint string, connected bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if connected && !a.link.Connected {
		a.link.Since = time.Now().UTC()
	}
	a.link.Endpoint, a.link.Connected = endpoint, connected
}

// Settings returns the owner's current choices.
func (a *Agent) Settings() WorkerSettings {
	a.mu.Lock()
	defer a.mu.Unlock()
	return WorkerSettings{AllowTerminal: a.c.AllowTerminal, TaskApproval: taskPolicy(a.c.TaskApproval), ShareGPU: a.c.ShareGPU != "off"}
}

// UpdateSettings applies and saves the owner's choices. Turning the terminal
// off does not end a shell that is already open; the owner closes those
// with the worker dashboard's stop button (CloseTerminals).
func (a *Agent) UpdateSettings(s WorkerSettings) (WorkerSettings, error) {
	switch s.TaskApproval {
	case "ask", "auto", "off":
	default:
		return a.Settings(), errors.New("task approval must be ask, auto or off")
	}
	c, err := config.Update(a.dir, func(c *config.Config) error {
		c.AllowTerminal, c.TaskApproval = s.AllowTerminal, s.TaskApproval
		if s.ShareGPU {
			c.ShareGPU = ""
		} else {
			c.ShareGPU = "off"
		}
		return nil
	})
	if err != nil {
		return a.Settings(), err
	}
	a.mu.Lock()
	a.c.AllowTerminal, a.c.TaskApproval, a.c.ShareGPU = c.AllowTerminal, c.TaskApproval, c.ShareGPU
	a.mu.Unlock()
	a.log.Printf("event=worker_settings terminal=%t tasks=%s share_gpu=%t", s.AllowTerminal, s.TaskApproval, s.ShareGPU)
	return a.Settings(), nil
}

// TerminalCount is the number of shells the host has open here.
func (a *Agent) TerminalCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.termOpen
}

// CloseTerminals ends every shell the host opened on this computer.
func (a *Agent) CloseTerminals() {
	a.mu.Lock()
	term := a.term
	a.mu.Unlock()
	if term != nil {
		term.CloseAll()
	}
	a.mu.Lock()
	a.term = nil
	a.mu.Unlock()
}

func (a *Agent) startTasks(ctx context.Context) {
	w := task.NewWorker(ctx, a.autoTasks)
	w.Logf = a.log.Printf
	w.Notify = func(info task.Info) {
		cmd := info.Command
		if len(cmd) > 200 {
			cmd = cmd[:200] + "..."
		}
		fmt.Fprintf(a.log.Writer(), "%s  The group host sent a task and is waiting for your approval:\n    %s\n  Approve or deny it on this computer's dashboard%s.\n",
			time.Now().Format("15:04:05"), strings.ReplaceAll(cmd, "\n", "\n    "), a.dashboardHint())
	}
	a.mu.Lock()
	a.tasks = w
	a.mu.Unlock()
}

// SetDashboardURL records where this worker's dashboard listens, for hints.
func (a *Agent) SetDashboardURL(u string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dashURL = u
}

func (a *Agent) dashboardHint() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.dashURL == "" {
		return ""
	}
	return " (" + a.dashURL + ")"
}

// serveTask runs one task stream from the host.
func (a *Agent) serveTask(w http.ResponseWriter, r *http.Request) {
	runner := a.TaskRunner()
	if runner == nil || a.tasksOff() {
		http.Error(w, "the owner of this computer has turned tasks off", http.StatusForbidden)
		return
	}
	conn, err := tunnel.AcceptStream(w, r)
	if err != nil {
		return
	}
	_ = runner.Serve(conn)
}
