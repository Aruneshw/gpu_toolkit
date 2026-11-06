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
	"strconv"
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
	u, e := url.Parse(raw)
	if e != nil {
		return errors.New("invalid endpoint")
	}
	scheme := "https"
	if backend {
		scheme = "http"
	}
	if u.Scheme != scheme || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
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
	if !privateIP(host) {
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
	if e := Endpoint("https://"+c.Listen, false); e != nil {
		return fmt.Errorf("listen: %w", e)
	}
	if e := Endpoint(c.Advertise, false); e != nil {
		return fmt.Errorf("advertise: %w", e)
	}
	u, _ := url.Parse(c.Advertise)
	if u.Host != c.Listen {
		return errors.New("advertise must match listen address (use an explicit VPN IP)")
	}
	if e := Endpoint(c.BackendURL, true); e != nil {
		return e
	}
	seen := map[string]bool{}
	for _, p := range c.Peers {
		if p.Name == "" || seen[p.Name] {
			return errors.New("peer names must be nonempty and unique")
		}
		seen[p.Name] = true
		if e := Endpoint(p.Endpoint, false); e != nil {
			return e
		}
	}
	return nil
}
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
