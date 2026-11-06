package resource

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
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
