package agent

import (
	"meshcompute/internal/config"
	"net"
	"strings"
	"testing"
)

func TestParsePaths(t *testing.T) {
	got := parsePaths(" https://100.64.1.2:7443, http://100.64.1.3:7443, https://[fd7a:115c:a1e0::5]:7443,https://host.example:7443, https://0.0.0.0:1, https://100.64.1.4, https://100.64.1.5:7443/x, https://224.0.0.1:5, https://10.0.0.1:1,https://10.0.0.2:2,https://10.0.0.3:3")
	want := []string{"https://100.64.1.2:7443", "https://[fd7a:115c:a1e0::5]:7443", "https://10.0.0.1:1", "https://10.0.0.2:2", "https://10.0.0.3:3"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v want %v", got, want)
	}
	if n := len(parsePaths(strings.Repeat("https://10.0.0.9:9,", 20))); n != maxPaths {
		t.Fatalf("kept %d paths, want at most %d", n, maxPaths)
	}
	if len(parsePaths("")) != 0 {
		t.Fatal("empty header")
	}
}

// Two failed connections on a mesh path return the worker to the
// invitation's address, and the path is not offered again for a while.
func TestHostPathFallsBack(t *testing.T) {
	var notes []string
	h := newHostPaths(t.TempDir(), "https://203.0.113.5:4000", func(s string) { notes = append(notes, s) })
	h.offer("https://100.64.1.2:7443", "")
	h.chosen = "https://100.64.1.2:7443"
	h.result("https://100.64.1.2:7443", false)
	if h.chosen == "" {
		t.Fatal("left the path after one failure")
	}
	h.result("https://100.64.1.2:7443", true)
	h.result("https://100.64.1.2:7443", false)
	if h.chosen == "" {
		t.Fatal("a success must reset the failure count")
	}
	h.result("https://100.64.1.2:7443", false)
	if h.chosen != "" || len(notes) != 1 || !strings.Contains(notes[0], "203.0.113.5:4000") {
		t.Fatalf("chosen %q notes %v", h.chosen, notes)
	}
	if _, bad := h.bad["https://100.64.1.2:7443"]; !bad {
		t.Fatal("failed path not set aside")
	}
	ep, cfg, via := h.current(nil)
	if ep != "https://203.0.113.5:4000" || cfg != nil || via != "" {
		t.Fatalf("current %s", ep)
	}
	// Results for the invitation's own address never change the choice.
	h.result("https://203.0.113.5:4000", false)
	h.result("https://203.0.113.5:4000", false)
	if len(notes) != 1 {
		t.Fatal(notes)
	}
}

func TestClassifyPath(t *testing.T) {
	addr := func(s string) net.Addr { a, _ := net.ResolveTCPAddr("tcp", s); return a }
	for _, tc := range []struct {
		local, remote string
		relay         bool
		want          string
	}{
		{"127.0.0.1:7443", "127.0.0.1:5555", true, "relay"},
		{"127.0.0.1:7443", "127.0.0.1:5555", false, "local"},
		{"[fd7a:115c:a1e0::1]:7443", "[fd7a:115c:a1e0::2]:5555", false, "mesh"},
		{"192.168.1.5:7443", "192.168.1.9:5555", false, "direct"},
	} {
		if got := classifyPath(addr(tc.local), addr(tc.remote), tc.relay); got != tc.want {
			t.Errorf("%+v: %q", tc, got)
		}
	}
}

func TestLinkWarnings(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	links := map[string]LinkStatus{
		"worker-fast-1":   {Path: "mesh", RTTMillis: f(4)},
		"worker-fast-2":   {Path: "direct", RTTMillis: f(5)},
		"worker-slow":     {Path: "direct", RTTMillis: f(60)},
		"worker-relay":    {Path: "relay", RTTMillis: f(180)},
		"worker-waiting":  {Path: "mesh", RTTMillis: f(30), RPCRoundTripsPerS: 20, WaitShare: f(0.6)},
		"worker-unmeasrd": {Path: "relay"},
	}
	none := linkWarnings([]string{"worker-fast-1", "worker-fast-2", "worker-unmeasrd"}, links)
	if len(none) != 0 {
		t.Fatalf("no slow link here: %v", none)
	}
	got := strings.Join(linkWarnings([]string{"worker-fast-1", "worker-fast-2", "worker-slow"}, links), "\n")
	if !strings.Contains(got, "worker-slow") || !strings.Contains(got, "12× slower") {
		t.Fatalf("outlier: %s", got)
	}
	got = strings.Join(linkWarnings([]string{"worker-fast-1", "worker-relay"}, links), "\n")
	if !strings.Contains(got, "public relay (180 ms") || !strings.Contains(got, "mesh VPN") {
		t.Fatalf("relay: %s", got)
	}
	got = strings.Join(linkWarnings([]string{"worker-waiting"}, links), "\n")
	if !strings.Contains(got, "about 60% of generation time") || !strings.Contains(got, "20 round trips/s × 30.0 ms") {
		t.Fatalf("wait share: %s", got)
	}
}

// A non-local worker that answers each round trip with ~600 KB is sending
// logits: the warning says so and names the fix. On this computer it is not
// worth a warning.
func TestLinkWarningsLogits(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	links := map[string]LinkStatus{
		"worker-last":  {Path: "mesh", RTTMillis: f(5), RPCRoundTripsPerS: 14, RPCDownBytesPerSec: 14 * 593 * 1024},
		"worker-mid":   {Path: "mesh", RTTMillis: f(5), RPCRoundTripsPerS: 14, RPCDownBytesPerSec: 14 * 3 * 1024},
		"worker-local": {Path: "local", RTTMillis: f(0.5), RPCRoundTripsPerS: 14, RPCDownBytesPerSec: 14 * 593 * 1024},
	}
	got := strings.Join(linkWarnings([]string{"worker-mid", "worker-last", "worker-local"}, links), "\n")
	if !strings.Contains(got, "worker-last: sends the host about 593 KB per token") || !strings.Contains(got, "--output-layer host") {
		t.Fatalf("%s", got)
	}
	if strings.Contains(got, "worker-mid") || strings.Contains(got, "worker-local") {
		t.Fatalf("only the logits sender over a network: %s", got)
	}
}

func TestOutputDevice(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		noHost  bool
		local   []string
		workers int
		want    string
	}{
		{"", false, []string{"CUDA0"}, 2, "CUDA0"},  // auto with a host GPU: keep it on the host
		{"", false, nil, 2, ""},                     // auto, CPU-only host: llama.cpp's choice
		{"host", false, nil, 2, "CPU"},              // asked for: the host CPU
		{"host", true, []string{"CUDA0"}, 2, "CPU"}, // host GPU excluded from the pool
		{"worker", false, []string{"CUDA0"}, 2, ""},
		{"host", false, []string{"Vulkan0"}, 0, ""}, // no workers: nothing crosses a network
	} {
		c := config.PoolConfig{OutputLayer: tc.mode, NoHostGPU: tc.noHost}
		if got := outputDevice(c, tc.local, tc.workers); got != tc.want {
			t.Errorf("%+v: %q", tc, got)
		}
	}
}

func TestParseHops(t *testing.T) {
	id := "worker-" + strings.Repeat("ab", 32)
	got := parseHops("https://192.168.1.4:5000 " + id + ", https://192.168.1.5:5000 worker-short, https://host.example:1 " + id + ", https://10.1.1.1:2 " + strings.ToUpper(id) + ",junk")
	if len(got) != 1 || got[0].endpoint != "https://192.168.1.4:5000" || got[0].via != id {
		t.Fatalf("%+v", got)
	}
}
