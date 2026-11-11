package backend

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RPCServerNames are the names llama.cpp has shipped its RPC worker under.
var RPCServerNames = []string{"rpc-server", "llama-rpc-server", "ggml-rpc-server"}

// ToolDirs lists where MeshCompute looks for llama.cpp programs besides PATH:
// next to this executable, in the state directory, and where the GPU setup
// scripts install them.
func ToolDirs(stateDir string) []string {
	var dirs []string
	if exe, err := os.Executable(); err == nil {
		d := filepath.Dir(exe)
		dirs = append(dirs, d, filepath.Join(d, "llama"))
	}
	if stateDir != "" {
		dirs = append(dirs, filepath.Join(stateDir, "llama"))
	}
	if d := os.Getenv("LOCALAPPDATA"); d != "" {
		dirs = append(dirs, filepath.Join(d, "meshcompute", "llama"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".local", "share", "meshcompute", "llama"))
	}
	return dirs
}

// FindTool returns the absolute path of a llama.cpp program. A configured
// path wins; otherwise the names are tried on PATH, then in dirs.
func FindTool(configured string, names, dirs []string) (string, error) {
	if configured != "" {
		p, err := filepath.Abs(configured)
		if err != nil {
			return "", err
		}
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			return "", fmt.Errorf("%s is not a program file", configured)
		}
		return p, nil
	}
	for _, name := range names {
		if p, err := exec.LookPath(name); err == nil {
			return filepath.Abs(p)
		}
	}
	for _, dir := range dirs {
		for _, name := range names {
			if runtime.GOOS == "windows" {
				name += ".exe"
			}
			p := filepath.Join(dir, name)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return filepath.Abs(p)
			}
		}
	}
	return "", fmt.Errorf("%s not found; install llama.cpp (see docs/backend.md)", names[0])
}

// LogTail keeps the last lines a child process printed.
type LogTail struct {
	mu    sync.Mutex
	lines []string
	part  string
	max   int
}

func NewLogTail(max int) *LogTail { return &LogTail{max: max} }

func (l *LogTail) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.part += string(b)
	for {
		i := strings.IndexByte(l.part, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(l.part[:i], "\r")
		l.part = l.part[i+1:]
		if len(line) > 400 {
			line = line[:400]
		}
		l.lines = append(l.lines, line)
		if len(l.lines) > l.max {
			l.lines = l.lines[len(l.lines)-l.max:]
		}
	}
	if len(l.part) > 4096 {
		l.part = l.part[len(l.part)-4096:]
	}
	return len(b), nil
}

// Lines returns up to n of the most recent lines.
func (l *LogTail) Lines(n int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n > len(l.lines) {
		n = len(l.lines)
	}
	return append([]string{}, l.lines[len(l.lines)-n:]...)
}

// cleanEnv drops ambient llama.cpp settings so only MeshCompute's explicit
// arguments decide what a child process loads, downloads or listens on.
func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		key := strings.ToUpper(strings.SplitN(kv, "=", 2)[0])
		if !strings.HasPrefix(key, "LLAMA_") && !strings.HasPrefix(key, "GGML_RPC") {
			env = append(env, kv)
		}
	}
	return env
}

// toolEnv is cleanEnv plus the program's own directory on the library path,
// because llama.cpp releases ship their shared libraries beside the programs.
func toolEnv(binary string) []string {
	env := cleanEnv()
	if runtime.GOOS != "linux" {
		return env
	}
	dir := filepath.Dir(binary)
	for i, kv := range env {
		if strings.HasPrefix(kv, "LD_LIBRARY_PATH=") {
			env[i] = "LD_LIBRARY_PATH=" + dir + string(os.PathListSeparator) + kv[len("LD_LIBRARY_PATH="):]
			return env
		}
	}
	return append(env, "LD_LIBRARY_PATH="+dir)
}

// freePort asks the kernel for an unused loopback TCP port.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func listening(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// RPCServer keeps a llama.cpp rpc-server running on a loopback port. The
// port is never exposed: the group host reaches it only through this
// worker's authenticated outbound tunnel.
type RPCServer struct {
	Binary string
	Log    *LogTail

	mu    sync.Mutex
	addr  string
	ready bool
}

// Addr returns the loopback address while the server accepts connections.
func (s *RPCServer) Addr() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr, s.ready
}

func (s *RPCServer) set(addr string, ready bool) {
	s.mu.Lock()
	s.addr, s.ready = addr, ready
	s.mu.Unlock()
}

// Run restarts the server until ctx ends.
func (s *RPCServer) Run(ctx context.Context) {
	if s.Log == nil {
		s.Log = NewLogTail(50)
	}
	cache := true // "-c" caches received weights on disk; older builds lack it
	delay := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		wasReady, err := s.once(ctx, cache)
		s.set("", false)
		if ctx.Err() != nil {
			return
		}
		if !wasReady && cache {
			cache = false // retry without the optional flag
			continue
		}
		if wasReady && time.Since(started) > time.Minute {
			delay = time.Second
		} else if delay < 30*time.Second {
			delay *= 2
		}
		fmt.Fprintf(s.Log, "rpc-server stopped (%v); restarting in %s\n", err, delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func (s *RPCServer) once(ctx context.Context, cache bool) (ready bool, err error) {
	port, err := freePort()
	if err != nil {
		return false, err
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	args := []string{"--host", "127.0.0.1", "--port", strconv.Itoa(port)}
	if cache {
		args = append(args, "-c")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.Binary, args...)
	cmd.Dir = filepath.Dir(s.Binary) // bundled CUDA/runtime libraries live beside it
	cmd.Env = toolEnv(s.Binary)
	cmd.Stdout, cmd.Stderr = s.Log, s.Log
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return false, err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	poll := time.NewTicker(200 * time.Millisecond)
	defer poll.Stop()
	deadline := time.After(60 * time.Second)
	for !ready {
		select {
		case err := <-exited:
			return false, err
		case <-deadline:
			cancel()
			<-exited
			return false, errors.New("rpc-server did not start listening")
		case <-poll.C:
			if listening(addr) {
				ready = true
				s.set(addr, true)
			}
		}
	}
	return true, <-exited
}
