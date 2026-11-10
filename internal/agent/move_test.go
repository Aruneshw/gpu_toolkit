package agent_test

import (
	"context"
	"io"
	"meshcompute/internal/agent"
	"meshcompute/internal/config"
	"meshcompute/internal/security"
	"meshcompute/internal/tunnel"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A group host whose IP changed keeps its CA and re-issues its certificate.
// Old invitations and old worker pins stop working; a worker that pastes a new
// invitation replaces its identity (with a backup) and appears again.
func TestHostAddressChangeAndRejoin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hostDir := filepath.Join(t.TempDir(), "host")
	workerDir := filepath.Join(t.TempDir(), "worker")
	c := config.Default()
	l := listen(t)
	c.Listen, c.Advertise, c.GroupHost = l.Addr().String(), "https://"+l.Addr().String(), true
	if err := security.InitGroup(hostDir, c.Listen, c.Advertise, false); err != nil {
		t.Fatal(err)
	}
	hostCtx, stopHost := context.WithCancel(ctx)
	hostDone := make(chan error, 1)
	go func() { hostDone <- agent.New(hostDir, c, io.Discard).ServeListener(hostCtx, l) }()
	code, err := security.Pair(hostDir, c.Advertise, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := security.Pair(hostDir, c.Advertise, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := security.JoinHost(ctx, workerDir, code, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	oldID, _ := security.NodeID(workerDir)
	stopHost()
	if err := <-hostDone; err != nil {
		t.Fatal(err)
	}

	// The host moves to a different loopback address and port.
	l2, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skip("127.0.0.2 unavailable:", err)
	}
	t.Cleanup(func() { l2.Close() })
	c.Listen, c.Advertise = l2.Addr().String(), "https://"+l2.Addr().String()
	if err := security.RetargetGroup(hostDir, c.Listen, c.Advertise, false); err != nil {
		t.Fatal(err)
	}
	startAgent(t, ctx, hostDir, c, l2)

	// The old worker identity is pinned to the old certificate.
	cfg, err := security.WorkerTLS(workerDir)
	if err != nil {
		t.Fatal(err)
	}
	attempt, stop := context.WithTimeout(ctx, 3*time.Second)
	if err := tunnel.ServeWorker(attempt, c.Advertise, cfg, http.NotFoundHandler()); err == nil {
		t.Fatal("old worker pin accepted after the host certificate changed")
	}
	stop()
	// Invitations issued before the move are discarded.
	if _, err := security.RejoinHost(ctx, workerDir, stale, "127.0.0.1"); err == nil {
		t.Fatal("stale invitation accepted after move")
	}
	if id, _ := security.NodeID(workerDir); id != oldID {
		t.Fatal("failed rejoin replaced the existing identity")
	}

	fresh, err := security.Pair(hostDir, c.Advertise, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := security.RejoinHost(ctx, hostDir, fresh, "127.0.0.1"); err == nil {
		t.Fatal("rejoin replaced the host's own identity")
	}
	backup, err := security.RejoinHost(ctx, workerDir, fresh, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(backup); err != nil || fi.Size() == 0 {
		t.Fatalf("no backup of the previous identity: %v", err)
	}
	newID, _ := security.NodeID(workerDir)
	if newID == oldID {
		t.Fatal("worker identity not replaced")
	}
	wl := listen(t)
	wc := config.Default()
	wc.Listen, wc.Advertise = wl.Addr().String(), "https://"+wl.Addr().String()
	startAgent(t, ctx, workerDir, wc, wl)
	client, err := security.Client(hostDir)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(20 * time.Second)
	for {
		r, e := client.Get(c.Advertise + "/v1/workers/" + newID + "/v1/ping")
		if e == nil {
			body, _ := io.ReadAll(r.Body)
			r.Body.Close()
			if r.StatusCode == 200 && string(body) == "pong\n" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("re-joined worker did not connect to the moved host")
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
}

// A group worker keeps working when its optional local port is taken, e.g. by
// a group host on the same PC or another program.
func TestWorkerConnectsWhenLocalPortBusy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hostDir := filepath.Join(t.TempDir(), "host")
	workerDir := filepath.Join(t.TempDir(), "worker")
	c := config.Default()
	l := listen(t)
	c.Listen, c.Advertise, c.GroupHost = l.Addr().String(), "https://"+l.Addr().String(), true
	if err := security.InitGroup(hostDir, c.Listen, c.Advertise, false); err != nil {
		t.Fatal(err)
	}
	startAgent(t, ctx, hostDir, c, l)
	code, err := security.Pair(hostDir, c.Advertise, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := security.JoinHost(ctx, workerDir, code, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	id, _ := security.NodeID(workerDir)
	busy := listen(t) // occupied for the whole test
	wc := config.Default()
	wc.Listen, wc.Advertise = busy.Addr().String(), "https://"+busy.Addr().String()
	done := make(chan error, 1)
	go func() { done <- agent.New(workerDir, wc, io.Discard).Serve(ctx) }()
	client, err := security.Client(hostDir)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(20 * time.Second)
	for {
		r, e := client.Get(c.Advertise + "/v1/workers/" + id + "/v1/ping")
		if e == nil {
			body, _ := io.ReadAll(r.Body)
			r.Body.Close()
			if r.StatusCode == 200 && string(body) == "pong\n" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("worker with a busy local port did not connect")
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop")
	}
}
