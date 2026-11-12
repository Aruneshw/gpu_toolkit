package agent_test

import (
	"context"
	"meshcompute/internal/agent"
	"meshcompute/internal/config"
	"meshcompute/internal/security"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A host that also listens on a second address (standing in for its mesh-VPN
// address) offers it to workers; a worker probes it, moves its connections
// there, and the host measures the link through the tunnel.
func TestMeshPathAndLinkMeasurement(t *testing.T) {
	alt, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skipf("no second loopback address here: %v", err)
	}
	altAddr := alt.Addr().String()
	alt.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hostDir := filepath.Join(t.TempDir(), "host")
	hl := listen(t)
	hc := config.Default()
	hc.Listen = hl.Addr().String()
	hc.Advertise = "https://" + hc.Listen
	hc.GroupHost = true
	hc.MeshPaths = []string{altAddr}
	if err := security.InitGroup(hostDir, hc.Listen, hc.Advertise, false); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(hostDir, hc); err != nil {
		t.Fatal(err)
	}
	startAgent(t, ctx, hostDir, hc, hl)
	w := joinWorker(t, ctx, hostDir, hc, "w1", nil)
	client, err := security.Client(hostDir)
	if err != nil {
		t.Fatal(err)
	}
	client.Timeout = 10 * time.Second
	var link agent.LinkStatus
	deadline := time.Now().Add(45 * time.Second)
	for {
		var st agent.Status
		getJSON(t, client, hc.LocalEndpoint()+"/v1/status", nil, &st)
		link = st.Links[w.id]
		if link.Measured == "tunnel" && link.RTTMillis != nil && link.Via == altAddr {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker never moved to the offered path with a measured link: %+v (want via %s)", link, altAddr)
		}
		time.Sleep(300 * time.Millisecond)
	}
	if *link.RTTMillis <= 0 || *link.RTTMillis > 250 {
		t.Fatalf("implausible loopback round trip: %+v", link)
	}
	if link.Path != "local" {
		t.Fatalf("a worker on this computer without a relay is %q, want local", link.Path)
	}
	if !strings.HasPrefix(link.Via, "127.0.0.2:") {
		t.Fatalf("via %q", link.Via)
	}
}
