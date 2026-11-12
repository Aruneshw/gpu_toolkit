package agent_test

import (
	"context"
	"io"
	"meshcompute/internal/agent"
	"meshcompute/internal/config"
	"meshcompute/internal/security"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// slowProxy forwards TCP to target, holding every chunk for delay in each
// direction: a stand-in for a far-away relay.
func slowProxy(t *testing.T, target string, delay time.Duration) string {
	t.Helper()
	l := listen(t)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				up, err := net.Dial("tcp", target)
				if err != nil {
					c.Close()
					return
				}
				var once sync.Once
				done := func() { once.Do(func() { c.Close(); up.Close() }) }
				copySlow := func(dst, src net.Conn) {
					defer done()
					buf := make([]byte, 32<<10)
					for {
						n, err := src.Read(buf)
						if n > 0 {
							time.Sleep(delay)
							if _, werr := dst.Write(buf[:n]); werr != nil {
								return
							}
						}
						if err != nil {
							return
						}
					}
				}
				go copySlow(up, c)
				copySlow(c, up)
			}()
		}
	}()
	return l.Addr().String()
}

func loopbackFree(t *testing.T, ip string) string {
	t.Helper()
	l, err := net.Listen("tcp", ip+":0")
	if err != nil {
		t.Skipf("no loopback address %s here: %v", ip, err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// Worker A's only own path to the host is slow (a delaying proxy, like a far
// relay). Worker B also has a fast path (the host's second address) and lets
// other workers hop through it. A moves to the hop, the host labels A's link
// "hop via B" and measures both workers towards each other; a worker cannot
// read the map; when B leaves, A falls back to its own path.
func TestHopThroughNearbyWorker(t *testing.T) {
	mesh := loopbackFree(t, "127.0.0.2")
	hopB := loopbackFree(t, "127.0.0.3")
	hopA := loopbackFree(t, "127.0.0.4")
	defer agent.SetTopologyInterval(time.Second)()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hostDir := filepath.Join(t.TempDir(), "host")
	hl := listen(t)
	far := slowProxy(t, hl.Addr().String(), 15*time.Millisecond)
	hc := config.Default()
	hc.Listen = hl.Addr().String()
	hc.Advertise = "https://" + far // the invitation points at the slow path
	hc.GroupHost = true
	hc.MeshPaths = []string{mesh}
	if err := security.InitGroup(hostDir, hc.Listen, hc.Advertise, false); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(hostDir, hc); err != nil {
		t.Fatal(err)
	}
	startAgent(t, ctx, hostDir, hc, hl)

	// A cannot use the host's second address: only its slow path and B.
	var aDir atomic.Value
	aDir.Store("")
	defer agent.SetPathFilter(func(dir, endpoint string) bool {
		return dir != aDir.Load().(string) || !strings.Contains(endpoint, "127.0.0.2:")
	})()
	bctx, stopB := context.WithCancel(ctx)
	defer stopB()
	b := joinWorker(t, bctx, hostDir, hc, "b", func(c *config.Config) { c.AllowHop, c.HopListen = true, []string{hopB} })
	a := joinWorker(t, ctx, hostDir, hc, "a", func(c *config.Config) { c.AllowHop, c.HopListen = true, []string{hopA} })
	aDir.Store(a.dir)

	client, err := security.Client(hostDir)
	if err != nil {
		t.Fatal(err)
	}
	client.Timeout = 10 * time.Second
	local := hc.LocalEndpoint()
	status := func() agent.Status {
		var st agent.Status
		getJSON(t, client, local+"/v1/status", nil, &st)
		return st
	}
	waitFor := func(what string, limit time.Duration, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(limit)
		for !ok() {
			if time.Now().After(deadline) {
				st := status()
				t.Fatalf("timed out waiting for %s; links %+v", what, st.Links)
			}
			time.Sleep(300 * time.Millisecond)
		}
	}
	waitFor("A to reach the host through B", 90*time.Second, func() bool {
		l := status().Links[a.id]
		return l.Path == "hop" && l.HopVia == b.id && l.Measured == "tunnel"
	})
	if l := status().Links[b.id]; l.Path == "hop" {
		t.Fatalf("B, which has a fast path of its own, hops: %+v", l)
	}

	var topo agent.Topology
	waitFor("both workers measured towards each other", 60*time.Second, func() bool {
		topo = agent.Topology{}
		getJSON(t, client, local+"/v1/topology", nil, &topo)
		got := 0
		for _, p := range topo.Pairs {
			if p.Millis != nil && (p.From == a.id && p.To == b.id || p.From == b.id && p.To == a.id) {
				got++
			}
		}
		return got == 2
	})
	nodes := map[string]agent.TopoNode{}
	for _, n := range topo.Nodes {
		nodes[n.ID] = n
	}
	if nodes[b.id].Hop == nil || nodes[b.id].Hop.Served == 0 || !strings.HasPrefix(nodes[b.id].Hop.Endpoints[0], "https://127.0.0.3:") {
		t.Fatalf("B's forwarder: %+v", nodes[b.id].Hop)
	}
	if nodes[a.id].Path != "hop" || nodes[a.id].HopVia != b.id {
		t.Fatalf("A on the map: %+v", nodes[a.id])
	}

	// The map holds workers' LAN addresses: only the host itself may read it.
	wc, err := security.Client(a.dir)
	if err != nil {
		t.Fatal(err)
	}
	wc.Timeout = 10 * time.Second
	res, err := wc.Get("https://" + hl.Addr().String() + "/v1/topology")
	if err != nil {
		t.Fatalf("worker request to the host failed before authorization: %v", err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("a worker read the map: HTTP %d %s", res.StatusCode, body)
	}

	// B leaves: A goes back to its own path.
	stopB()
	waitFor("A to fall back when B is gone", 90*time.Second, func() bool {
		l := status().Links[a.id]
		return l.Path != "hop" && l.HopVia == "" && l.Measured == "tunnel"
	})
}
