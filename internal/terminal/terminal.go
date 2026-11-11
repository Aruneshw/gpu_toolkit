// Package terminal runs interactive shells on pseudo-terminals for the
// loopback dashboard. Sessions run as the local user. A session is either a
// shell on this computer or, on a group host, a shell on a worker that
// explicitly allowed it (see remote.go); workers can never open one here.
package terminal

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"
)

const (
	// MaxSessions bounds concurrent shells per dashboard: enough for one
	// terminal per worker of a large group plus a few local ones.
	MaxSessions = 24
	// scrollback is how much recent output a reconnecting browser replays.
	scrollback = 256 << 10
	// idleTimeout closes a session nobody has watched for this long.
	idleTimeout = 30 * time.Minute
)

// ErrUnsupported is returned on platforms without a pseudo-terminal backend.
var ErrUnsupported = errors.New("the terminal is only available on Linux and Windows 10 (1809) or newer")

// endpoint is what a session reads and writes: a local pseudo-terminal or a
// shell running on a worker.
type endpoint interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Resize(cols, rows int) error
	// Wait is called once after Read fails; it returns the shell's exit code.
	Wait() int
	// Kill ends the shell and unblocks Read.
	Kill()
}

// local is a shell on this computer attached to a pseudo-terminal.
type local struct {
	pty  console
	proc process
}

func (l *local) Read(b []byte) (int, error)  { return l.pty.Read(b) }
func (l *local) Write(b []byte) (int, error) { return l.pty.Write(b) }
func (l *local) Resize(cols, rows int) error { return l.pty.Resize(cols, rows) }
func (l *local) Kill()                       { l.proc.Kill(); _ = l.pty.Close() }
func (l *local) Wait() int {
	code := l.proc.Wait()
	_ = l.pty.Close()
	return code
}

// process is the running shell.
type process interface {
	Wait() int
	Kill()
}

// command adapts an exec.Cmd started by the operating system backend.
type command struct{ cmd *exec.Cmd }

func (c command) Kill() { killGroup(c.cmd) }
func (c command) Wait() int {
	if ee, ok := c.cmd.Wait().(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return 0
}

// console is the operating system's pseudo-terminal handle.
type console interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Resize(cols, rows int) error
	Close() error
}

// Manager owns all shell sessions of one dashboard.
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	env      []string
	workdir  string
	bashrc   string // extra lines run after ~/.bashrc in bash sessions
	rcfile   string
	closed   bool
	stop     chan struct{}
}

// Session is one running shell attached to a pseudo-terminal.
type Session struct {
	ID string
	// Node is the computer the shell runs on ("local" or a worker ID); set
	// by whoever opened it, for listing.
	Node string

	io   endpoint
	mu   sync.Mutex
	buf  []byte        // last <= scrollback bytes of output
	end  int64         // total bytes ever produced
	wake chan struct{} // closed and replaced on every write or exit
	done chan struct{}
	code int
	subs int
	seen time.Time
}

// NewManager prepares sessions that start in workdir with env appended to the
// current environment.
func NewManager(env []string, workdir string) *Manager {
	m := &Manager{sessions: map[string]*Session{}, env: env, workdir: workdir, stop: make(chan struct{})}
	go m.reap()
	return m
}

func (m *Manager) reap() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
		}
		m.mu.Lock()
		var idle []*Session
		for _, s := range m.sessions {
			s.mu.Lock()
			if s.subs == 0 && time.Since(s.seen) > idleTimeout {
				idle = append(idle, s)
			}
			s.mu.Unlock()
		}
		m.mu.Unlock()
		for _, s := range idle {
			m.Close(s.ID)
		}
	}
}

// SetBashInit adds shell lines that bash sessions run after the user's own
// ~/.bashrc, so settings such as PATH survive distribution startup files.
func (m *Manager) SetBashInit(lines string) {
	m.mu.Lock()
	m.bashrc = lines
	m.mu.Unlock()
}

// shell picks the user's shell. It starts as an ordinary interactive shell,
// like a desktop terminal window, rather than a login shell whose profile
// scripts would reset PATH.
func shell() string {
	if sh := os.Getenv("MESHCOMPUTE_SHELL"); sh != "" {
		if p, err := exec.LookPath(sh); err == nil {
			return p
		}
	}
	if runtime.GOOS == "windows" {
		for _, sh := range []string{"powershell.exe", "cmd.exe"} {
			if p, err := exec.LookPath(sh); err == nil {
				return p
			}
		}
		return "cmd.exe"
	}
	if sh := os.Getenv("SHELL"); sh != "" {
		if _, err := os.Stat(sh); err == nil {
			return sh
		}
	}
	for _, sh := range []string{"/bin/bash", "/usr/bin/bash", "/bin/zsh", "/bin/sh"} {
		if _, err := os.Stat(sh); err == nil {
			return sh
		}
	}
	return "/bin/sh"
}

// rcArgs returns bash arguments that load ~/.bashrc and then m.bashrc.
// The caller holds m.mu.
func (m *Manager) rcArgs(sh string) []string {
	if m.bashrc == "" || filepath.Base(sh) != "bash" {
		return nil
	}
	if m.rcfile == "" {
		f, err := os.CreateTemp("", "meshcompute-bashrc-")
		if err != nil {
			return nil
		}
		_, werr := f.WriteString("[ -f ~/.bashrc ] && . ~/.bashrc\n" + m.bashrc + "\n")
		if f.Close() != nil || werr != nil {
			os.Remove(f.Name())
			return nil
		}
		m.rcfile = f.Name()
	}
	return []string{"--rcfile", m.rcfile}
}

// Open starts a new shell on this computer sized cols x rows.
func (m *Manager) Open(cols, rows int) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.room(); err != nil {
		return nil, err
	}
	sh := shell()
	cmd := exec.Command(sh, m.rcArgs(sh)...)
	cmd.Dir = m.workdir
	cmd.Env = append(append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor"), m.env...)
	pty, proc, err := start(cmd, clamp(cols, 20, 500, 80), clamp(rows, 5, 200, 24))
	if err != nil {
		return nil, err
	}
	return m.add(&local{pty: pty, proc: proc})
}

// room reports whether another session may start. The caller holds m.mu.
func (m *Manager) room() error {
	if m.closed {
		return errors.New("terminal manager closed")
	}
	if len(m.sessions) >= MaxSessions {
		return errors.New("too many terminal sessions; close one first")
	}
	return nil
}

// add registers a running endpoint as a session. The caller holds m.mu.
func (m *Manager) add(e endpoint) (*Session, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		e.Kill()
		return nil, err
	}
	s := &Session{ID: hex.EncodeToString(b), io: e, wake: make(chan struct{}), done: make(chan struct{}), seen: time.Now()}
	m.sessions[s.ID] = s
	go s.pump()
	go func() {
		<-s.done
		m.mu.Lock()
		delete(m.sessions, s.ID)
		m.mu.Unlock()
	}()
	return s, nil
}

func clamp(v, lo, hi, def int) int {
	if v == 0 {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Get returns a live session.
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}

// Close terminates one session's shell and its process group.
// SessionInfo describes one running shell.
type SessionInfo struct {
	ID   string `json:"id"`
	Node string `json:"node"`
}

// Label records which computer a session's shell runs on.
func (m *Manager) Label(s *Session, node string) {
	m.mu.Lock()
	s.Node = node
	m.mu.Unlock()
}

// List returns the running shells, so a reloaded page can re-attach.
func (m *Manager) List() []SessionInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SessionInfo, 0, len(m.sessions))
	for _, s := range m.sessions {
		select {
		case <-s.done:
			continue
		default:
		}
		out = append(out, SessionInfo{ID: s.ID, Node: s.Node})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node+out[i].ID < out[j].Node+out[j].ID })
	return out
}

func (m *Manager) Close(id string) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	m.mu.Unlock()
	if ok {
		s.kill()
	}
}

// CloseAll stops every shell; later Open calls fail.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		close(m.stop)
		if m.rcfile != "" {
			os.Remove(m.rcfile)
		}
	}
	all := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()
	for _, s := range all {
		s.kill()
		<-s.done
	}
}

func (s *Session) pump() {
	buf := make([]byte, 64<<10)
	for {
		n, err := s.io.Read(buf)
		if n > 0 {
			s.mu.Lock()
			s.buf = append(s.buf, buf[:n]...)
			if len(s.buf) > scrollback {
				s.buf = append([]byte(nil), s.buf[len(s.buf)-scrollback:]...)
			}
			s.end += int64(n)
			close(s.wake)
			s.wake = make(chan struct{})
			s.mu.Unlock()
		}
		if err != nil {
			break
		}
	}
	code := s.io.Wait()
	s.mu.Lock()
	s.code = code
	close(s.done)
	close(s.wake)
	s.wake = make(chan struct{})
	s.mu.Unlock()
}

// Read returns output produced since offset (clipped to the retained
// scrollback), the next offset, a channel closed on the next change, and
// whether the shell has exited.
func (s *Session) Read(offset int64) (data []byte, next int64, wake <-chan struct{}, exited bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := s.end - int64(len(s.buf))
	if offset < start || offset > s.end {
		offset = start
	}
	data = append([]byte(nil), s.buf[offset-start:]...)
	select {
	case <-s.done:
		exited = true
	default:
	}
	return data, s.end, s.wake, exited
}

// ExitCode is valid after the shell exits.
func (s *Session) ExitCode() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.code
}

// Attach records a watching browser; the returned function detaches it.
func (s *Session) Attach() func() {
	s.mu.Lock()
	s.subs++
	s.seen = time.Now()
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		s.subs--
		s.seen = time.Now()
		s.mu.Unlock()
	}
}

// Done is closed when the shell exits.
func (s *Session) Done() <-chan struct{} { return s.done }

// Write sends keyboard input to the shell.
func (s *Session) Write(b []byte) error {
	select {
	case <-s.done:
		return errors.New("shell exited")
	default:
	}
	_, err := s.io.Write(b)
	return err
}

// Resize changes the terminal window size.
func (s *Session) Resize(cols, rows int) error {
	return s.io.Resize(clamp(cols, 20, 500, 80), clamp(rows, 5, 200, 24))
}

func (s *Session) kill() {
	select {
	case <-s.done:
		return
	default:
	}
	s.io.Kill()
}
