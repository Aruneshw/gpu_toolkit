package agent

import (
	"encoding/json"
	"io"
	"log"
	"meshcompute/internal/config"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func probeAgent(allow bool) *Agent {
	return &Agent{c: config.Config{AllowHop: allow}, worker: true, log: log.New(io.Discard, "", 0)}
}

func postProbe(a *Agent, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	a.handlePeerProbe(w, httptest.NewRequest(http.MethodPost, probePath, strings.NewReader(body)))
	return w
}

// The latency-map endpoint answers only when the owner allowed hops, takes
// at most maxProbes IP-literal https targets, and runs at most once every
// probeEvery.
func TestPeerProbeIsBounded(t *testing.T) {
	if w := postProbe(probeAgent(false), `{"targets":[]}`); w.Code != http.StatusForbidden {
		t.Fatalf("without allow_hop: %d", w.Code)
	}
	for _, bad := range []string{
		`{"targets":["http://127.0.0.1:1"]}`,
		`{"targets":["https://example.com:443"]}`,
		`{"targets":["https://127.0.0.1"]}`,
		`{"targets":[` + strings.Repeat(`"https://127.0.0.1:1",`, maxProbes) + `"https://127.0.0.1:1"]}`,
		`{"targets":[],"extra":1}`,
	} {
		if w := postProbe(probeAgent(true), bad); w.Code != http.StatusBadRequest {
			t.Errorf("%s: HTTP %d", bad, w.Code)
		}
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	gone := closed.Addr().String()
	closed.Close()
	a := probeAgent(true)
	up, down := "https://"+l.Addr().String(), "https://"+gone
	w := postProbe(a, `{"targets":["`+up+`","`+down+`"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d %s", w.Code, w.Body)
	}
	var res probeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if v, ok := res.Results[up]; !ok || v == nil || *v < 0 || *v > 1000 {
		t.Fatalf("reachable target: %+v", res.Results)
	}
	if v, ok := res.Results[down]; !ok || v != nil {
		t.Fatalf("closed port must be unreachable: %+v", res.Results)
	}
	if w := postProbe(a, `{"targets":["`+up+`"]}`); w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("second run right away: HTTP %d", w.Code)
	}
}

// A worker's own forwarder never sends through another hop: it uses the
// best direct path, so connections pass through at most one worker.
func TestForwarderUsesDirectPath(t *testing.T) {
	h := newHostPaths(t.TempDir(), "https://203.0.113.5:4000", func(string) {})
	h.chosen, h.via, h.direct = "https://192.168.1.7:5000", "worker-"+strings.Repeat("a", 64), "https://100.64.0.9:7443"
	if got := h.directEndpoint(); got != "https://100.64.0.9:7443" {
		t.Fatalf("forwarding to %s", got)
	}
	h.direct = ""
	if got := h.directEndpoint(); got != "https://203.0.113.5:4000" {
		t.Fatalf("forwarding to %s", got)
	}
}

func TestHopClearlyFaster(t *testing.T) {
	ms := func(v float64) time.Duration { return time.Duration(v * float64(time.Millisecond)) }
	for _, tc := range []struct {
		hop, direct float64
		want        bool
	}{
		{2, 120, true},  // a nearby worker instead of a far relay
		{25, 60, true},  // half and 35 ms less
		{3, 9, false},   // three times faster, but only 6 ms: noise on a busy machine
		{40, 60, false}, // 20 ms less, but not half
		{0.4, 0.6, false},
	} {
		if got := hopClearlyFaster(ms(tc.hop), ms(tc.direct)); got != tc.want {
			t.Errorf("hop %v ms vs direct %v ms: %v", tc.hop, tc.direct, got)
		}
	}
}

func TestVirtualBridge(t *testing.T) {
	for name, want := range map[string]bool{"docker0": true, "br-1a2b3c": true, "virbr0": true, "vEthernet (WSL)": true, "vboxnet0": true,
		"wlan0": false, "eth0": false, "enp3s0": false, "Wi-Fi": false, "tailscale0": false} {
		if virtualBridge(name) != want {
			t.Errorf("%s: want %v", name, want)
		}
	}
}
