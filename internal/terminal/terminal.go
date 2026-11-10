// Package terminal runs interactive shells on pseudo-terminals for the
// loopback dashboard. Sessions belong to the local user and run with that
// user's privileges; nothing here is reachable from enrolled workers.
package terminal

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

const (
	// MaxSessions bounds concurrent shells per dashboard.
	MaxSessions = 8
	// scrollback is how much recent output a reconnecting browser replays.
	scrollback = 256 << 10
	// idleTimeout closes a session nobody has watched for this long.
	idleTimeout = 30 * time.Minute
)

// ErrUnsupported is returned on platforms without a pseudo-terminal backend.
var ErrUnsupported = errors.New("the dashboard terminal is only available on Linux hosts")

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

	pty  *os.File
	cmd  *exec.Cmd
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

// Open starts a new shell sized cols x rows.
func (m *Manager) Open(cols, rows int) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("terminal manager closed")
	}
	if len(m.sessions) >= MaxSessions {
		return nil, errors.New("too many terminal sessions; close one first")
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	sh := shell()
	cmd := exec.Command(sh, m.rcArgs(sh)...)
	cmd.Dir = m.workdir
	cmd.Env = append(append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor"), m.env...)
	pty, err := start(cmd, clamp(cols, 20, 500, 80), clamp(rows, 5, 200, 24))
	if err != nil {
		return nil, err
	}
	s := &Session{ID: hex.EncodeToString(b), pty: pty, cmd: cmd, wake: make(chan struct{}), done: make(chan struct{}), seen: time.Now()}
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
	buf := make([]byte, 32<<10)
	for {
		n, err := s.pty.Read(buf)
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
	err := s.cmd.Wait()
	s.mu.Lock()
	s.code = 0
	if ee, ok := err.(*exec.ExitError); ok {
		s.code = ee.ExitCode()
	}
	close(s.done)
	close(s.wake)
	s.wake = make(chan struct{})
	s.mu.Unlock()
	_ = s.pty.Close()
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
	_, err := s.pty.Write(b)
	return err
}

// Resize changes the terminal window size.
func (s *Session) Resize(cols, rows int) error {
	return resize(s.pty, clamp(cols, 20, 500, 80), clamp(rows, 5, 200, 24))
}

func (s *Session) kill() {
	select {
	case <-s.done:
		return
	default:
	}
	killGroup(s.cmd)
	_ = s.pty.Close()
}
