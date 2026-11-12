package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A slow nvidia-smi must not make status requests time out: the first
// waits at most firstWait, later ones answer at once, and the full
// snapshot replaces the stand-in when discovery ends.
func TestSlowGPUDiscoveryDoesNotBlockStatus(t *testing.T) {
	// The test binary itself plays nvidia-smi (see TestMain).
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	name := "nvidia-smi"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, name), data, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MESHCOMPUTE_TEST_FAKE_NVIDIA", "1")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	defer func(d time.Duration) { firstWait = d }(firstWait)
	firstWait = 300 * time.Millisecond
	var c resourceCache
	start := time.Now()
	s, err := c.get(context.Background(), t.TempDir())
	if err != nil || time.Since(start) > 2*time.Second || len(s.GPUs) != 0 || len(s.Issues) == 0 || !strings.Contains(s.Issues[0], "still running") {
		t.Fatalf("first request: %v after %s, %+v", err, time.Since(start), s)
	}
	start = time.Now()
	if s, _ = c.get(context.Background(), t.TempDir()); time.Since(start) > 200*time.Millisecond || len(s.GPUs) != 0 {
		t.Fatalf("second request waited %s", time.Since(start))
	}
	// Full discovery on Windows also runs PowerShell, which takes many
	// seconds on a busy CI machine; discovery itself is bounded at 45 s.
	deadline := time.Now().Add(45 * time.Second)
	for {
		s, _ = c.get(context.Background(), t.TempDir())
		if len(s.GPUs) == 1 && s.GPUs[0].Name == "Slow GPU" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("full discovery never replaced the stand-in: %+v", s)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
