//go:build linux

package terminal

import (
	"net"
	"strings"
	"testing"
	"time"
)

func waitFor(t *testing.T, s *Session, want string) string {
	t.Helper()
	deadline := time.After(10 * time.Second)
	var all []byte
	var off int64
	for {
		data, next, wake, exited := s.Read(off)
		all = append(all, data...)
		off = next
		if strings.Contains(string(all), want) {
			return string(all)
		}
		if exited {
			t.Fatalf("shell exited before %q; output: %q", want, all)
		}
		select {
		case <-wake:
		case <-deadline:
			t.Fatalf("timed out waiting for %q; output: %q", want, all)
		}
	}
}

func TestShellRoundTrip(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	m := NewManager([]string{"MESHCOMPUTE_TEST_MARK=mesh-ok"}, t.TempDir())
	defer m.CloseAll()
	s, err := m.Open(100, 30)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write([]byte("echo $MESHCOMPUTE_TEST_MARK; stty size; tty\n")); err != nil {
		t.Fatal(err)
	}
	out := waitFor(t, s, "/dev/pts/")
	if !strings.Contains(out, "mesh-ok") || !strings.Contains(out, "30 100") {
		t.Fatalf("environment or size missing: %q", out)
	}
	if err := s.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	if err := s.Write([]byte("stty size\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, s, "40 120")
	if err := s.Write([]byte("exit 3\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("shell did not exit")
	}
	if s.ExitCode() != 3 {
		t.Fatalf("exit code %d", s.ExitCode())
	}
	time.Sleep(50 * time.Millisecond)
	if _, ok := m.Get(s.ID); ok {
		t.Fatal("exited session still registered")
	}
}

func TestCloseKillsShellAndLimits(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	m := NewManager(nil, t.TempDir())
	var sessions []*Session
	for i := 0; i < MaxSessions; i++ {
		s, err := m.Open(80, 24)
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, s)
	}
	if _, err := m.Open(80, 24); err == nil {
		t.Fatal("exceeded session limit")
	}
	_ = sessions[0].Write([]byte("sleep 1000\n"))
	m.Close(sessions[0].ID)
	select {
	case <-sessions[0].Done():
	case <-time.After(5 * time.Second):
		t.Fatal("close did not stop the shell")
	}
	m.CloseAll()
	for _, s := range sessions {
		select {
		case <-s.Done():
		default:
			t.Fatal("CloseAll left a shell running")
		}
	}
	if _, err := m.Open(80, 24); err == nil {
		t.Fatal("opened after CloseAll")
	}
}

func TestScrollbackOffsets(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	m := NewManager(nil, t.TempDir())
	defer m.CloseAll()
	s, err := m.Open(80, 24)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Write([]byte("echo first-marker\n"))
	waitFor(t, s, "first-marker")
	_, end, _, _ := s.Read(0)
	_ = s.Write([]byte("echo second-marker\n"))
	waitFor(t, s, "second-marker")
	later, _, _, _ := s.Read(end)
	if strings.Contains(string(later), "first-marker\r\n") && !strings.Contains(string(later), "second") {
		t.Fatalf("offset replayed old output: %q", later)
	}
	all, _, _, _ := s.Read(-5)
	if !strings.Contains(string(all), "first-marker") {
		t.Fatal("invalid offset did not replay scrollback")
	}
}

// A shell on a "worker" manager driven from a "host" manager over one stream:
// output, input, resize, exit code, and killing the shell when the host hangs up.
func TestRemoteShellOverStream(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	worker := NewManager([]string{"MESHCOMPUTE_TEST_MARK=remote-ok"}, t.TempDir())
	defer worker.CloseAll()
	host := NewManager(nil, "")
	defer host.CloseAll()
	a, b := net.Pipe()
	served := make(chan error, 1)
	go func() { served <- worker.Serve(b) }()
	s, err := host.Attach(a, 90, 25)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write([]byte("echo $MESHCOMPUTE_TEST_MARK; stty size\n")); err != nil {
		t.Fatal(err)
	}
	if out := waitFor(t, s, "25 90"); !strings.Contains(out, "remote-ok") {
		t.Fatalf("worker environment missing: %q", out)
	}
	if err := s.Resize(110, 33); err != nil {
		t.Fatal(err)
	}
	// A large paste crosses several frames.
	big := strings.Repeat(strings.Repeat("x", 49)+"\n", 3000)
	if err := s.Write([]byte("stty size; stty -echo; wc -c <<'END'\n" + big + "END\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, s, "33 110")
	waitFor(t, s, "150000")
	if err := s.Write([]byte("exit 7\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("remote shell did not exit")
	}
	if s.ExitCode() != 7 {
		t.Fatalf("exit code %d", s.ExitCode())
	}
	if err := <-served; err != nil {
		t.Fatalf("serve: %v", err)
	}

	// Hanging up from the host kills the worker's shell.
	a, b = net.Pipe()
	go func() { served <- worker.Serve(b) }()
	s, err = host.Attach(a, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write([]byte("echo started\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, s, "started")
	host.Close(s.ID)
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("worker kept serving after the host hung up")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		worker.mu.Lock()
		n := len(worker.sessions)
		worker.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d worker shells survived the hangup", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
