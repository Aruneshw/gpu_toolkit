package task

import (
	"context"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pair connects a Host to a Worker with in-memory pipes. breakAfter, when
// positive, cuts the first connection after that long.
func pair(t *testing.T, auto bool, breakAfter time.Duration) (*Host, *Worker) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var on atomic.Bool
	on.Store(auto)
	w := NewWorker(ctx, on.Load)
	var dials atomic.Int32
	h := NewHost(ctx, func(ctx context.Context, node string) (io.ReadWriteCloser, error) {
		a, b := net.Pipe()
		go w.Serve(b)
		if dials.Add(1) == 1 && breakAfter > 0 {
			time.AfterFunc(breakAfter, func() { a.Close() })
		}
		return a, nil
	})
	return h, w
}

func wait(t *testing.T, r *Record, states ...string) Info {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		_, _, info, wake := r.Read(0)
		for _, s := range states {
			if info.State == s {
				return info
			}
		}
		select {
		case <-wake:
		case <-deadline:
			t.Fatalf("task stayed %s (%s), want %v", info.State, info.Detail, states)
		}
	}
}

func output(r *Record) string {
	data, _, _, _ := r.Read(0)
	return string(data)
}

func send(t *testing.T, h *Host, cmd string) *Record {
	t.Helper()
	infos, err := h.Send([]string{"worker-a"}, Spec{Command: cmd, Shell: "sh"})
	if err != nil || len(infos) != 1 {
		t.Fatal(infos, err)
	}
	r, _ := h.Book.Get(infos[0].ID)
	return r
}

func TestAutoTaskRunsAndMirrorsOutput(t *testing.T) {
	h, w := pair(t, true, 0)
	r := send(t, h, `echo hello-$((40+2)); echo to-stderr >&2; echo id=$MESHCOMPUTE_TASK_ID`)
	info := wait(t, r, StateSucceeded, StateFailed)
	if info.State != StateSucceeded || info.ExitCode == nil || *info.ExitCode != 0 {
		t.Fatalf("%+v", info)
	}
	out := output(r)
	for _, want := range []string{"hello-42", "to-stderr", "id=" + info.ID} {
		if !strings.Contains(out, want) {
			t.Fatalf("output %q lacks %q", out, want)
		}
	}
	wr, ok := w.Book.Get(info.ID)
	if !ok || output(wr) != out {
		t.Fatalf("worker copy differs: %q", output(wr))
	}
}

func TestFailedExitCode(t *testing.T) {
	h, _ := pair(t, true, 0)
	info := wait(t, send(t, h, "exit 3"), StateFailed, StateSucceeded)
	if info.State != StateFailed || info.ExitCode == nil || *info.ExitCode != 3 {
		t.Fatalf("%+v", info)
	}
}

func TestApprovalAndDenial(t *testing.T) {
	h, w := pair(t, false, 0)
	var notified sync.WaitGroup
	notified.Add(2)
	w.Notify = func(Info) { notified.Done() }
	ok := send(t, h, "echo approved-run")
	no := send(t, h, "echo must-not-run")
	wait(t, ok, StateApproval)
	wait(t, no, StateApproval)
	notified.Wait()
	if err := w.Decide(ok.Info().ID, true); err != nil {
		t.Fatal(err)
	}
	if err := w.Decide(no.Info().ID, false); err != nil {
		t.Fatal(err)
	}
	if info := wait(t, ok, StateSucceeded); !strings.Contains(output(ok), "approved-run") {
		t.Fatalf("%+v %q", info, output(ok))
	}
	if info := wait(t, no, StateDenied, StateSucceeded); info.State != StateDenied || strings.Contains(output(no), "must-not-run") {
		t.Fatalf("%+v %q", info, output(no))
	}
	if err := w.Decide(ok.Info().ID, true); err == nil {
		t.Fatal("deciding a finished task must fail")
	}
}

func TestHostCancelKillsProcessTree(t *testing.T) {
	h, _ := pair(t, true, 0)
	r := send(t, h, `sh -c 'sleep 30; echo child-survived' & echo started; wait`)
	wait(t, r, StateRunning)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(output(r), "started") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	start := time.Now()
	if err := h.Cancel(r.Info().ID); err != nil {
		t.Fatal(err)
	}
	info := wait(t, r, StateCancelled, StateSucceeded, StateFailed)
	if info.State != StateCancelled || time.Since(start) > 8*time.Second {
		t.Fatalf("%+v after %s", info, time.Since(start))
	}
}

func TestCancelWhileWaitingForApproval(t *testing.T) {
	h, _ := pair(t, false, 0)
	r := send(t, h, "echo never")
	wait(t, r, StateApproval)
	if err := h.Cancel(r.Info().ID); err != nil {
		t.Fatal(err)
	}
	if info := wait(t, r, StateCancelled, StateSucceeded); info.State != StateCancelled {
		t.Fatalf("%+v", info)
	}
}

func TestReattachResumesOutputAfterDrop(t *testing.T) {
	h, w := pair(t, true, 400*time.Millisecond)
	r := send(t, h, `i=0; while [ $i -lt 12 ]; do echo line-$i; i=$((i+1)); sleep 0.1; done`)
	info := wait(t, r, StateSucceeded, StateFailed)
	if info.State != StateSucceeded {
		t.Fatalf("%+v", info)
	}
	wr, _ := w.Book.Get(info.ID)
	if got, want := output(r), output(wr); got != want {
		t.Fatalf("host output after reconnect\n%q\nworker output\n%q", got, want)
	}
	if strings.Count(output(r), "line-") != 12 {
		t.Fatalf("lines lost or duplicated: %q", output(r))
	}
}

func TestWorkerForgotTaskReportsExpired(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := NewWorker(ctx, func() bool { return true })
	restarted := NewWorker(ctx, func() bool { return true })
	var dials atomic.Int32
	h := NewHost(ctx, func(context.Context, string) (io.ReadWriteCloser, error) {
		a, b := net.Pipe()
		w := first
		if dials.Add(1) > 1 {
			w = restarted
		}
		go w.Serve(b)
		if w == first {
			time.AfterFunc(300*time.Millisecond, func() { a.Close() })
		}
		return a, nil
	})
	infos, err := h.Send([]string{"w"}, Spec{Command: "sleep 3", Shell: "sh"})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := h.Book.Get(infos[0].ID)
	if info := wait(t, r, StateExpired, StateSucceeded); info.State != StateExpired {
		t.Fatalf("%+v", info)
	}
	if _, ok := restarted.Book.Get(infos[0].ID); ok {
		t.Fatal("a re-attach must never start the task on another worker")
	}
}

func TestSpecValidation(t *testing.T) {
	good := Spec{ID: NewID(), Command: "echo"}
	if err := good.Validate(); err != nil || good.Shell != "auto" {
		t.Fatal(err, good.Shell)
	}
	for _, s := range []Spec{
		{ID: "x", Command: "echo"},
		{ID: NewID(), Command: "  "},
		{ID: NewID(), Command: strings.Repeat("a", MaxCommand+1)},
		{ID: NewID(), Command: "echo", Shell: "zsh"},
		{ID: NewID(), Command: "a\x00b"},
	} {
		if s.Validate() == nil {
			t.Fatalf("accepted %+v", s)
		}
	}
}
