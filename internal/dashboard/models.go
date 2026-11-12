package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"meshcompute/internal/agent"
	"meshcompute/internal/config"
	"meshcompute/internal/model"
	"meshcompute/internal/security"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// PoolManager reads and changes a group host's pools.
type PoolManager interface {
	Pools(ctx context.Context) (agent.PoolsStatus, error)
	ChangePools(ctx context.Context, action string, pool config.PoolConfig, name string) (agent.PoolsStatus, error)
}

// ModelLibrary lists, finds and registers model files on this computer.
type ModelLibrary interface {
	ModelDir() string
	ConfigDir() string
}

// Pools reads every pool from this host's agent.
func (r *Remote) Pools(ctx context.Context) (agent.PoolsStatus, error) {
	var st agent.PoolsStatus
	if !r.c.GroupHost {
		return st, errors.New("pools run on a group host; start one with: meshcompute host")
	}
	cl, err := security.Client(r.dir)
	if err != nil {
		return st, err
	}
	defer cl.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	err = readJSON(ctx, cl, r.c.LocalEndpoint(), "/v1/pools", nil, &st)
	return st, err
}

// ChangePools sets ("set") or removes ("remove") one pool.
func (r *Remote) ChangePools(ctx context.Context, action string, pool config.PoolConfig, name string) (agent.PoolsStatus, error) {
	var st agent.PoolsStatus
	if !r.c.GroupHost {
		return st, errors.New("pools run on a group host; start one with: meshcompute host")
	}
	cl, err := security.Client(r.dir)
	if err != nil {
		return st, err
	}
	defer cl.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	err = readJSON(ctx, cl, r.c.LocalEndpoint(), "/v1/pools", map[string]any{"action": action, "pool": pool, "name": name}, &st)
	return st, err
}

// ModelDir is where this computer's model registry lives.
func (r *Remote) ModelDir() string { return filepath.Join(r.dir, "models") }

// ConfigDir is this computer's state directory.
func (r *Remote) ConfigDir() string { return r.dir }

// ModelInfo is a registered model as the library shows it.
type ModelInfo struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	// Missing: the file is not there now (an external disk may be unplugged).
	Missing bool `json:"missing,omitempty"`
}

// addJob is a registration running in the background: hashing a large
// model on a slow disk takes minutes.
type addJob struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	State   string    `json:"state"` // hashing | done | failed
	Error   string    `json:"error,omitempty"`
	Started time.Time `json:"started"`
}

type library struct {
	mu   sync.Mutex
	jobs []*addJob
}

func (s *Server) modelRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/pools", s.handlePools)
	mux.HandleFunc("/api/models", s.handleModels)
	mux.HandleFunc("/api/models/scan", s.handleModelScan)
	mux.HandleFunc("/api/models/add", s.handleModelAdd)
	mux.HandleFunc("/api/models/remove", s.handleModelRemove)
}

func (s *Server) handlePools(w http.ResponseWriter, r *http.Request) {
	pm, ok := s.cluster.(PoolManager)
	if !ok || s.worker != nil {
		writeError(w, http.StatusNotFound, "pools run on a group host")
		return
	}
	switch r.Method {
	case http.MethodGet:
		st, err := pm.Pools(r.Context())
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeJSON(w, st)
	case http.MethodPost:
		var req struct {
			Action string            `json:"action"`
			Pool   config.PoolConfig `json:"pool"`
			Name   string            `json:"name"`
		}
		if !decode(w, r, 64<<10, &req) {
			return
		}
		st, err := pm.ChangePools(r.Context(), req.Action, req.Pool, req.Name)
		if err != nil {
			writeError(w, http.StatusBadRequest, cleanAgentError(err))
			return
		}
		writeJSON(w, st)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// cleanAgentError strips transport wording from an error the agent sent.
func cleanAgentError(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, `"error":"`); i >= 0 {
		msg = msg[i+9:]
		if j := strings.Index(msg, `"`); j >= 0 {
			msg = msg[:j]
		}
	}
	return msg
}

func (s *Server) library() (ModelLibrary, bool) {
	lib, ok := s.cluster.(ModelLibrary)
	return lib, ok && s.worker == nil
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	lib, ok := s.library()
	if !ok {
		writeError(w, http.StatusNotFound, "the model library is on the group host")
		return
	}
	entries, err := model.List(lib.ModelDir())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	models := []ModelInfo{}
	for _, e := range entries {
		info := ModelInfo{Name: e.Name, Path: e.Path, SizeBytes: e.SizeBytes}
		if _, err := os.Stat(e.Path); err != nil {
			info.Missing = true
		}
		models = append(models, info)
	}
	dirs := []string{}
	if c, err := config.Load(lib.ConfigDir()); err == nil && c.ModelDirs != nil {
		dirs = c.ModelDirs
	}
	s.lib.mu.Lock()
	jobs := []addJob{}
	for _, j := range s.lib.jobs {
		jobs = append(jobs, *j)
	}
	s.lib.mu.Unlock()
	writeJSON(w, map[string]any{"models": models, "dirs": dirs, "jobs": jobs})
}

// handleModelScan lists the .gguf files in a folder and, if asked,
// remembers the folder.
func (s *Server) handleModelScan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Dir      string `json:"dir"`
		Remember bool   `json:"remember"`
	}
	if !decode(w, r, 8192, &req) {
		return
	}
	lib, ok := s.library()
	if !ok {
		writeError(w, http.StatusNotFound, "the model library is on the group host")
		return
	}
	dir := strings.TrimSpace(req.Dir)
	if strings.HasPrefix(dir, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, strings.TrimPrefix(dir, "~"))
		}
	}
	if !filepath.IsAbs(dir) {
		writeError(w, http.StatusBadRequest, "give the full path of the folder, for example /media/you/disk/models")
		return
	}
	found, err := model.Scan(dir)
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot read that folder: "+err.Error())
		return
	}
	entries, _ := model.List(lib.ModelDir())
	registered := map[string]string{}
	for _, e := range entries {
		registered[e.Path] = e.Name
	}
	type row struct {
		model.Found
		Registered string `json:"registered,omitempty"`
	}
	rows := []row{}
	for _, f := range found {
		real := f.Path
		if p, err := filepath.EvalSymlinks(f.Path); err == nil {
			real = p
		}
		rows = append(rows, row{Found: f, Registered: registered[real]})
	}
	if req.Remember {
		abs := filepath.Clean(dir)
		if _, err := config.Update(lib.ConfigDir(), func(c *config.Config) error {
			for _, d := range c.ModelDirs {
				if d == abs {
					return nil
				}
			}
			if len(c.ModelDirs) >= 32 {
				return errors.New("at most 32 model folders")
			}
			c.ModelDirs = append(c.ModelDirs, abs)
			sort.Strings(c.ModelDirs)
			return nil
		}); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, map[string]any{"dir": dir, "files": rows})
}

// handleModelAdd registers a model in the background.
func (s *Server) handleModelAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	if !decode(w, r, 8192, &req) {
		return
	}
	lib, ok := s.library()
	if !ok {
		writeError(w, http.StatusNotFound, "the model library is on the group host")
		return
	}
	if !model.ValidName(req.Name) {
		writeError(w, http.StatusBadRequest, "model names use letters, digits, dot, dash and underscore (up to 64)")
		return
	}
	if !filepath.IsAbs(req.Path) {
		writeError(w, http.StatusBadRequest, "model path must be absolute")
		return
	}
	if _, err := model.Get(lib.ModelDir(), req.Name); err == nil {
		writeError(w, http.StatusConflict, "a model named "+req.Name+" is already registered")
		return
	}
	s.lib.mu.Lock()
	for _, j := range s.lib.jobs {
		if j.State == "hashing" && (j.Name == req.Name || j.Path == req.Path) {
			s.lib.mu.Unlock()
			writeError(w, http.StatusConflict, "that model is already being registered")
			return
		}
	}
	job := &addJob{Name: req.Name, Path: req.Path, State: "hashing", Started: time.Now().UTC()}
	s.lib.jobs = append(s.lib.jobs, job)
	if len(s.lib.jobs) > 20 {
		s.lib.jobs = s.lib.jobs[len(s.lib.jobs)-20:]
	}
	snapshot := *job
	s.lib.mu.Unlock()
	go func() {
		_, err := model.Add(lib.ModelDir(), req.Name, req.Path)
		s.lib.mu.Lock()
		defer s.lib.mu.Unlock()
		if err != nil {
			job.State, job.Error = "failed", err.Error()
		} else {
			job.State = "done"
		}
	}()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(snapshot)
}

// handleModelRemove forgets a registered model (the file is not touched).
func (s *Server) handleModelRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, 1024, &req) {
		return
	}
	lib, ok := s.library()
	if !ok {
		writeError(w, http.StatusNotFound, "the model library is on the group host")
		return
	}
	if !model.ValidName(req.Name) {
		writeError(w, http.StatusBadRequest, "invalid model name")
		return
	}
	if pm, ok := s.cluster.(PoolManager); ok {
		if st, err := pm.Pools(r.Context()); err == nil {
			for _, p := range st.Pools {
				if p.Model == req.Name {
					writeError(w, http.StatusConflict, "pool "+p.Name+" uses this model; remove or change that pool first")
					return
				}
			}
		}
	}
	if err := os.Remove(filepath.Join(lib.ModelDir(), req.Name+".json")); err != nil {
		writeError(w, http.StatusNotFound, "no model named "+req.Name)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
