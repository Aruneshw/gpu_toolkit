// Package config owns validated, user-scoped, non-secret configuration.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

const Version = 1

type Peer struct {
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
}
type Config struct {
	Version    int    `json:"version"`
	Listen     string `json:"listen"`
	Advertise  string `json:"advertise"`
	BackendURL string `json:"backend_url"`
	Peers      []Peer `json:"peers"`
	GroupHost  bool   `json:"group_host,omitempty"`
	Public     bool   `json:"public,omitempty"`
	// Relay is the public TCP relay a group host is reachable through
	// (e.g. "bore.pub"); RelayPort is the public port it holds there.
	Relay     string `json:"relay,omitempty"`
	RelayPort int    `json:"relay_port,omitempty"`
	// Mesh names the VPN interface (Tailscale, WireGuard, ...) a group host
	// listens on; informational.
	Mesh string `json:"mesh,omitempty"`
	// MeshPaths are mesh-VPN addresses (IP:port) a group host also listens
	// on, so workers on the same mesh skip the relay. host sets them.
	MeshPaths []string `json:"mesh_paths,omitempty"`
	// AllowTerminal lets the group host open a shell on this worker. It is an
	// explicit choice of the worker's owner and is off by default.
	AllowTerminal bool `json:"allow_terminal,omitempty"`
	// TaskApproval decides what happens when the group host sends a task
	// (a command to run here): "ask" (empty) waits for the owner's approval
	// on this computer's dashboard, "auto" runs it, "off" refuses it.
	TaskApproval string `json:"task_approval,omitempty"`
	// ShareGPU "off" stops this worker from lending its GPU to the host's
	// pooled model through llama.cpp RPC; empty means share when possible.
	ShareGPU string `json:"share_gpu,omitempty"`
	// RPCServer and LlamaServer are optional paths to the llama.cpp
	// rpc-server (workers) and llama-server (host) binaries.
	RPCServer   string `json:"rpc_server,omitempty"`
	LlamaServer string `json:"llama_server,omitempty"`
	// PoolModel is the registered model a group host serves across every
	// shared GPU; PoolContext is its context size.
	PoolModel   string `json:"pool_model,omitempty"`
	PoolContext int    `json:"pool_context,omitempty"`
	// Pools are the host's pooled models; each runs its own llama-server
	// over its own workers. PoolModel/PoolContext above are the older
	// single-pool form and are read as a pool named "main".
	Pools []PoolConfig `json:"pools,omitempty"`
	// ModelDirs are folders (for example on an external disk) whose .gguf
	// files the dashboard lists for registration.
	ModelDirs []string `json:"model_dirs,omitempty"`
}

// PoolConfig is one pooled model and how it is spread.
type PoolConfig struct {
	// Name identifies the pool: lowercase letters, digits and dashes.
	Name  string `json:"name"`
	Model string `json:"model"`
	// Enabled pools are kept running; a disabled one keeps its settings.
	Enabled bool `json:"enabled"`
	Context int  `json:"context,omitempty"`
	// Parallel is the number of requests answered at once (llama.cpp
	// slots); the context is shared between them. 0 means 1.
	Parallel int `json:"parallel,omitempty"`
	// Workers lists the workers whose GPUs this pool uses. Empty: every
	// sharing worker no other pool claimed (the first such pool only).
	Workers []string `json:"workers,omitempty"`
	// NoHostGPU keeps the model off the host's own GPU(s).
	NoHostGPU bool `json:"no_host_gpu,omitempty"`
	// Split gives relative weights per computer ("local" is the host); a
	// computer not listed gets 1, 0 gives it no layers. Empty: llama.cpp
	// divides by free memory.
	Split map[string]float64 `json:"split,omitempty"`
	// CacheTypeK and CacheTypeV are the KV cache types (f16 when empty).
	CacheTypeK string `json:"cache_type_k,omitempty"`
	CacheTypeV string `json:"cache_type_v,omitempty"`
	// KVOnHost keeps the KV cache in the host's RAM instead of on the GPUs.
	KVOnHost bool `json:"kv_on_host,omitempty"`
	// FlashAttn is auto (default), on or off.
	FlashAttn string `json:"flash_attn,omitempty"`
	// OutputLayer is where the output layer (the logits) runs: "host" keeps
	// it on the host (its GPU, else its CPU) so each token sends the host a
	// few KB instead of the whole vocabulary's logits; "worker" leaves
	// llama.cpp's choice (the last device); "" (auto) means host when the
	// host lends its own GPU to the pool, else worker.
	OutputLayer string `json:"output_layer,omitempty"`
}

var poolName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// CacheTypes are the KV cache types llama.cpp accepts.
var CacheTypes = []string{"f32", "f16", "bf16", "q8_0", "q4_0", "q4_1", "iq4_nl", "q5_0", "q5_1"}

// Validate checks one pool's settings.
func (p PoolConfig) Validate() error {
	if !poolName.MatchString(p.Name) {
		return errors.New("pool name: 1-32 lowercase letters, digits or dashes")
	}
	if p.Model == "" || len(p.Model) > 64 {
		return errors.New("pool model required")
	}
	if p.Context != 0 && (p.Context < 256 || p.Context > 1048576) {
		return errors.New("context must be between 256 and 1048576")
	}
	if p.Parallel < 0 || p.Parallel > 64 {
		return errors.New("parallel must be between 1 and 64")
	}
	if p.Parallel > 1 && p.Context != 0 && p.Context/p.Parallel < 128 {
		return errors.New("each parallel slot needs at least 128 tokens of context")
	}
	if len(p.Workers) > 64 || len(p.Split) > 65 {
		return errors.New("too many workers")
	}
	seen := map[string]bool{}
	for _, w := range p.Workers {
		if !strings.HasPrefix(w, "worker-") || len(w) > 100 || seen[w] {
			return errors.New("pool workers must be distinct worker IDs")
		}
		seen[w] = true
	}
	positive := false
	for node, wgt := range p.Split {
		if node != "local" && (!strings.HasPrefix(node, "worker-") || len(node) > 100) {
			return errors.New("split keys are local or worker IDs")
		}
		if wgt < 0 || wgt > 1000 || wgt != wgt {
			return errors.New("split weights must be between 0 and 1000")
		}
		positive = positive || wgt > 0
	}
	if len(p.Split) > 0 && !positive {
		return errors.New("at least one split weight must be above 0")
	}
	for _, t := range []string{p.CacheTypeK, p.CacheTypeV} {
		if t == "" {
			continue
		}
		ok := false
		for _, c := range CacheTypes {
			ok = ok || t == c
		}
		if !ok {
			return errors.New("unknown KV cache type " + t)
		}
	}
	switch p.FlashAttn {
	case "", "auto", "on", "off":
	default:
		return errors.New("flash_attn must be auto, on or off")
	}
	switch p.OutputLayer {
	case "", "auto", "host", "worker":
	default:
		return errors.New("output_layer must be auto, host or worker")
	}
	return nil
}

// PoolList returns the configured pools, reading the older single-pool
// fields as a pool named "main".
func (c Config) PoolList() []PoolConfig {
	if len(c.Pools) > 0 {
		return append([]PoolConfig(nil), c.Pools...)
	}
	if c.PoolModel != "" {
		return []PoolConfig{{Name: "main", Model: c.PoolModel, Context: c.PoolContext, Enabled: true}}
	}
	return nil
}

func Default() Config {
	return Config{Version: Version, Listen: "127.0.0.1:7443", Advertise: "https://127.0.0.1:7443", BackendURL: "http://127.0.0.1:8080", Peers: []Peer{}}
}
func Dir() (string, error) {
	if d := os.Getenv("MESHCOMPUTE_HOME"); d != "" {
		return filepath.Abs(d)
	}
	d, e := os.UserConfigDir()
	if e != nil {
		return "", e
	}
	return filepath.Join(d, "meshcompute"), nil
}
func privateIP(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || net.ParseIP("100.64.0.0").To4() != nil && inCGNAT(ip))
}
func inCGNAT(ip net.IP) bool { _, n, _ := net.ParseCIDR("100.64.0.0/10"); return n.Contains(ip) }
func Endpoint(raw string, backend bool) error {
	return endpoint(raw, backend, false)
}

// RemoteEndpoint allows explicit public IPs only for opt-in group hosting.
func RemoteEndpoint(raw string, public bool) error {
	return endpoint(raw, false, public)
}

func endpoint(raw string, backend, public bool) error {
	u, e := url.Parse(raw)
	if e != nil {
		return errors.New("invalid endpoint")
	}
	scheme := "https"
	if backend {
		scheme = "http"
	}
	if u.Scheme != scheme || u.Opaque != "" || u.User != nil || u.ForceQuery || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("endpoint must be %s://IP:port without credentials or path", scheme)
	}
	host, port, e := net.SplitHostPort(u.Host)
	if e != nil {
		return errors.New("endpoint needs literal IP and port")
	}
	n, e := strconv.Atoi(port)
	if e != nil || n < 1 || n > 65535 {
		return errors.New("invalid port")
	}
	ip := net.ParseIP(host)
	if ip == nil || (!privateIP(host) && !(public && ip.IsGlobalUnicast())) {
		return errors.New("endpoint must use a private, VPN or loopback IP")
	}
	if backend && !net.ParseIP(host).IsLoopback() {
		return errors.New("backend must bind loopback; remote requests use authenticated agents")
	}
	return nil
}
func (c Config) Validate() error {
	if c.Version != Version {
		return errors.New("unsupported configuration version")
	}
	if c.Public && !c.GroupHost {
		return errors.New("public listening requires group_host")
	}
	if len(c.MeshPaths) > 8 {
		return errors.New("at most 8 mesh_paths")
	}
	for _, p := range c.MeshPaths {
		host, port, err := net.SplitHostPort(p)
		if err != nil || net.ParseIP(host) == nil || port == "" || port == "0" {
			return fmt.Errorf("mesh_paths: %q is not IP:port", p)
		}
	}
	if e := RemoteEndpoint("https://"+c.Listen, c.Public); e != nil {
		return fmt.Errorf("listen: %w", e)
	}
	if e := RemoteEndpoint(c.Advertise, c.Public); e != nil {
		return fmt.Errorf("advertise: %w", e)
	}
	u, _ := url.Parse(c.Advertise)
	if !c.GroupHost && u.Host != c.Listen {
		return errors.New("advertise must match listen address (use an explicit VPN IP)")
	}
	if e := Endpoint(c.BackendURL, true); e != nil {
		return e
	}
	switch c.TaskApproval {
	case "", "ask", "auto", "off":
	default:
		return errors.New("task_approval must be ask, auto or off")
	}
	if c.ShareGPU != "" && c.ShareGPU != "off" {
		return errors.New("share_gpu must be empty or off")
	}
	if c.PoolContext < 0 || c.PoolContext > 1048576 {
		return errors.New("invalid pool context")
	}
	if len(c.Pools) > 16 || len(c.ModelDirs) > 32 {
		return errors.New("too many pools or model folders")
	}
	names := map[string]bool{}
	claimed := map[string]string{}
	for _, p := range c.Pools {
		if err := p.Validate(); err != nil {
			return fmt.Errorf("pool %q: %w", p.Name, err)
		}
		if names[p.Name] {
			return fmt.Errorf("two pools are named %q", p.Name)
		}
		names[p.Name] = true
		for _, w := range p.Workers {
			if other, ok := claimed[w]; ok {
				return fmt.Errorf("worker %s is in pools %q and %q; a worker lends its GPU to one pool", w[:min(len(w), 15)], other, p.Name)
			}
			claimed[w] = p.Name
		}
	}
	for _, d := range c.ModelDirs {
		if !filepath.IsAbs(d) || len(d) > 4096 {
			return errors.New("model folders must be absolute paths")
		}
	}
	seen := map[string]bool{}
	for _, p := range c.Peers {
		if p.Name == "" || p.Name == "local" || seen[p.Name] {
			return errors.New("peer names must be nonempty and unique")
		}
		seen[p.Name] = true
		if e := Endpoint(p.Endpoint, false); e != nil {
			return e
		}
	}
	return nil
}

// LocalEndpoint is reachable without hairpin NAT when advertise uses a
// forwarded address. Group certificates contain both listen and advertise IPs.
func (c Config) LocalEndpoint() string { return "https://" + c.Listen }
func Load(dir string) (Config, error) {
	var c Config
	f, e := os.Open(filepath.Join(dir, "config.json"))
	if e != nil {
		return c, fmt.Errorf("configuration unavailable; run meshcompute install: %w", e)
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if e = d.Decode(&c); e != nil {
		return c, e
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return c, errors.New("trailing configuration content")
	}
	return c, c.Validate()
}
func Save(dir string, c Config) error {
	if e := c.Validate(); e != nil {
		return e
	}
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	b, e := json.MarshalIndent(c, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(dir, ".config-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(append(b, '\n'))
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	return os.Rename(f.Name(), filepath.Join(dir, "config.json"))
}
func Install(dir string) error {
	if _, e := os.Stat(filepath.Join(dir, "config.json")); e == nil {
		_, e = Load(dir)
		return e
	} else if !os.IsNotExist(e) {
		return e
	}
	return Save(dir, Default())
}

var updateMu sync.Mutex

// Update loads the configuration in dir, applies change and saves it, under
// a lock shared by everything in this process (the agent saving pools and
// the dashboard saving model folders must not overwrite each other).
func Update(dir string, change func(*Config) error) (Config, error) {
	updateMu.Lock()
	defer updateMu.Unlock()
	c, err := Load(dir)
	if err != nil {
		return c, err
	}
	if err := change(&c); err != nil {
		return c, err
	}
	return c, Save(dir, c)
}
