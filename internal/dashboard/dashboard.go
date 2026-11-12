// Package dashboard serves a token-protected, loopback-only control panel.
//
// It aggregates telemetry from the local agent and its workers, opens
// terminals (here, and on workers that allowed it) and controls the group
// host's pooled model. Without the pool, a request runs on one selected node.
package dashboard

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"meshcompute/internal/agent"
	"meshcompute/internal/backend"
	"meshcompute/internal/config"
	"meshcompute/internal/network"
	"meshcompute/internal/security"
	"meshcompute/internal/sse"
	"meshcompute/internal/terminal"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed index.html static
var assets embed.FS

const (
	cookieName = "meshcompute_token"
	maxBody    = backend.MaxPromptBytes + 4096
	scopeNote  = "Totals add up every connected computer. Start the pooled model to run one model across all shared GPUs; without it each request runs entirely on one selected node."
)

// scope describes how inference is placed right now.
func scope(nodes []Node) string {
	for _, n := range nodes {
		if n.Name != "local" || n.Status == nil || n.Status.Pool == nil {
			continue
		}
		p := n.Status.Pool
		switch {
		case p.State == "ready" && len(p.Workers) > 0:
			return fmt.Sprintf("Pooled: model %q runs across this computer and %d worker GPU(s) through llama.cpp RPC. Choose node \"local\" to use them all.", p.Model, len(p.Workers))
		case p.State == "ready":
			return fmt.Sprintf("Pool is on: model %q runs on this computer; workers that lend a GPU are added automatically.", p.Model)
		case p.Enabled:
			return "Pool is " + p.State + ": " + p.Detail
		}
	}
	return scopeNote
}

// Node is one cluster member as reported by its agent.
type Node struct {
	Name    string        `json:"name"`
	Healthy bool          `json:"healthy"`
	Status  *agent.Status `json:"status,omitempty"`
	Error   string        `json:"error,omitempty"`
}

// Aggregate is a display-only sum across healthy nodes.
type Aggregate struct {
	Nodes             int      `json:"nodes"`
	Healthy           int      `json:"healthy"`
	GPUs              int      `json:"gpus"`
	VRAMTotalBytes    uint64   `json:"vram_total_bytes"`
	VRAMFreeBytes     uint64   `json:"vram_free_bytes"`
	RAMTotalBytes     uint64   `json:"ram_total_bytes"`
	RAMAvailableBytes uint64   `json:"ram_available_bytes"`
	Models            []string `json:"models"`
}

// StatusResponse is the body of GET /api/status.
type StatusResponse struct {
	Nodes     []Node    `json:"nodes"`
	Aggregate Aggregate `json:"aggregate"`
	Scope     string    `json:"scope"`
}

// Cluster is the small surface the HTTP layer needs. Tests supply a fake.
type Cluster interface {
	Statuses(context.Context) ([]Node, error)
	Stream(context.Context, string, backend.Request, func(backend.Chunk) error) error
}

// Remote talks to local and peer agents with the local pinned mTLS identity.
type Remote struct {
	dir string
	c   config.Config

	pingMu     sync.Mutex
	pingClient *http.Client
}

func NewRemote(dir string, c config.Config) *Remote {
	return &Remote{dir: dir, c: c}
}

func (r *Remote) endpoint(ctx context.Context, name string) (string, error) {
	if name == "local" {
		return r.c.LocalEndpoint(), nil
	}
	peers, err := r.Peers(ctx)
	if err != nil {
		return "", err
	}
	for _, p := range peers {
		if p.Name == name {
			return p.Endpoint, nil
		}
	}
	return "", errors.New("unknown node; add it with peer add NAME HTTPS_ENDPOINT")
}

// Peers discovers creator-owned reverse connections through the authenticated
// local agent, so CLI and dashboard processes share the same broker.
func (r *Remote) Peers(ctx context.Context) ([]config.Peer, error) {
	peers := append([]config.Peer{}, r.c.Peers...)
	if !r.c.GroupHost {
		return peers, nil
	}
	cl, err := security.Client(r.dir)
	if err != nil {
		return nil, err
	}
	defer cl.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var connected []config.Peer
	if err := readJSON(ctx, cl, r.c.LocalEndpoint(), "/v1/workers", nil, &connected); err != nil {
		return nil, err
	}
	return append(peers, connected...), nil
}

func (r *Remote) Endpoint(ctx context.Context, name string) (string, error) {
	return r.endpoint(ctx, name)
}

func readJSON(ctx context.Context, cl *http.Client, endpoint, path string, body, result any) error {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	method := http.MethodGet
	if body != nil {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(endpoint, "/")+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := cl.Do(req)
	if err != nil {
		return errors.New("agent unreachable or authentication failed")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		if why := backend.ErrorText(res); why != "" {
			return fmt.Errorf("agent returned HTTP %d: %s", res.StatusCode, why)
		}
		return fmt.Errorf("agent returned HTTP %d", res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, backend.MaxResponseBytes+1))
	if err != nil {
		return err
	}
	if len(b) > backend.MaxResponseBytes {
		return errors.New("agent response too large")
	}
	return json.Unmarshal(b, result)
}

func (r *Remote) Statuses(ctx context.Context) ([]Node, error) {
	cl, err := security.Client(r.dir)
	if err != nil {
		return nil, err
	}
	defer cl.CloseIdleConnections()
	configured, err := r.Peers(ctx)
	if err != nil {
		return nil, err
	}
	peers := append([]config.Peer{{Name: "local", Endpoint: r.c.LocalEndpoint()}}, configured...)
	// Ask every computer at once, so one slow or offline laptop does not
	// delay the others.
	rows := make([]Node, len(peers))
	var wg sync.WaitGroup
	for i, p := range peers {
		wg.Add(1)
		go func(i int, p config.Peer) {
			defer wg.Done()
			var st agent.Status
			bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := readJSON(bounded, cl, p.Endpoint, "/v1/status", nil, &st)
			cancel()
			row := Node{Name: p.Name, Healthy: err == nil}
			if err != nil {
				row.Error = err.Error()
			} else {
				row.Status = &st
			}
			rows[i] = row
		}(i, p)
	}
	wg.Wait()
	return rows, nil
}

// Stream runs one completion on exactly one node. It repeats the same readiness
// and network admission checks as the run command, then relays the agent's SSE
// stream without retrying.
func (r *Remote) Stream(ctx context.Context, name string, req backend.Request, emit func(backend.Chunk) error) error {
	endpoint, err := r.endpoint(ctx, name)
	if err != nil {
		return err
	}
	cl, err := security.Client(r.dir)
	if err != nil {
		return err
	}
	defer cl.CloseIdleConnections()
	cl.Timeout = backend.StreamTimeout + backend.Timeout
	var st agent.Status
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = readJSON(bounded, cl, endpoint, "/v1/status", nil, &st)
	cancel()
	if err != nil {
		return refuse("selected node is not reachable")
	}
	if why := agent.CanServe(st, &req, true); why != "" {
		return refuse(why)
	}
	if name != "local" {
		q, err := network.Measure(ctx, cl, endpoint)
		if err != nil || !q.Available || !q.Stable {
			return refuse("remote network failed admission; no inference started")
		}
	}
	var data []byte
	data, err = json.Marshal(req)
	if err != nil {
		return err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(endpoint, "/")+"/v1/run/stream", bytes.NewReader(data))
	if err != nil {
		return err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
	res, err := cl.Do(hreq)
	if err != nil {
		return errors.New("agent stream unreachable or authentication failed")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		if why := backend.ErrorText(res); why != "" {
			return refuse(why)
		}
		return fmt.Errorf("agent stream returned HTTP %d", res.StatusCode)
	}
	return sse.Read(res.Body, func(event string, data []byte) error {
		switch event {
		case "token":
			var v struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal(data, &v); err != nil {
				return errors.New("invalid token event")
			}
			if v.Content != "" {
				return emit(backend.Chunk{Content: v.Content})
			}
			return nil
		case "done":
			var v struct {
				TokensPerSecond float64 `json:"tokens_per_second"`
			}
			if err := json.Unmarshal(data, &v); err != nil {
				return errors.New("invalid completion event")
			}
			if err := emit(backend.Chunk{Done: true, TokensPerSecond: v.TokensPerSecond}); err != nil {
				return err
			}
			return sse.Done
		case "error":
			// The agent sends agent.Reason: an admission reason or a
			// generic failure, never backend internals.
			var v struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(data, &v) == nil && v.Error != "" && len(v.Error) <= 300 {
				return refuse(v.Error)
			}
			return errors.New("remote inference failed")
		default:
			return nil
		}
	})
}

func aggregate(nodes []Node) Aggregate {
	a := Aggregate{Nodes: len(nodes), Models: []string{}}
	models := map[string]bool{}
	for _, n := range nodes {
		if !n.Healthy || n.Status == nil {
			continue
		}
		a.Healthy++
		res := n.Status.Resources
		if res.RAMTotalBytes != nil {
			a.RAMTotalBytes += *res.RAMTotalBytes
		}
		if res.RAMAvailableBytes != nil {
			a.RAMAvailableBytes += *res.RAMAvailableBytes
		}
		for _, g := range res.GPUs {
			a.GPUs++
			if g.TotalBytes != nil {
				a.VRAMTotalBytes += *g.TotalBytes
			}
			if g.FreeBytes != nil {
				a.VRAMFreeBytes += *g.FreeBytes
			}
		}
		if n.Status.Backend != nil {
			for _, m := range n.Status.Backend.Models {
				models[m] = true
			}
		}
	}
	for m := range models {
		a.Models = append(a.Models, m)
	}
	sort.Strings(a.Models)
	return a
}

func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func validToken(token string) bool {
	if len(token) != 64 {
		return false
	}
	_, err := hex.DecodeString(token)
	return err == nil
}

// Server is the loopback dashboard HTTP server.
type Server struct {
	cluster        Cluster
	listen         string
	port           string
	token          string
	allowedOrigins map[string]bool
	handler        http.Handler
	inviter        Inviter
	term           *terminal.Manager
	localTerm      bool
	cli            string
	// role is "host" (group host), "worker" (a worker's own dashboard) or
	// "local".
	role   string
	tasks  tasks
	worker WorkerConsole
	fleet  fleet
	lib    library
}

func New(cluster Cluster, listen, token string) (*Server, error) {
	if cluster == nil {
		return nil, errors.New("cluster client required")
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, errors.New("listen must be an IP:port on loopback")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, errors.New("dashboard listen address must be loopback")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return nil, errors.New("dashboard port must be 1-65535")
	}
	if token == "" {
		if token, err = NewToken(); err != nil {
			return nil, err
		}
	} else if !validToken(token) {
		return nil, errors.New("dashboard token must be 64 hexadecimal characters")
	}
	s := &Server{cluster: cluster, listen: listen, port: port, token: token, cli: "meshcompute", role: "local"}
	if inv, ok := cluster.(Inviter); ok {
		s.inviter = inv
	}
	s.allowedOrigins = map[string]bool{
		"http://127.0.0.1:" + port: true,
		"http://localhost:" + port: true,
		"http://[::1]:" + port:     true,
	}
	s.handler = s.guard(s.routes())
	return s, nil
}

func (s *Server) Token() string  { return s.token }
func (s *Server) Listen() string { return s.listen }

func (s *Server) validHost(hostport string) bool {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil || port != s.port {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) validOrigin(origin string) bool {
	if origin == "" {
		return true // non-browser client; the token is still required
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || u.User != nil {
		return false
	}
	return s.allowedOrigins[origin]
}

func (s *Server) matches(token string) bool {
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) == 1
}

func (s *Server) authorized(r *http.Request) bool {
	if token := r.Header.Get("X-MeshCompute-Token"); token != "" && s.matches(token) {
		return true
	}
	if token := r.URL.Query().Get("token"); token != "" && s.matches(token) {
		return true
	}
	if c, err := r.Cookie(cookieName); err == nil && s.matches(c.Value) {
		return true
	}
	return false
}

func securityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
}

func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w)
		if !s.validHost(r.Host) {
			http.Error(w, "invalid host", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost && !s.validOrigin(r.Header.Get("Origin")) {
			http.Error(w, "invalid origin", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/" {
			if token := r.URL.Query().Get("token"); token != "" {
				if !s.matches(token) {
					http.Error(w, "invalid dashboard token", http.StatusForbidden)
					return
				}
				http.SetCookie(w, &http.Cookie{Name: cookieName, Value: s.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 12 * 60 * 60})
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
		}
		if !s.authorized(r) {
			http.Error(w, "dashboard token required; open the URL printed by meshcompute ui", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		b, err := assets.ReadFile("index.html")
		if err != nil {
			http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(b)
	})
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		nodes, err := s.cluster.Statuses(r.Context())
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "cluster identity unavailable")
			return
		}
		writeJSON(w, StatusResponse{Nodes: nodes, Aggregate: aggregate(nodes), Scope: scope(nodes)})
	})
	mux.HandleFunc("/api/run", s.handleRun)
	s.toolRoutes(mux)
	s.fleetRoutes(mux)
	s.modelRoutes(mux)
	return mux
}

type runRequest struct {
	Node string `json:"node"`
	backend.Request
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	defer r.Body.Close()
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid inference request")
		return
	}
	if len(b) > maxBody {
		writeError(w, http.StatusBadRequest, "inference request too large")
		return
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	var req runRequest
	if err := d.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid inference request")
		return
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid trailing data")
		return
	}
	if req.Node == "" || !backend.ValidRequest(req.Request) {
		writeError(w, http.StatusBadRequest, "invalid node, model, prompt, output limit or context budget")
		return
	}
	sse.Prepare(w)
	sw := sse.NewWriter(w)
	err = s.cluster.Stream(r.Context(), req.Node, req.Request, func(c backend.Chunk) error {
		if c.Done {
			return sw.Event("done", map[string]any{"tokens_per_second": c.TokensPerSecond})
		}
		return sw.Event("token", map[string]any{"content": c.Content})
	})
	if err != nil {
		message := "inference failed; verify node readiness, model, context and worker availability; no retry performed"
		var why refusal
		if errors.As(err, &why) {
			message = why.Error()
		}
		_ = sw.Event("error", map[string]string{"error": message})
	}
}

// refusal is a reason a run was declined or failed that MeshCompute itself
// wrote for people (no free slot, unknown pool, context too large, the
// agent's own sanitized error), so the page may show it. Other errors stay
// generic.
type refusal struct{ error }

func refuse(message string) error { return refusal{errors.New(message)} }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func (s *Server) Handler() http.Handler { return s.handler }

// Serve runs the dashboard until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	server := &http.Server{
		Addr:              s.listen,
		Handler:           s.handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16384,
	}
	if s.term != nil {
		defer s.term.CloseAll()
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdown)
		case <-done:
		}
	}()
	err := server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
