// Package task runs commands that a group host sends to its workers.
//
// The host chooses the command, the shell and the working directory; the
// worker's owner decides whether it runs (see Policy). A task keeps running
// when the connection to the host drops: the host re-attaches and resumes
// its output where it left off. Both sides keep the same Record shape, so
// one dashboard view serves the host and the worker.
package task

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

// States. The last five are final.
const (
	StateSending   = "sending"   // host: connecting to the worker
	StateApproval  = "approval"  // worker: waiting for the owner's decision
	StateRunning   = "running"   // the command is running
	StateSucceeded = "succeeded" // exit code 0
	StateFailed    = "failed"    // non-zero exit, could not start, or unreachable
	StateDenied    = "denied"    // the owner refused it
	StateCancelled = "cancelled" // stopped by the host or the owner
	StateExpired   = "expired"   // nobody approved it in time, or the worker lost it
)

// Final reports whether a task in state s can change again.
func Final(s string) bool {
	switch s {
	case StateSucceeded, StateFailed, StateDenied, StateCancelled, StateExpired:
		return true
	}
	return false
}

// Shells a task may name. "auto" is bash (or sh) on Linux and PowerShell on
// Windows.
var Shells = []string{"auto", "bash", "sh", "powershell", "pwsh", "cmd"}

const (
	// MaxCommand bounds a command's size.
	MaxCommand = 16 << 10
	// scrollback is the output each side keeps per task.
	scrollback = 512 << 10
	// maxRecords bounds the tasks remembered per side; finished ones go first.
	maxRecords = 200
)

// Spec is what the host asks a worker to run.
type Spec struct {
	ID      string `json:"id"`
	Command string `json:"command"`
	Shell   string `json:"shell,omitempty"`
	Dir     string `json:"dir,omitempty"`
}

// Validate checks a spec received from the network.
func (s *Spec) Validate() error {
	if len(s.ID) != 32 {
		return errors.New("invalid task id")
	}
	for _, c := range s.ID {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return errors.New("invalid task id")
		}
	}
	if strings.TrimSpace(s.Command) == "" || len(s.Command) > MaxCommand || strings.ContainsRune(s.Command, 0) {
		return errors.New("the command must be 1 to 16384 bytes of text")
	}
	if s.Shell == "" {
		s.Shell = "auto"
	}
	ok := false
	for _, sh := range Shells {
		ok = ok || s.Shell == sh
	}
	if !ok {
		return errors.New("unknown shell; use auto, bash, sh, powershell, pwsh or cmd")
	}
	if len(s.Dir) > 4096 || strings.ContainsRune(s.Dir, 0) {
		return errors.New("invalid working directory")
	}
	return nil
}

// NewID returns a random task identifier.
func NewID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Info is a task's state without its output.
type Info struct {
	ID       string     `json:"id"`
	Node     string     `json:"node,omitempty"`
	Command  string     `json:"command"`
	Shell    string     `json:"shell"`
	Dir      string     `json:"dir,omitempty"`
	State    string     `json:"state"`
	Detail   string     `json:"detail,omitempty"`
	Created  time.Time  `json:"created"`
	Started  *time.Time `json:"started,omitempty"`
	Ended    *time.Time `json:"ended,omitempty"`
	ExitCode *int       `json:"exit_code,omitempty"`
	// Output is the number of output bytes produced so far.
	Output int64 `json:"output"`
}

// Record is one task with its recent output. Readers wait on Read's wake
// channel, which closes on new output or a state change.
type Record struct {
	mu   sync.Mutex
	info Info
	buf  []byte // last <= scrollback bytes of output
	wake chan struct{}

	// Worker side.
	decide chan bool
	cancel func()
	// cancelled records a cancellation that arrived before the task started.
	cancelled bool
}

func newRecord(info Info) *Record {
	return &Record{info: info, wake: make(chan struct{}), decide: make(chan bool, 1)}
}

func (r *Record) signal() {
	close(r.wake)
	r.wake = make(chan struct{})
}

// Info returns a copy of the task's state.
func (r *Record) Info() Info {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.info
}

// Update changes the state under the record's lock and wakes readers.
func (r *Record) Update(change func(*Info)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if Final(r.info.State) {
		return
	}
	change(&r.info)
	if Final(r.info.State) && r.info.Ended == nil {
		now := time.Now().UTC()
		r.info.Ended = &now
	}
	r.signal()
}

// Write appends output.
func (r *Record) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if over := len(r.buf) - scrollback; over > 0 {
		r.buf = append(r.buf[:0], r.buf[over:]...)
	}
	r.info.Output += int64(len(p))
	r.signal()
	return len(p), nil
}

// writeAt appends the part of p that starts at or after the current end.
// Output older than the kept scrollback is reported as skipped.
func (r *Record) writeAt(offset int64, p []byte) {
	r.mu.Lock()
	end := r.info.Output
	r.mu.Unlock()
	if offset > end {
		_, _ = r.Write([]byte("\r\n[... output skipped while reconnecting ...]\r\n"))
		r.mu.Lock()
		r.info.Output = offset
		r.mu.Unlock()
		end = offset
	}
	if skip := end - offset; skip < int64(len(p)) {
		_, _ = r.Write(p[skip:])
	}
}

// Read returns output from offset on (or the oldest kept byte), the offset
// after it, the task's state, and a channel closed on the next change.
func (r *Record) Read(offset int64) (data []byte, next int64, info Info, wake <-chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	end := r.info.Output
	start := end - int64(len(r.buf))
	if offset < start || offset < 0 {
		offset = start
	}
	if offset > end {
		offset = end
	}
	data = append([]byte(nil), r.buf[offset-start:]...)
	return data, end, r.info, r.wake
}

// Book keeps the tasks of one side.
type Book struct {
	mu    sync.Mutex
	tasks map[string]*Record
	order []string
}

func NewBook() *Book { return &Book{tasks: map[string]*Record{}} }

// add stores a new record, forgetting the oldest finished one when full.
func (b *Book) add(info Info) (*Record, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, dup := b.tasks[info.ID]; dup {
		return nil, errors.New("duplicate task id")
	}
	if len(b.order) >= maxRecords {
		dropped := false
		for i, id := range b.order {
			if Final(b.tasks[id].Info().State) {
				delete(b.tasks, id)
				b.order = append(b.order[:i:i], b.order[i+1:]...)
				dropped = true
				break
			}
		}
		if !dropped {
			return nil, errors.New("too many unfinished tasks")
		}
	}
	if info.Created.IsZero() {
		info.Created = time.Now().UTC()
	}
	r := newRecord(info)
	b.tasks[info.ID] = r
	b.order = append(b.order, info.ID)
	return r, nil
}

// Get returns a task by ID.
func (b *Book) Get(id string) (*Record, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.tasks[id]
	return r, ok
}

// List returns every task, newest first.
func (b *Book) List() []Info {
	b.mu.Lock()
	recs := make([]*Record, 0, len(b.order))
	for _, id := range b.order {
		recs = append(recs, b.tasks[id])
	}
	b.mu.Unlock()
	out := make([]Info, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Info())
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}
