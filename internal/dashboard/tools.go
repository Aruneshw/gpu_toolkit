package dashboard

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"meshcompute/internal/agent"
	"meshcompute/internal/model"
	"meshcompute/internal/security"
	"meshcompute/internal/sse"
	"meshcompute/internal/terminal"
	"meshcompute/internal/tunnel"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ReleasesURL is where friends download the ready-to-run packages.
const ReleasesURL = "https://github.com/Aruneshw/gpu_toolkit/releases/latest"

// Command is one copy-ready instruction shown next to an invitation.
type Command struct {
	Label   string `json:"label"`
	Command string `json:"command"`
}

// Invitation is a freshly issued single-use join code plus the exact commands
// a friend runs, so nobody has to assemble them by hand.
type Invitation struct {
	Code      string    `json:"code"`
	Endpoint  string    `json:"endpoint"`
	ExpiresAt time.Time `json:"expires_at"`
	Commands  []Command `json:"commands"`
	Download  string    `json:"download"`
}

// Inviter issues invitations. Remote implements it for group hosts.
type Inviter interface {
	Invite(ctx context.Context, ttl time.Duration) (Invitation, error)
}

// HostInfo describes this dashboard's group for the invite panel.
type HostInfo struct {
	GroupHost    bool   `json:"group_host"`
	Endpoint     string `json:"endpoint"`
	Reachability string `json:"reachability"`
}

// Invite issues a single-use invitation for this group host.
func (r *Remote) Invite(_ context.Context, ttl time.Duration) (Invitation, error) {
	if !r.c.GroupHost {
		return Invitation{}, errors.New("this dashboard is not a group host; start it with: meshcompute host")
	}
	code, err := security.Pair(r.dir, r.c.Advertise, ttl)
	if err != nil {
		return Invitation{}, err
	}
	return Invitation{
		Code:      code,
		Endpoint:  r.c.Advertise,
		ExpiresAt: time.Now().Add(ttl).UTC(),
		Download:  ReleasesURL,
		Commands:  JoinCommands(code),
	}, nil
}

// JoinCommands are the friend-side commands for one invitation code.
func JoinCommands(code string) []Command {
	return []Command{
		{Label: "Windows: open PowerShell inside the extracted meshcompute-windows-amd64.zip folder (or double-click win.bat, choose 2 and paste the key)", Command: `.\join.cmd ` + code},
		{Label: "Windows, already installed (any PowerShell window)", Command: `& "$env:LOCALAPPDATA\meshcompute\bin\meshcompute.exe" join ` + code},
		{Label: "Linux: in a terminal inside the extracted meshcompute-linux-amd64.tar.gz folder", Command: `./linux.sh join ` + code},
	}
}

// Info reports the group endpoint and what the friend's network needs.
func (r *Remote) Info() HostInfo {
	info := HostInfo{GroupHost: r.c.GroupHost, Endpoint: r.c.Advertise}
	u, err := url.Parse(r.c.Advertise)
	if err != nil {
		return info
	}
	ip := net.ParseIP(u.Hostname())
	switch {
	case r.c.Relay != "":
		info.Reachability = "Public relay " + r.c.Relay + ":" + u.Port() + ": friends on any network can join (no IPv6, firewall or router setup). Traffic is end-to-end encrypted; the relay only forwards it."
	case ip == nil:
	case ip.IsLoopback():
		info.Reachability = "Loopback address: only this computer can join. Restart with: meshcompute host --auto"
	case ip.To4() == nil && ip.IsGlobalUnicast() && !ip.IsPrivate():
		info.Reachability = "Global IPv6: your friend's network must have IPv6 (test at test-ipv6.com). No port forwarding needed; allow TCP " + u.Port() + " in your firewall."
	case ip.IsPrivate():
		info.Reachability = "Private IPv4: works for friends on the same network, or with TCP " + u.Port() + " forwarded on your router."
	default:
		info.Reachability = "Public IPv4: TCP " + u.Port() + " must be forwarded to this computer."
	}
	return info
}

type infoProvider interface{ Info() HostInfo }

// TerminalDialer opens a shell stream on a worker that allowed it.
type TerminalDialer interface {
	DialTerminal(ctx context.Context, node string) (io.ReadWriteCloser, error)
}

// Pooler controls the group host's pooled model.
type Pooler interface {
	SetPool(ctx context.Context, enabled bool, model string, contextSize int) (agent.PoolStatus, error)
	Models() []string
}

// ErrTerminalDenied means the worker's owner has not allowed remote terminals.
var ErrTerminalDenied = errors.New("the owner of that computer has not allowed remote terminals; they enable it with: meshcompute config set allow_terminal true (then reconnect)")

// DialTerminal reaches a worker's shell through this host's agent, which
// forwards it down the worker's own authenticated connection.
func (r *Remote) DialTerminal(ctx context.Context, node string) (io.ReadWriteCloser, error) {
	if !r.c.GroupHost || !strings.HasPrefix(node, "worker-") {
		return nil, errors.New("remote terminals are available for workers of a group you host")
	}
	endpoint, err := r.endpoint(ctx, node)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	cfg, err := security.ClientTLS(r.dir)
	if err != nil {
		return nil, err
	}
	conn, err := tunnel.DialStream(ctx, "https://"+u.Host, u.Path+agent.StreamTerminal, cfg)
	if err != nil {
		if strings.Contains(err.Error(), "HTTP 403") {
			return nil, ErrTerminalDenied
		}
		return nil, errors.New("that computer is offline or busy; try again in a few seconds")
	}
	return conn, nil
}

// SetPool turns the pooled model on or off on this group host.
func (r *Remote) SetPool(ctx context.Context, enabled bool, name string, contextSize int) (agent.PoolStatus, error) {
	var st agent.PoolStatus
	if !r.c.GroupHost {
		return st, errors.New("the pool runs on a group host; start one with: meshcompute host")
	}
	if enabled {
		// Fail with a useful message before asking the agent.
		if _, err := model.Get(filepath.Join(r.dir, "models"), name); err != nil {
			return st, errors.New("that model is not registered on this computer")
		}
	}
	cl, err := security.Client(r.dir)
	if err != nil {
		return st, err
	}
	defer cl.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	err = readJSON(ctx, cl, r.c.LocalEndpoint(), "/v1/pool", map[string]any{"enabled": enabled, "model": name, "context": contextSize}, &st)
	return st, err
}

// Models lists the models registered on this computer.
func (r *Remote) Models() []string {
	names := []string{}
	entries, err := model.List(filepath.Join(r.dir, "models"))
	if err != nil {
		return names
	}
	for _, e := range entries {
		names = append(names, e.Name)
	}
	return names
}

// EnableTerminal turns on the browser shell for the local user. The shell
// starts in the user's home directory with MESHCOMPUTE_HOME pointing at dir
// and this executable first on PATH, so meshcompute commands work directly.
func (s *Server) EnableTerminal(dir string) {
	// Local shells need Linux or Windows; shells on workers work from any host.
	s.localTerm = runtime.GOOS == "linux" || runtime.GOOS == "windows"
	env := []string{}
	if dir != "" {
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
		env = append(env, "MESHCOMPUTE_HOME="+dir)
	}
	s.cli = "meshcompute"
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		env = append(env, "PATH="+filepath.Dir(exe)+string(os.PathListSeparator)+os.Getenv("PATH"), "MESHCOMPUTE_BIN="+exe)
		s.cli = strings.TrimSuffix(filepath.Base(exe), ".exe")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	s.term = terminal.NewManager(env, home)
	if exe, ok := envValue(env, "MESHCOMPUTE_BIN"); ok {
		// Distribution bashrc files may rewrite PATH; re-add the binary after
		// them and make "meshcompute" work whatever the file is called.
		s.term.SetBashInit("export PATH=" + shellQuote(filepath.Dir(exe)) + ":\"$PATH\"\nalias meshcompute=" + shellQuote(exe))
		s.cli = "meshcompute"
	}
}

func envValue(env []string, key string) (string, bool) {
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			return kv[len(key)+1:], true
		}
	}
	return "", false
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (s *Server) toolRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/static/", s.handleStatic)
	mux.HandleFunc("/api/info", s.handleInfo)
	s.taskRoutes(mux)
	s.gridRoutes(mux)
	mux.HandleFunc("/api/invite", s.handleInvite)
	mux.HandleFunc("/api/pool", s.handlePool)
	mux.HandleFunc("/api/term/open", s.handleTermOpen)
	mux.HandleFunc("/api/term/stream", s.handleTermStream)
	mux.HandleFunc("/api/term/input", s.handleTermInput)
	mux.HandleFunc("/api/term/resize", s.handleTermResize)
	mux.HandleFunc("/api/term/close", s.handleTermClose)
}

var staticTypes = map[string]string{
	".js":  "text/javascript; charset=utf-8",
	".css": "text/css; charset=utf-8",
	".txt": "text/plain; charset=utf-8",
	".svg": "image/svg+xml",
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/static/")
	ctype, ok := staticTypes[path.Ext(name)]
	if r.Method != http.MethodGet || !ok || strings.Contains(name, "/") || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	b, err := assets.ReadFile("static/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ctype)
	_, _ = w.Write(b)
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	_, remoteTerm := s.cluster.(TerminalDialer)
	resp := map[string]any{"role": s.role, "tasks": s.tasks != nil || s.worker != nil, "invites": s.inviter != nil, "terminal": s.term != nil, "local_terminal": s.term != nil && s.localTerm, "remote_terminal": s.term != nil && remoteTerm, "cli": s.cli, "download": ReleasesURL}
	if p, ok := s.cluster.(Pooler); ok {
		resp["models"] = p.Models()
	}
	if p, ok := s.cluster.(infoProvider); ok {
		resp["host"] = p.Info()
	}
	writeJSON(w, resp)
}

// decode reads one bounded JSON object with no unknown fields.
func decode(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return false
	}
	defer r.Body.Close()
	b, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil || int64(len(b)) > limit {
		writeError(w, http.StatusBadRequest, "request too large or unreadable")
		return false
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return false
	}
	return true
}

func (s *Server) handleInvite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TTLMinutes int `json:"ttl_minutes"`
	}
	if !decode(w, r, 1024, &req) {
		return
	}
	if s.inviter == nil {
		writeError(w, http.StatusNotFound, "invitations are not available on this dashboard")
		return
	}
	if req.TTLMinutes < 1 || req.TTLMinutes > 24*60 {
		writeError(w, http.StatusBadRequest, "expiry must be between 1 minute and 24 hours")
		return
	}
	inv, err := s.inviter.Invite(r.Context(), time.Duration(req.TTLMinutes)*time.Minute)
	if err != nil {
		writeError(w, http.StatusConflict, "could not create invitation: "+err.Error())
		return
	}
	writeJSON(w, inv)
}

func (s *Server) session(w http.ResponseWriter, id string) (*terminal.Session, bool) {
	if s.term == nil {
		writeError(w, http.StatusNotFound, "terminal is not enabled on this dashboard")
		return nil, false
	}
	sess, ok := s.term.Get(id)
	if !ok {
		writeError(w, http.StatusGone, "terminal session ended")
		return nil, false
	}
	return sess, true
}

func (s *Server) handlePool(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool   `json:"enabled"`
		Model   string `json:"model"`
		Context int    `json:"context"`
	}
	if !decode(w, r, 1024, &req) {
		return
	}
	p, ok := s.cluster.(Pooler)
	if !ok {
		writeError(w, http.StatusNotFound, "the pool is not available on this dashboard")
		return
	}
	st, err := p.SetPool(r.Context(), req.Enabled, strings.TrimSpace(req.Model), req.Context)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, st)
}

func (s *Server) handleTermOpen(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Node string `json:"node"`
		Cols int    `json:"cols"`
		Rows int    `json:"rows"`
	}
	if !decode(w, r, 1024, &req) {
		return
	}
	if s.term == nil {
		writeError(w, http.StatusNotFound, "terminal is not enabled on this dashboard")
		return
	}
	var sess *terminal.Session
	var err error
	if req.Node == "" || req.Node == "local" {
		sess, err = s.term.Open(req.Cols, req.Rows)
	} else {
		dialer, ok := s.cluster.(TerminalDialer)
		if !ok {
			writeError(w, http.StatusNotFound, "remote terminals are not available on this dashboard")
			return
		}
		var stream io.ReadWriteCloser
		if stream, err = dialer.DialTerminal(r.Context(), req.Node); err == nil {
			sess, err = s.term.Attach(stream, req.Cols, req.Rows)
		}
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	node := req.Node
	if node == "" {
		node = "local"
	}
	s.term.Label(sess, node)
	writeJSON(w, map[string]string{"id": sess.ID, "node": node})
}

func (s *Server) handleTermInput(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		Data string `json:"data"`
		// IDs, instead of ID, types the same input into several shells
		// ("type into all"). Shells that ended are skipped.
		IDs []string `json:"ids"`
	}
	if !decode(w, r, 256<<10, &req) {
		return
	}
	if len(req.IDs) > 0 {
		if s.term == nil || len(req.IDs) > maxMulti {
			writeError(w, http.StatusBadRequest, "invalid sessions")
			return
		}
		written := 0
		for _, id := range req.IDs {
			if sess, ok := s.term.Get(id); ok && sess.Write([]byte(req.Data)) == nil {
				written++
			}
		}
		if written == 0 {
			writeError(w, http.StatusGone, "terminal sessions ended")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	sess, ok := s.session(w, req.ID)
	if !ok {
		return
	}
	if err := sess.Write([]byte(req.Data)); err != nil {
		writeError(w, http.StatusGone, "terminal session ended")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTermResize(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		Cols int    `json:"cols"`
		Rows int    `json:"rows"`
	}
	if !decode(w, r, 1024, &req) {
		return
	}
	sess, ok := s.session(w, req.ID)
	if !ok {
		return
	}
	_ = sess.Resize(req.Cols, req.Rows)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTermClose(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if !decode(w, r, 1024, &req) {
		return
	}
	if s.term != nil {
		s.term.Close(req.ID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTermStream relays shell output as server-sent events. The offset lets
// a reloaded page resume (and replay scrollback) without losing output.
func (s *Server) handleTermStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	sess, ok := s.session(w, r.URL.Query().Get("id"))
	if !ok {
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil {
		offset = -1
	}
	detach := sess.Attach()
	defer detach()
	sse.Prepare(w)
	sw := sse.NewWriter(w)
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		data, next, wake, exited := sess.Read(offset)
		for len(data) > 0 {
			chunk := data
			if len(chunk) > 48<<10 {
				chunk = chunk[:48<<10]
			}
			data = data[len(chunk):]
			if err := sw.Event("output", map[string]any{"data": base64.StdEncoding.EncodeToString(chunk), "offset": next - int64(len(data))}); err != nil {
				return
			}
		}
		offset = next
		if exited {
			_ = sw.Event("exit", map[string]int{"code": sess.ExitCode()})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-wake:
		case <-heartbeat.C:
			if err := sw.Event("ping", map[string]bool{"ok": true}); err != nil {
				return
			}
		}
	}
}
