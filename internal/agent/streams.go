package agent

import (
	"context"
	"fmt"
	"meshcompute/internal/backend"
	"meshcompute/internal/terminal"
	"meshcompute/internal/tunnel"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Raw streams a group host may open on a worker through the worker's own
// outbound, mutually authenticated connection.
const (
	// StreamTerminal is a shell; served only if the worker's owner set
	// allow_terminal.
	StreamTerminal = "/v1/stream/term"
	// StreamRPC is a pipe to the worker's loopback llama.cpp rpc-server.
	StreamRPC = "/v1/stream/rpc"
)

// workerConnections is how many outbound connections a worker keeps ready, so
// status, inference and several streams can run at the same time.
const workerConnections = 4

// maxStreams bounds concurrent streams served by one worker.
const maxStreams = 24

func terminalSupported() bool { return runtime.GOOS == "linux" || runtime.GOOS == "windows" }

// startSharing launches the llama.cpp RPC server that lends this worker's GPU
// to the host's pooled model. It listens on loopback only.
func (a *Agent) startSharing(ctx context.Context) {
	out := a.log.Writer()
	if !a.Settings().ShareGPU {
		fmt.Fprintln(out, "GPU pooling is off on this computer (share_gpu=off); it still reports telemetry.")
		return
	}
	binary, err := backend.FindTool(a.c.RPCServer, backend.RPCServerNames, backend.ToolDirs(a.dir))
	if err != nil {
		fmt.Fprintln(out, "GPU pooling: llama.cpp rpc-server was not found, so this computer only reports telemetry.")
		fmt.Fprintln(out, "  Install it (Windows: win.bat option 5; Linux: ./linux.sh gpu) and reconnect to lend this GPU to the group.")
		return
	}
	rpc := &backend.RPCServer{Binary: binary, Log: backend.NewLogTail(50)}
	a.mu.Lock()
	a.rpc = rpc
	a.mu.Unlock()
	go rpc.Run(ctx)
	fmt.Fprintf(out, "GPU pooling: sharing this computer's GPU with the group host (%s on loopback, reachable only through the encrypted group connection).\n", binary)
}

func (a *Agent) rpcServer() *backend.RPCServer {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.rpc
}

func (a *Agent) terminals() *terminal.Manager {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.term == nil {
		home, _ := os.UserHomeDir()
		a.term = terminal.NewManager(nil, home)
	}
	return a.term
}

// connectWorker keeps this worker's outbound connections to the group host
// open until ctx ends.
func (a *Agent) connectWorker(ctx context.Context, endpoint string) {
	a.mu.Lock()
	a.worker = true
	a.mu.Unlock()
	a.startSharing(ctx)
	a.startTasks(ctx)
	a.setLink(endpoint, false)
	switch a.Settings().TaskApproval {
	case "auto":
		fmt.Fprintln(a.log.Writer(), "Tasks: AUTO. Commands the group host sends run here without asking. Change with: meshcompute config set task_approval ask")
	case "ask":
		fmt.Fprintln(a.log.Writer(), "Tasks: ASK. Commands the group host sends wait for your approval on this computer's dashboard.")
	}
	if a.allowTerminal() && terminalSupported() {
		fmt.Fprintln(a.log.Writer(), "Remote terminal: ON. The group host can open a shell on this computer as your user. Turn it off with: meshcompute config set allow_terminal false")
	}
	a.hostPaths = newHostPaths(a.dir, endpoint, func(msg string) {
		a.log.Printf("event=host_path note=%q", msg)
		fmt.Fprintf(a.log.Writer(), "%s  %s\n", time.Now().Format("15:04:05"), msg)
	})
	go a.hostPaths.run(ctx)
	var loops sync.WaitGroup
	for i := 0; i < workerConnections; i++ {
		loops.Add(1)
		go func(primary bool) { defer loops.Done(); a.connectLoop(ctx, endpoint, primary) }(i == 0)
	}
	loops.Wait()
	a.mu.Lock()
	term := a.term
	a.mu.Unlock()
	if term != nil {
		term.CloseAll()
	}
}

// serveStream handles a stream the pinned group host opened on this worker.
func (a *Agent) serveStream(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	select {
	case a.streams <- struct{}{}:
		defer func() { <-a.streams }()
	default:
		http.Error(w, "too many streams", http.StatusServiceUnavailable)
		return
	}
	switch r.URL.Path {
	case StreamTask:
		a.serveTask(w, r)
	case StreamTerminal:
		if !a.allowTerminal() || !terminalSupported() {
			http.Error(w, "the owner of this computer has not allowed remote terminals", http.StatusForbidden)
			return
		}
		manager := a.terminals()
		conn, err := tunnel.AcceptStream(w, r)
		if err != nil {
			return
		}
		stop := context.AfterFunc(ctx, func() { conn.Close() })
		defer stop()
		a.log.Print("event=remote_terminal state=opened")
		fmt.Fprintf(a.log.Writer(), "%s  The group host opened a terminal on this computer.\n", time.Now().Format("15:04:05"))
		a.mu.Lock()
		a.termOpen++
		a.mu.Unlock()
		_ = manager.Serve(conn)
		a.mu.Lock()
		a.termOpen--
		a.mu.Unlock()
		a.log.Print("event=remote_terminal state=closed")
	case StreamEcho:
		serveEcho(w, r, ctx)
	case StreamRPC:
		rpc := a.rpcServer()
		if rpc == nil {
			http.Error(w, "GPU sharing is not available on this worker", http.StatusServiceUnavailable)
			return
		}
		// The RPC server restarts for a moment after a pool stops or
		// reloads; the host's new llama-server may already be connecting.
		addr, ready := rpc.Addr()
		for wait := time.Now().Add(8 * time.Second); !ready && time.Now().Before(wait) && ctx.Err() == nil; {
			time.Sleep(100 * time.Millisecond)
			addr, ready = rpc.Addr()
		}
		if !ready {
			http.Error(w, "the RPC server is not running", http.StatusServiceUnavailable)
			return
		}
		local, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			http.Error(w, "the RPC server is not reachable", http.StatusServiceUnavailable)
			return
		}
		conn, err := tunnel.AcceptStream(w, r)
		if err != nil {
			local.Close()
			return
		}
		stop := context.AfterFunc(ctx, func() { conn.Close() })
		defer stop()
		tunnel.MeteredPipe(local, conn, nil)
	default:
		http.NotFound(w, r)
	}
}

// proxyStream lets the local dashboard reach a worker's terminal or task
// runner: only this host's own certificate may call it (SelfOnly), never
// another worker.
func (a *Agent) proxyStream(w http.ResponseWriter, r *http.Request, id, path string) {
	kind := "remote_terminal"
	denied := "the owner of that computer has not allowed remote terminals"
	if path == StreamTask {
		kind, denied = "task_stream", "the owner of that computer has turned tasks off"
	}
	up, err := a.broker.OpenStream(r.Context(), id, path, a.verifyWorker)
	if err != nil {
		if strings.Contains(err.Error(), "HTTP 403") {
			http.Error(w, denied, http.StatusForbidden)
			return
		}
		http.Error(w, "worker disconnected or busy", http.StatusServiceUnavailable)
		return
	}
	down, err := tunnel.HijackStream(w, r)
	if err != nil {
		up.Close()
		return
	}
	if path == StreamTerminal {
		a.log.Printf("event=%s worker=%s state=opened", kind, id)
		defer a.log.Printf("event=%s worker=%s state=closed", kind, id)
	}
	tunnel.Pipe(down, up)
}
