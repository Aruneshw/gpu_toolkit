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
	"path/filepath"
	"testing"
	"time"
)

func TestIndependentGroupsAndHostRestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type group struct {
		dir, worker, id string
		c               config.Config
		stop            context.CancelFunc
		done            chan error
		client          *http.Client
	}
	groups := make([]*group, 0, 2)
	for i := 0; i < 2; i++ {
		g := &group{dir: filepath.Join(t.TempDir(), "host"), worker: filepath.Join(t.TempDir(), "worker"), c: config.Default()}
		l := listen(t)
		g.c.Listen = l.Addr().String()
		g.c.Advertise = "https://" + g.c.Listen
		g.c.GroupHost = true
		if err := security.InitGroup(g.dir, g.c.Listen, g.c.Advertise, false); err != nil {
			t.Fatal(err)
		}
		runCtx, stop := context.WithCancel(ctx)
		g.stop = stop
		g.done = make(chan error, 1)
		go func() { g.done <- agent.New(g.dir, g.c, io.Discard).ServeListener(runCtx, l) }()
		code, err := security.Pair(g.dir, g.c.Advertise, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		var joinErr error
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			joinErr = security.JoinHost(ctx, g.worker, code, "127.0.0.1")
			if joinErr == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if joinErr != nil {
			t.Fatal(joinErr)
		}
		g.id, err = security.NodeID(g.worker)
		if err != nil {
			t.Fatal(err)
		}
		wl := listen(t)
		wc := config.Default()
		wc.Listen = wl.Addr().String()
		wc.Advertise = "https://" + wc.Listen
		startAgent(t, ctx, g.worker, wc, wl)
		g.client, err = security.Client(g.dir)
		if err != nil {
			t.Fatal(err)
		}
		defer g.client.CloseIdleConnections()
		groups = append(groups, g)
	}
	t.Cleanup(cancel)
	waitPing := func(g *group) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for {
			r, e := g.client.Get(g.c.Advertise + "/v1/workers/" + g.id + "/v1/ping")
			if e == nil {
				body, _ := io.ReadAll(r.Body)
				r.Body.Close()
				if r.StatusCode == 200 && string(body) == "pong\n" {
					return
				}
			}
			if time.Now().After(deadline) {
				t.Fatal("worker did not reconnect")
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	for _, g := range groups {
		waitPing(g)
	}
	// A group's client cannot ask its own relay to find another group's worker.
	r, err := groups[0].client.Get(groups[0].c.Advertise + "/v1/workers/" + groups[1].id + "/v1/ping")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode == 200 {
		t.Fatal("cross-group routing succeeded")
	}
	cfg, err := security.WorkerTLS(groups[0].worker)
	if err != nil {
		t.Fatal(err)
	}
	attackCtx, stop := context.WithTimeout(ctx, 2*time.Second)
	err = tunnel.ServeWorker(attackCtx, groups[1].c.Advertise, cfg, http.NotFoundHandler())
	stop()
	if err == nil {
		t.Fatal("foreign host accepted pinned worker connection")
	}
	g := groups[0]
	g.stop()
	select {
	case err := <-g.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("host did not stop")
	}
	l, err := net.Listen("tcp", g.c.Listen)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	g.stop = stop
	g.done = make(chan error, 1)
	go func() { g.done <- agent.New(g.dir, g.c, io.Discard).ServeListener(runCtx, l) }()
	waitPing(g)
	waitPing(groups[1])
	cancel()
	for _, g := range groups {
		select {
		case e := <-g.done:
			if e != nil {
				t.Fatal(e)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("host shutdown timed out")
		}
	}
}
