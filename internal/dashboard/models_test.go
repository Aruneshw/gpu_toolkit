package dashboard

import (
	"context"
	"encoding/json"
	"meshcompute/internal/agent"
	"meshcompute/internal/config"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type libraryCluster struct {
	fakeCluster
	dir   string
	pools []config.PoolConfig
}

func (l *libraryCluster) ModelDir() string  { return filepath.Join(l.dir, "models") }
func (l *libraryCluster) ConfigDir() string { return l.dir }
func (l *libraryCluster) Pools(context.Context) (agent.PoolsStatus, error) {
	st := agent.PoolsStatus{Pools: []agent.PoolStatus{}, Sharing: []string{"worker-a"}}
	for _, p := range l.pools {
		st.Pools = append(st.Pools, agent.PoolStatus{Name: p.Name, Model: p.Model, Settings: p})
	}
	return st, nil
}
func (l *libraryCluster) ChangePools(ctx context.Context, action string, p config.PoolConfig, name string) (agent.PoolsStatus, error) {
	if action == "set" {
		if err := p.Validate(); err != nil {
			return agent.PoolsStatus{}, err
		}
		l.pools = append(l.pools, p)
	}
	return l.Pools(ctx)
}

func call(t *testing.T, s *Server, method, path string, body any, out any) int {
	t.Helper()
	var rd *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	} else {
		rd = strings.NewReader("")
	}
	rec := request(s, method, path, rd, auth)
	if out != nil {
		_ = json.Unmarshal(rec.Body.Bytes(), out)
	}
	return rec.Code
}

func TestModelLibrary(t *testing.T) {
	dir := t.TempDir()
	if err := config.Save(dir, config.Default()); err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(t.TempDir(), "hdd")
	os.MkdirAll(filepath.Join(disk, "llm"), 0700)
	file := filepath.Join(disk, "llm", "Tiny-Model.Q8_0.gguf")
	if err := os.WriteFile(file, append([]byte("GGUF"), make([]byte, 4096)...), 0600); err != nil {
		t.Fatal(err)
	}
	cl := &libraryCluster{dir: dir}
	s := newServer(t, cl)

	var scan struct {
		Files []struct {
			Path, Suggested, Registered string
			SizeBytes                   int64 `json:"size_bytes"`
		} `json:"files"`
	}
	if code := call(t, s, "POST", "/api/models/scan", map[string]any{"dir": disk, "remember": true}, &scan); code != 200 || len(scan.Files) != 1 || scan.Files[0].Suggested != "tiny-model.q8_0" || scan.Files[0].SizeBytes != 4100 {
		t.Fatalf("scan: %d %+v", code, scan)
	}
	if c, _ := config.Load(dir); len(c.ModelDirs) != 1 || c.ModelDirs[0] != disk {
		t.Fatalf("folder not remembered: %+v", c.ModelDirs)
	}
	if code := call(t, s, "POST", "/api/models/scan", map[string]any{"dir": "relative/path"}, nil); code != http.StatusBadRequest {
		t.Fatalf("relative folder: %d", code)
	}
	if code := call(t, s, "POST", "/api/models/add", map[string]any{"name": "bad name!", "path": file}, nil); code != http.StatusBadRequest {
		t.Fatalf("bad name: %d", code)
	}
	if code := call(t, s, "POST", "/api/models/add", map[string]any{"name": "tiny", "path": file}, nil); code != http.StatusAccepted {
		t.Fatalf("add: %d", code)
	}
	var lib struct {
		Models []ModelInfo `json:"models"`
		Dirs   []string    `json:"dirs"`
		Jobs   []addJob    `json:"jobs"`
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		call(t, s, "GET", "/api/models", nil, &lib)
		if len(lib.Models) == 1 && len(lib.Jobs) == 1 && lib.Jobs[0].State == "done" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("registration never finished: %+v", lib)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if lib.Models[0].Name != "tiny" || lib.Models[0].Missing || len(lib.Dirs) != 1 {
		t.Fatalf("%+v", lib)
	}
	call(t, s, "POST", "/api/models/scan", map[string]any{"dir": disk}, &scan)
	if scan.Files[0].Registered != "tiny" {
		t.Fatalf("scan must show the registered name: %+v", scan.Files[0])
	}
	if code := call(t, s, "POST", "/api/models/add", map[string]any{"name": "tiny", "path": file}, nil); code != http.StatusConflict {
		t.Fatalf("duplicate: %d", code)
	}
	// A failing registration is reported, not hidden.
	if code := call(t, s, "POST", "/api/models/add", map[string]any{"name": "broken", "path": filepath.Join(disk, "nope.gguf")}, nil); code != http.StatusAccepted {
		t.Fatalf("add broken: %d", code)
	}
	deadline = time.Now().Add(10 * time.Second)
	for {
		call(t, s, "GET", "/api/models", nil, &lib)
		if len(lib.Jobs) == 2 && lib.Jobs[1].State == "failed" && lib.Jobs[1].Error != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("failed registration not reported: %+v", lib.Jobs)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The disk is unplugged.
	os.Rename(disk, disk+"-gone")
	call(t, s, "GET", "/api/models", nil, &lib)
	if !lib.Models[0].Missing {
		t.Fatal("a missing model file must be flagged")
	}
	// A model a pool uses cannot be removed.
	if code := call(t, s, "POST", "/api/pools", map[string]any{"action": "set", "pool": config.PoolConfig{Name: "p", Model: "tiny"}}, nil); code != 200 {
		t.Fatalf("pool set: %d", code)
	}
	if code := call(t, s, "POST", "/api/models/remove", map[string]any{"name": "tiny"}, nil); code != http.StatusConflict {
		t.Fatalf("removed a model in use: %d", code)
	}
	cl.pools = nil
	if code := call(t, s, "POST", "/api/models/remove", map[string]any{"name": "tiny"}, nil); code != http.StatusNoContent {
		t.Fatalf("remove: %d", code)
	}
	if code := call(t, s, "POST", "/api/pools", map[string]any{"action": "set", "pool": config.PoolConfig{Name: "Bad Name", Model: "m"}}, nil); code != http.StatusBadRequest {
		t.Fatalf("invalid pool accepted: %d", code)
	}
	var pools agent.PoolsStatus
	if code := call(t, s, "GET", "/api/pools", nil, &pools); code != 200 || len(pools.Sharing) != 1 {
		t.Fatalf("pools: %d %+v", code, pools)
	}
}

func TestModelLibraryNotOnWorkerOrPlainDashboards(t *testing.T) {
	s := newServer(t, &fakeCluster{})
	for _, path := range []string{"/api/models", "/api/pools"} {
		if code := call(t, s, "GET", path, nil, nil); code != http.StatusNotFound {
			t.Fatalf("%s on a plain dashboard: %d", path, code)
		}
	}
}
