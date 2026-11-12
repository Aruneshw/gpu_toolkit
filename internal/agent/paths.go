package agent

import (
	"context"
	"crypto/tls"
	"fmt"
	"meshcompute/internal/security"
	"meshcompute/internal/tunnel"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// PathsHeader lists, in the host's answer to a reverse connection, other
// addresses of the host (mesh VPN) a worker may connect to instead of the
// one in its invitation. Workers from older releases ignore it.
const PathsHeader = "Meshcompute-Paths"

const (
	maxPaths     = 4
	pathProbe    = time.Minute     // how often a worker re-checks its paths
	pathDial     = 3 * time.Second // per connection attempt while probing
	pathFailures = 2               // failed connections before leaving a path
	pathBadFor   = 5 * time.Minute // a path that failed is not tried again for this long
)

// serveMeshPaths makes the group host also listen on its mesh-VPN addresses
// (config mesh_paths), with the same TLS server and limits as the main
// listener, and remembers which of them work for PathsHeader.
func (a *Agent) serveMeshPaths(ctx context.Context, server *http.Server, workers *sync.WaitGroup) {
	for _, addr := range a.c.MeshPaths {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			a.log.Printf("event=mesh_path state=unavailable addr=%s", addr)
			continue
		}
		a.meshPaths = append(a.meshPaths, "https://"+l.Addr().String())
		a.log.Printf("event=mesh_path state=listening addr=%s", l.Addr())
		limited := tunnel.LimitListener(l, 1024)
		workers.Add(1)
		go func() {
			defer workers.Done()
			stop := context.AfterFunc(ctx, func() { limited.Close() })
			defer stop()
			_ = server.ServeTLS(limited, "", "")
		}()
	}
}

// pathsHeader is the header line offering the host's mesh paths, or "".
func (a *Agent) pathsHeader() string {
	if len(a.meshPaths) == 0 {
		return ""
	}
	return PathsHeader + ": " + strings.Join(a.meshPaths, ", ") + "\r\n"
}

// parsePaths keeps the well-formed https://IP:port entries of a PathsHeader.
func parsePaths(header string) []string {
	var out []string
	for _, p := range strings.Split(header, ",") {
		p = strings.TrimSpace(p)
		u, err := url.Parse(p)
		if err != nil || u.Scheme != "https" || u.Path != "" || u.User != nil || u.RawQuery != "" || u.Port() == "" {
			continue
		}
		ip := net.ParseIP(u.Hostname())
		if ip == nil || ip.IsUnspecified() || ip.IsMulticast() {
			continue
		}
		out = append(out, "https://"+u.Host)
		if len(out) == maxPaths {
			break
		}
	}
	return out
}

// hostPaths chooses which address of the group host a worker connects to:
// the invitation's (often the public relay) or a faster mesh address the
// host offered. The host's identity is checked the same way on every path
// (pinned certificate; the TLS server name stays the invitation's host).
type hostPaths struct {
	primary string
	dir     string
	mu      sync.Mutex
	offered []string
	chosen  string // "" means primary
	fails   int
	bad     map[string]time.Time
	kick    chan struct{}
	note    func(string)
}

func newHostPaths(dir, primary string, note func(string)) *hostPaths {
	return &hostPaths{primary: primary, dir: dir, bad: map[string]time.Time{}, kick: make(chan struct{}, 1), note: note}
}

// current returns the endpoint to connect to and its TLS settings.
func (h *hostPaths) current(base *tls.Config) (string, *tls.Config) {
	h.mu.Lock()
	chosen := h.chosen
	h.mu.Unlock()
	if chosen == "" {
		return h.primary, base
	}
	return chosen, h.config(base)
}

// config is base with the invitation's host as TLS server name, so the
// host certificate verifies on another address.
func (h *hostPaths) config(base *tls.Config) *tls.Config {
	u, err := url.Parse(h.primary)
	if err != nil {
		return base
	}
	c := base.Clone()
	c.ServerName = u.Hostname()
	return c
}

// offer records the paths the host just listed; new ones are probed soon.
func (h *hostPaths) offer(header string) {
	paths := parsePaths(header)
	h.mu.Lock()
	changed := strings.Join(paths, ",") != strings.Join(h.offered, ",")
	h.offered = paths
	h.mu.Unlock()
	if changed && len(paths) > 0 {
		select {
		case h.kick <- struct{}{}:
		default:
		}
	}
}

// result records how a connection on endpoint went; repeated failures on a
// mesh path fall back to the invitation's address.
func (h *hostPaths) result(endpoint string, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if endpoint != h.chosen || h.chosen == "" {
		return
	}
	if ok {
		h.fails = 0
		return
	}
	h.fails++
	if h.fails >= pathFailures {
		h.bad[h.chosen] = time.Now().Add(pathBadFor)
		h.note(fmt.Sprintf("The direct path %s stopped working; using %s again.", hostOf(h.chosen), hostOf(h.primary)))
		h.chosen, h.fails = "", 0
	}
}

// run re-checks the paths every pathProbe, and soon after the host offers
// new ones.
func (h *hostPaths) run(ctx context.Context) {
	timer := time.NewTimer(pathProbe)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.kick:
		case <-timer.C:
		}
		h.probe(ctx)
		timer.Reset(pathProbe)
	}
}

// probe times a TLS connection to every path and picks the fastest one
// that works. A mesh path wins unless the invitation's address is clearly
// faster: it skips the relay's detour and the relay's capacity limits.
func (h *hostPaths) probe(ctx context.Context) {
	base, err := security.WorkerTLS(h.dir)
	if err != nil {
		return
	}
	h.mu.Lock()
	offered := append([]string{}, h.offered...)
	for p, until := range h.bad {
		if time.Now().After(until) {
			delete(h.bad, p)
		}
	}
	var candidates []string
	for _, p := range offered {
		if _, bad := h.bad[p]; !bad && p != h.primary {
			candidates = append(candidates, p)
		}
	}
	previous := h.chosen
	h.mu.Unlock()
	if len(candidates) == 0 {
		return
	}
	primaryTime, primaryOK := timeTLS(ctx, h.primary, base)
	best, bestTime := "", time.Duration(0)
	for _, p := range candidates {
		if d, ok := timeTLS(ctx, p, h.config(base)); ok && (best == "" || d < bestTime) {
			best, bestTime = p, d
		}
	}
	choice := ""
	if best != "" && (!primaryOK || bestTime <= primaryTime*6/5+2*time.Millisecond) {
		choice = best
	}
	h.mu.Lock()
	h.chosen = choice
	if choice != previous {
		h.fails = 0
	}
	h.mu.Unlock()
	if choice != previous {
		if choice != "" {
			h.note(fmt.Sprintf("Using the direct path %s to the group host (%s per connection instead of %s through %s).", hostOf(choice), roundMs(bestTime), roundMs(primaryTime), hostOf(h.primary)))
		} else {
			h.note(fmt.Sprintf("Using %s to reach the group host.", hostOf(h.primary)))
		}
	}
}

// timeTLS is the fastest of three pinned TLS handshakes to endpoint.
func timeTLS(ctx context.Context, endpoint string, cfg *tls.Config) (time.Duration, bool) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return 0, false
	}
	best, ok := time.Duration(0), false
	for i := 0; i < 3; i++ {
		dctx, cancel := context.WithTimeout(ctx, pathDial)
		start := time.Now()
		c, err := (&tls.Dialer{NetDialer: &net.Dialer{}, Config: cfg}).DialContext(dctx, "tcp", u.Host)
		d := time.Since(start)
		cancel()
		if err != nil {
			continue
		}
		c.Close()
		if !ok || d < best {
			best, ok = d, true
		}
	}
	return best, ok
}

func hostOf(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		return u.Host
	}
	return endpoint
}

func roundMs(d time.Duration) string {
	if d < 10*time.Millisecond {
		return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000)
	}
	return fmt.Sprintf("%d ms", d.Milliseconds())
}
