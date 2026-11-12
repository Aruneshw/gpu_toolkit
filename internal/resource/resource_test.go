package resource

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParseMeminfo(t *testing.T) {
	total, free := parseMeminfo("MemTotal: 123 kB\nMemAvailable: 45 kB\nMemFree: 99 kB\n")
	if total == nil || *total != 123*1024 || free == nil || *free != 45*1024 {
		t.Fatalf("bad memory: %v %v", total, free)
	}
	for _, s := range []string{"", "MemAvailable: nope kB", "MemAvailable: 18446744073709551615 kB", "MemAvailable: 10 MB", "MemFree: 10 kB"} {
		_, free = parseMeminfo(s)
		if free != nil {
			t.Errorf("fabricated available memory for %q", s)
		}
	}
}
func TestParseGPUs(t *testing.T) {
	g, e := parseGPUs("0, NVIDIA Example, 8192, 4096, 12, 550.1\n1, Other, N/A, [Not Supported], N/A, 551\n")
	if e != nil || len(g) != 2 {
		t.Fatalf("%v %v", g, e)
	}
	if *g[0].TotalBytes != 8192*1024*1024 || *g[0].FreeBytes != 4096*1024*1024 || *g[0].UtilizationPercent != 12 || g[0].Driver != "550.1" {
		t.Fatal(g[0])
	}
	if g[1].TotalBytes != nil || g[1].FreeBytes != nil || g[1].UtilizationPercent != nil {
		t.Fatal("unknown fabricated")
	}
	for _, s := range []string{"0, wrong", "x,a,1,1,1,v", "-1,a,1,1,1,v"} {
		if _, e := parseGPUs(s); e == nil {
			t.Errorf("accepted %q", s)
		}
	}
	for _, s := range []string{"NaN", "101", "-1", "+Inf"} {
		g, e := parseGPUs("0,a,1,1," + s + ",v")
		if e != nil || g[0].UtilizationPercent != nil {
			t.Errorf("invalid utilization %s", s)
		}
	}
}
func TestUnknownJSON(t *testing.T) {
	b, e := json.Marshal(Snapshot{})
	if e != nil || !strings.Contains(string(b), `"ram_total_bytes":null`) {
		t.Fatalf("%s %v", b, e)
	}
}
func TestDiscoverCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s, e := Discover(ctx, ".")
	if e != context.Canceled || s.OS == "" || s.Arch == "" {
		t.Fatalf("%+v %v", s, e)
	}
}
func TestDisk(t *testing.T) {
	total, free, e := disk(context.Background(), t.TempDir())
	if e != nil {
		t.Skip(e)
	}
	if total == 0 || free > total {
		t.Fatalf("%d %d", total, free)
	}
	if _, _, e = disk(context.Background(), t.TempDir()+"/absent"); e == nil {
		t.Fatal("missing path accepted")
	}
}

func TestParseGPUTelemetry(t *testing.T) {
	g, e := parseGPUs("0, RTX, 6144, 5251, 7, 555.58, 39, 3.44, [N/A]\n1, Hot, 1, 1, 1, 1, 999, -5, 80.5\n")
	if e != nil || len(g) != 2 {
		t.Fatalf("%v %v", g, e)
	}
	if g[0].TemperatureC == nil || *g[0].TemperatureC != 39 || g[0].PowerWatts == nil || *g[0].PowerWatts != 3.44 || g[0].PowerLimitWatts != nil {
		t.Fatalf("%+v", g[0])
	}
	if g[1].TemperatureC != nil || g[1].PowerWatts != nil || g[1].PowerLimitWatts == nil || *g[1].PowerLimitWatts != 80.5 {
		t.Fatalf("out-of-range readings must be dropped: %+v", g[1])
	}
	if _, e := parseGPUs("0, a, 1, 1, 1, v, 1, 2"); e == nil {
		t.Fatal("accepted 8 columns")
	}
}

func TestCPULoad(t *testing.T) {
	cpuPrev.busy, cpuPrev.total = 0, 0
	if p := cpuLoad("cpu  100 0 100 800 0 0 0 0 0 0\ncpu0 1 1 1 1\n"); p != nil {
		t.Fatalf("first sample must be unknown, got %v", *p)
	}
	// 100 more busy jiffies and 300 more idle: 25%.
	p := cpuLoad("cpu  150 0 150 1100 0 0 0 0 0 0\n")
	if p == nil || *p != 25 {
		t.Fatalf("got %v", p)
	}
	if cpuLoad("intr 1 2 3") != nil || cpuLoad("cpu 1 x 3 4 5") != nil {
		t.Fatal("parsed garbage")
	}
}

// Basic is the instant stand-in while GPU discovery runs: it must not start
// any program (PowerShell takes seconds on Windows).
func TestBasicIsInstant(t *testing.T) {
	start := time.Now()
	s := Basic(context.Background(), t.TempDir())
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("Basic took %s", d)
	}
	if runtime.GOOS == "linux" || runtime.GOOS == "windows" {
		if s.RAMTotalBytes == nil || s.RAMAvailableBytes == nil || *s.RAMAvailableBytes > *s.RAMTotalBytes || *s.RAMTotalBytes == 0 {
			t.Fatalf("memory: %+v", s)
		}
		if s.DiskTotalBytes == nil || s.DiskFreeBytes == nil || *s.DiskFreeBytes > *s.DiskTotalBytes {
			t.Fatalf("disk: %+v", s)
		}
	}
}
