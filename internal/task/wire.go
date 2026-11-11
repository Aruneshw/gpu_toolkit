package task

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"meshcompute/internal/tunnel"
	"sync"
	"time"
)

// Frames on a task stream; the frame's stream ID is the message kind.
const (
	kindStart  = 1 // host -> worker: JSON start, always first
	kindInfo   = 2 // worker -> host: JSON Info after every state change
	kindOutput = 3 // worker -> host: 8-byte output offset, then bytes
	kindCancel = 4 // host -> worker: stop the task (or withdraw it)
	kindPing   = 5 // worker -> host: keeps idle relay/NAT mappings open
)

// start opens a task or re-attaches to one.
type start struct {
	Spec Spec `json:"spec"`
	// Offset is the output offset the host already has.
	Offset int64 `json:"offset"`
	// AttachOnly: never run the task, only follow it. A worker that no
	// longer knows the task reports it expired.
	AttachOnly bool `json:"attach_only"`
}

type framer struct {
	mu sync.Mutex
	rw io.ReadWriteCloser
}

func (f *framer) send(kind uint32, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return tunnel.WriteFrame(f.rw, tunnel.Frame{StreamID: kind, Payload: payload})
}

func (f *framer) sendJSON(kind uint32, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return f.send(kind, b)
}

// ApprovalTimeout is how long a task waits for the owner's decision.
const ApprovalTimeout = 10 * time.Minute

// Worker runs tasks on a worker.
type Worker struct {
	Book *Book
	// Auto reports whether tasks run without asking the owner.
	Auto func() bool
	// Notify is called when a task starts waiting for approval.
	Notify func(Info)
	// Logf receives audit lines.
	Logf func(format string, args ...any)

	ctx context.Context
}

// NewWorker returns a worker task runner. Tasks are killed when ctx ends.
func NewWorker(ctx context.Context, auto func() bool) *Worker {
	return &Worker{Book: NewBook(), Auto: auto, ctx: ctx}
}

func (w *Worker) logf(format string, args ...any) {
	if w.Logf != nil {
		w.Logf(format, args...)
	}
}

// Decide approves or denies a task that is waiting for approval.
func (w *Worker) Decide(id string, approve bool) error {
	r, ok := w.Book.Get(id)
	if !ok {
		return errors.New("unknown task")
	}
	if r.Info().State != StateApproval {
		return errors.New("that task is not waiting for approval")
	}
	select {
	case r.decide <- approve:
	default:
	}
	return nil
}

// Cancel stops a task: a waiting one is withdrawn, a running one is killed.
func (w *Worker) Cancel(id string) error {
	r, ok := w.Book.Get(id)
	if !ok {
		return errors.New("unknown task")
	}
	r.mu.Lock()
	r.cancelled = true
	cancel := r.cancel
	state := r.info.State
	r.mu.Unlock()
	switch {
	case Final(state):
	case state == StateApproval:
		r.Update(func(i *Info) { i.State, i.Detail = StateCancelled, "withdrawn before it was approved" })
		select {
		case r.decide <- false:
		default:
		}
	case cancel != nil:
		cancel()
	}
	return nil
}

// Serve handles one task stream opened by the group host. The caller has
// authenticated the host and checked that tasks are allowed.
func (w *Worker) Serve(conn io.ReadWriteCloser) error {
	defer conn.Close()
	first, err := tunnel.ReadFrame(conn)
	if err != nil {
		return err
	}
	var req start
	if first.StreamID != kindStart || json.Unmarshal(first.Payload, &req) != nil {
		return errors.New("task stream must start with a task")
	}
	f := &framer{rw: conn}
	if err := req.Spec.Validate(); err != nil {
		_ = f.sendJSON(kindInfo, Info{ID: req.Spec.ID, State: StateFailed, Detail: err.Error()})
		return err
	}
	rec, ok := w.Book.Get(req.Spec.ID)
	switch {
	case ok:
		// A retry or a re-attach after a dropped connection.
	case req.AttachOnly:
		now := time.Now().UTC()
		_ = f.sendJSON(kindInfo, Info{ID: req.Spec.ID, State: StateExpired, Ended: &now, Detail: "the worker restarted and no longer knows this task"})
		return nil
	default:
		info := Info{ID: req.Spec.ID, Command: req.Spec.Command, Shell: req.Spec.Shell, Dir: req.Spec.Dir, State: StateApproval}
		if w.Auto != nil && w.Auto() {
			info.State = StateRunning
		}
		if rec, err = w.Book.add(info); err != nil {
			_ = f.sendJSON(kindInfo, Info{ID: req.Spec.ID, State: StateFailed, Detail: err.Error()})
			return err
		}
		w.logf("event=task id=%s state=received auto=%t", req.Spec.ID, info.State == StateRunning)
		go w.execute(rec, req.Spec)
	}
	return follow(w.ctx, f, rec, req.Offset, func() { _ = w.Cancel(rec.Info().ID) })
}

// execute waits for approval if needed, then runs the task to completion,
// independent of any connection to the host.
func (w *Worker) execute(r *Record, spec Spec) {
	if r.Info().State == StateApproval {
		if w.Notify != nil {
			w.Notify(r.Info())
		}
		timer := time.NewTimer(ApprovalTimeout)
		defer timer.Stop()
		select {
		case ok := <-r.decide:
			if !ok {
				r.Update(func(i *Info) { i.State, i.Detail = StateDenied, "the owner of this computer refused it" })
				w.logf("event=task id=%s state=denied", spec.ID)
				return
			}
		case <-timer.C:
			r.Update(func(i *Info) { i.State, i.Detail = StateExpired, "nobody approved it within 10 minutes" })
			return
		case <-w.ctx.Done():
			r.Update(func(i *Info) { i.State, i.Detail = StateCancelled, "the worker stopped" })
			return
		}
	}
	ctx, cancel := context.WithCancel(w.ctx)
	defer cancel()
	r.mu.Lock()
	if r.cancelled || Final(r.info.State) {
		r.mu.Unlock()
		return
	}
	r.cancel = cancel
	r.mu.Unlock()
	now := time.Now().UTC()
	r.Update(func(i *Info) { i.State, i.Started, i.Detail = StateRunning, &now, "" })
	w.logf("event=task id=%s state=running", spec.ID)
	code, err := Run(ctx, spec, r)
	r.mu.Lock()
	cancelled := r.cancelled
	r.mu.Unlock()
	r.Update(func(i *Info) {
		switch {
		case cancelled:
			i.State, i.Detail = StateCancelled, "stopped on request"
		case w.ctx.Err() != nil:
			i.State, i.Detail = StateCancelled, "the worker stopped"
		case err != nil:
			i.State, i.Detail = StateFailed, err.Error()
		case code == 0:
			i.State = StateSucceeded
		default:
			i.State, i.Detail = StateFailed, fmt.Sprintf("exit code %d", code)
		}
		if err == nil {
			i.ExitCode = &code
		}
	})
	w.logf("event=task id=%s state=%s", spec.ID, r.Info().State)
}

// follow streams a record's output and state changes until it finishes or
// the stream breaks. A cancel frame from the host calls onCancel.
func follow(ctx context.Context, f *framer, r *Record, offset int64, onCancel func()) error {
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		for {
			in, err := tunnel.ReadFrame(f.rw)
			if err != nil {
				return
			}
			if in.StreamID == kindCancel {
				onCancel()
			}
		}
	}()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	var sent string
	for {
		data, next, info, wake := r.Read(offset)
		for start := next - int64(len(data)); len(data) > 0; {
			chunk := data
			if len(chunk) > tunnel.MaxPayload-8 {
				chunk = chunk[:tunnel.MaxPayload-8]
			}
			b := make([]byte, 8, 8+len(chunk))
			binary.BigEndian.PutUint64(b, uint64(start))
			if err := f.send(kindOutput, append(b, chunk...)); err != nil {
				return err
			}
			data, start = data[len(chunk):], start+int64(len(chunk))
		}
		offset = next
		key := fmt.Sprint(info.State, info.Detail, info.Started != nil, info.ExitCode != nil)
		if key != sent {
			if err := f.sendJSON(kindInfo, info); err != nil {
				return err
			}
			sent = key
		}
		if Final(info.State) {
			return nil
		}
		select {
		case <-wake:
		case <-gone:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-ping.C:
			if err := f.send(kindPing, nil); err != nil {
				return err
			}
		}
	}
}

// Dialer opens a task stream to a worker.
type Dialer func(ctx context.Context, node string) (io.ReadWriteCloser, error)

// Host sends tasks to workers and mirrors their state and output.
type Host struct {
	Book *Book
	dial Dialer
	ctx  context.Context

	mu        sync.Mutex
	conns     map[string]*framer
	cancelled map[string]bool
}

// LostAfter is how long the host keeps trying to reach a worker that is
// running one of its tasks.
const LostAfter = 30 * time.Minute

// NewHost returns a task sender; following stops when ctx ends.
func NewHost(ctx context.Context, dial Dialer) *Host {
	return &Host{Book: NewBook(), dial: dial, ctx: ctx, conns: map[string]*framer{}, cancelled: map[string]bool{}}
}

// Send starts one task per node with the same command.
func (h *Host) Send(nodes []string, spec Spec) ([]Info, error) {
	if len(nodes) == 0 {
		return nil, errors.New("choose at least one worker")
	}
	if len(nodes) > 64 {
		return nil, errors.New("at most 64 workers per send")
	}
	var out []Info
	for _, node := range nodes {
		s := spec
		s.ID = NewID()
		if err := s.Validate(); err != nil {
			return nil, err
		}
		rec, err := h.Book.add(Info{ID: s.ID, Node: node, Command: s.Command, Shell: s.Shell, Dir: s.Dir, State: StateSending})
		if err != nil {
			return out, err
		}
		go h.follow(rec, node, s)
		out = append(out, rec.Info())
	}
	return out, nil
}

// Cancel asks the worker to stop a task (or withdraws one not yet started).
func (h *Host) Cancel(id string) error {
	r, ok := h.Book.Get(id)
	if !ok {
		return errors.New("unknown task")
	}
	if Final(r.Info().State) {
		return nil
	}
	h.mu.Lock()
	h.cancelled[id] = true
	f := h.conns[id]
	h.mu.Unlock()
	if f != nil {
		return f.send(kindCancel, nil)
	}
	if r.Info().State == StateSending {
		r.Update(func(i *Info) { i.State, i.Detail = StateCancelled, "cancelled before it reached the worker" })
	}
	return nil
}

func (h *Host) wasCancelled(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cancelled[id]
}

// follow delivers a task and mirrors it until it ends, re-attaching after
// dropped connections.
func (h *Host) follow(r *Record, node string, spec Spec) {
	defer func() {
		h.mu.Lock()
		delete(h.cancelled, spec.ID)
		h.mu.Unlock()
	}()
	accepted := false
	lost := time.Now().Add(LostAfter)
	for attempt := 0; ; attempt++ {
		if Final(r.Info().State) {
			return
		}
		if h.ctx.Err() != nil {
			r.Update(func(i *Info) { i.State, i.Detail = StateExpired, "the host stopped following it" })
			return
		}
		if !accepted && h.wasCancelled(spec.ID) {
			r.Update(func(i *Info) { i.State, i.Detail = StateCancelled, "cancelled before it reached the worker" })
			return
		}
		dctx, cancel := context.WithTimeout(h.ctx, 15*time.Second)
		conn, err := h.dial(dctx, node)
		cancel()
		if err != nil {
			if !accepted && attempt >= 2 {
				r.Update(func(i *Info) { i.State, i.Detail = StateFailed, err.Error() })
				return
			}
			if time.Now().After(lost) {
				r.Update(func(i *Info) { i.State, i.Detail = StateExpired, "lost contact with the worker" })
				return
			}
			if accepted {
				r.Update(func(i *Info) { i.Detail = "connection lost; reconnecting (the task keeps running on the worker)" })
			}
			select {
			case <-time.After(3 * time.Second):
			case <-h.ctx.Done():
			}
			continue
		}
		f := &framer{rw: conn}
		h.mu.Lock()
		h.conns[spec.ID] = f
		cancelNow := h.cancelled[spec.ID]
		h.mu.Unlock()
		err = f.sendJSON(kindStart, start{Spec: spec, Offset: r.Info().Output, AttachOnly: accepted})
		if err == nil && cancelNow {
			err = f.send(kindCancel, nil)
		}
		if err == nil {
			accepted, err = h.mirror(conn, r, accepted)
		}
		h.mu.Lock()
		delete(h.conns, spec.ID)
		h.mu.Unlock()
		conn.Close()
		if Final(r.Info().State) {
			return
		}
		if accepted {
			lost = time.Now().Add(LostAfter)
			r.Update(func(i *Info) { i.Detail = "connection lost; reconnecting (the task keeps running on the worker)" })
		}
		select {
		case <-time.After(time.Second):
		case <-h.ctx.Done():
		}
	}
}

// mirror copies frames from the worker into r until the task ends or the
// stream breaks. It reports whether the worker has acknowledged the task.
func (h *Host) mirror(conn io.Reader, r *Record, accepted bool) (bool, error) {
	for {
		in, err := tunnel.ReadFrame(conn)
		if err != nil {
			return accepted, err
		}
		switch in.StreamID {
		case kindInfo:
			var info Info
			if json.Unmarshal(in.Payload, &info) != nil {
				return accepted, errors.New("bad task state from worker")
			}
			accepted = true
			r.Update(func(i *Info) {
				i.State, i.Detail, i.Started, i.Ended, i.ExitCode = info.State, info.Detail, info.Started, info.Ended, info.ExitCode
			})
			if Final(info.State) {
				return true, nil
			}
		case kindOutput:
			if len(in.Payload) < 8 {
				return accepted, errors.New("bad task output frame")
			}
			r.writeAt(int64(binary.BigEndian.Uint64(in.Payload)), in.Payload[8:])
		}
	}
}
