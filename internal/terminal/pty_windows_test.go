//go:build windows

package terminal

import (
	"strings"
	"testing"
	"time"
)

// A real ConPTY shell: output arrives, input is executed, exit code is kept.
func TestWindowsShellRoundTrip(t *testing.T) {
	m := NewManager([]string{"MESHCOMPUTE_TEST_MARK=conpty-ok"}, t.TempDir())
	defer m.CloseAll()
	s, err := m.Open(100, 30)
	if err == ErrUnsupported {
		t.Skip("this Windows version has no pseudo-console")
	}
	if err != nil {
		t.Fatal(err)
	}
	// Works in both PowerShell and cmd.exe: only one of the two expands.
	if err := s.Write([]byte("echo mark-$env:MESHCOMPUTE_TEST_MARK%MESHCOMPUTE_TEST_MARK%\r")); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(30 * time.Second)
	var all []byte
	var off int64
	for !strings.Contains(string(all), "mark-conpty-ok") {
		data, next, wake, exited := s.Read(off)
		all, off = append(all, data...), next
		if exited {
			t.Fatalf("shell exited early: %q", all)
		}
		select {
		case <-wake:
		case <-deadline:
			t.Fatalf("no shell output: %q", all)
		}
	}
	if err := s.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	if err := s.Write([]byte("exit 5\r")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("shell did not exit")
	}
	if s.ExitCode() != 5 {
		t.Fatalf("exit code %d", s.ExitCode())
	}
}
