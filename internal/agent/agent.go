// Package agent exposes only bounded resource and inference operations over TLS.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"meshcompute/internal/backend"
	"meshcompute/internal/config"
	"meshcompute/internal/model"
	"meshcompute/internal/network"
	"meshcompute/internal/resource"
	"meshcompute/internal/security"
	"net/http"
	"path/filepath"
	"sync"
	"time"
)

type Status struct {
	NodeID              string                `json:"node_id"`
	Resources           resource.Snapshot     `json:"resources"`
	Backend             *backend.Capabilities `json:"backend,omitempty"`
	BackendReady        bool                  `json:"backend_ready"`
	Busy                bool                  `json:"busy"`
	Model               string                `json:"model,omitempty"`
	LastOutcome         string                `json:"last_outcome,omitempty"`
	TokensPerSecond     float64               `json:"tokens_per_second,omitempty"`
	DistributedVerified bool                  `json:"distributed_verified"`
}
type Result struct {
	backend.Response
	NodeID         string `json:"node_id"`
	Model          string `json:"model"`
	Mode           string `json:"mode"`
	DeviceEvidence string `json:"device_evidence"`
}
type Agent struct {
	dir         string
	c           config.Config
	mu          sync.Mutex
	busy        bool
	model, last string
	rate        float64
	log         *log.Logger
}

func New(dir string, c config.Config, audit io.Writer) *Agent {
	return &Agent{dir: dir, c: c, log: log.New(audit, "", log.LstdFlags|log.LUTC)}
}
func (a *Agent) Status(ctx context.Context) (Status, error) {
	s, e := resource.Discover(ctx, a.dir)
	if e != nil {
		return Status{}, e
	}
	id, e := security.NodeID(a.dir)
	if e != nil {
		return Status{}, e
	}
	st := Status{NodeID: id, Resources: s}
	a.mu.Lock()
	st.Busy = a.busy
	st.Model = a.model
	st.LastOutcome = a.last
	st.TokensPerSecond = a.rate
	a.mu.Unlock()
	b, e := backend.New(a.c.BackendURL)
	if e == nil {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		caps, e := b.Health(ctx)
		if e == nil {
			st.Backend = &caps
			st.BackendReady = true
		}
	}
	return st, nil
}
func (a *Agent) Generate(ctx context.Context, r backend.Request) (Result, error) {
	a.mu.Lock()
	if a.busy {
		a.mu.Unlock()
		return Result{}, errors.New("worker busy; retry explicitly later")
	}
	a.busy = true
	a.model = r.Model
	a.last = "running"
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.busy = false
		if a.last == "running" {
			a.last = "failed"
		}
		outcome := a.last
		a.mu.Unlock()
		a.log.Printf("event=inference outcome=%s", outcome)
	}()
	if _, e := model.Get(filepath.Join(a.dir, "models"), r.Model); e != nil {
		return Result{}, errors.New("model is not registered on this worker")
	}
	b, e := backend.New(a.c.BackendURL)
	if e != nil {
		return Result{}, e
	}
	response, e := b.Generate(ctx, r)
	if e != nil {
		return Result{}, e
	}
	id, e := security.NodeID(a.dir)
	if e != nil {
		return Result{}, e
	}
	a.mu.Lock()
	a.last = "completed"
	a.rate = response.TokensPerSecond
	a.mu.Unlock()
	return Result{Response: response, NodeID: id, Model: r.Model, Mode: "single_node", DeviceEvidence: "Backend node verified; CPU/GPU participation is not instrumented. No distributed execution claim."}, nil
}
func encode(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func (a *Agent) Handler() http.Handler {
	api := http.NewServeMux()
	api.Handle("/v1/ping", network.Handler())
	api.Handle("/v1/probe", network.Handler())
	api.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		st, e := a.Status(r.Context())
		if e != nil {
			http.Error(w, "resource discovery unavailable", 503)
			return
		}
		encode(w, st)
	})
	api.HandleFunc("/v1/run", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		defer r.Body.Close()
		const limit = backend.MaxPromptBytes + 4096
		b, e := io.ReadAll(io.LimitReader(r.Body, limit+1))
		if e != nil {
			http.Error(w, "invalid inference request", 400)
			return
		}
		if len(b) > limit {
			// Bounded drain lets a streaming client finish and read the error
			// instead of observing a connection reset from a half-written body.
			_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64<<10))
			http.Error(w, "inference request too large", 400)
			return
		}
		d := json.NewDecoder(bytes.NewReader(b))
		d.DisallowUnknownFields()
		var req backend.Request
		if e := d.Decode(&req); e != nil {
			http.Error(w, "invalid inference request", 400)
			return
		}
		var extra any
		if d.Decode(&extra) != io.EOF {
			http.Error(w, "invalid trailing data", 400)
			return
		}
		result, e := a.Generate(r.Context(), req)
		if e != nil {
			http.Error(w, "inference failed: verify registered alias, backend health, context budget, and worker availability; no retry performed", 503)
			return
		}
		encode(w, result)
	})
	root := http.NewServeMux()
	root.Handle("/enroll", security.EnrollHandler(a.dir))
	root.Handle("/", security.Authorize(a.dir, api))
	return root
}
func (a *Agent) Serve(ctx context.Context) error {
	if e := a.c.Validate(); e != nil {
		return e
	}
	tlsConfig, e := security.ServerTLS(a.dir)
	if e != nil {
		return fmt.Errorf("identity unavailable; cluster create or join first: %w", e)
	}
	server := &http.Server{Addr: a.c.Listen, Handler: a.Handler(), TLSConfig: tlsConfig, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: backend.Timeout + 10*time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384, ErrorLog: log.New(io.Discard, "", 0)}
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
	a.log.Print("event=agent_start transport=mutual_tls")
	e = server.ListenAndServeTLS("", "")
	if errors.Is(e, http.ErrServerClosed) {
		return nil
	}
	return e
}
