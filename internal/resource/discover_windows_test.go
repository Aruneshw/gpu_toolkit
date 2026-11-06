//go:build windows

package resource

import (
	"context"
	"strings"
	"testing"
)

// TestDiscoverWindows exercises the real PowerShell discovery path on a
// Windows runner. GPU discovery is optional; CPU/RAM/disk must be present.
func TestDiscoverWindows(t *testing.T) {
	s, err := Discover(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if s.OS != "windows" || s.Arch == "" || s.LogicalCPUs < 1 {
		t.Fatalf("unexpected identity: %+v", s)
	}
	if s.CPU == "" || s.CPU == "unknown" {
		t.Fatalf("CPU unavailable: %+v", s)
	}
	if s.RAMTotalBytes == nil || s.RAMAvailableBytes == nil {
		t.Fatalf("RAM unavailable: %+v issues=%v", s, s.Issues)
	}
	if s.DiskTotalBytes == nil || s.DiskFreeBytes == nil {
		t.Fatalf("disk unavailable: %+v issues=%v", s, s.Issues)
	}
	for _, issue := range s.Issues {
		if strings.HasPrefix(issue, "Windows hardware:") {
			t.Fatalf("PowerShell discovery failed: %s", issue)
		}
	}
}
