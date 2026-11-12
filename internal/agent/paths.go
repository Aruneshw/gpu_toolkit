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
	"sync/atomic"
	"time"
)

// PathsHeader lists, in the host's answer to a reverse connection, other
// addresses of the host (mesh VPN) a worker may connect to instead of the
// one in its invitation. Workers from older releases ignore it.
const PathsHeader = "Meshcompute-Paths"

const (
	maxPaths     = 8
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

// HopsHeader lists, in the host's answer to a reverse connection, other
// workers that forward connections to the host ("https://IP:port worker-ID",
// comma-separated). HopHeader in a worker's request names the worker whose
// forwarder it came through.
const (
	HopsHeader = "Meshcompute-Hops"
	HopHeader  = "Meshcompute-Hop"
	maxHops    = 6
)

// hop is another worker's forwarder to the host.
type hop struct {
	endpoint, via string
}

// parseHops keeps the well-formed "https://IP:port worker-ID" entries.
func parseHops(header string) []hop {
	var out []hop
	for _, entry := range strings.Split(header, ",") {
		fields := strings.Fields(entry)
		if len(fields) != 2 || !validWorkerID(fields[1]) {
			continue
		}
		if p := parsePaths(fields[0]); len(p) == 1 {
			out = append(out, hop{p[0], fields[1]})
		}
		if len(out) == maxHops {
			break
		}
	}
	return out
}

func validWorkerID(id string) bool {
	if len(id) != 71 || !strings.HasPrefix(id, "worker-") {
		return false
	}
	for _, c := range id[7:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// pathFilter lets tests hide some offered paths from one worker (nil: keep
// every path).
var pathFilter atomic.Pointer[func(dir, endpoint string) bool]

func keepPath(dir, endpoint string) bool {
	if f := pathFilter.Load(); f != nil {
		return (*f)(dir, endpoint)
	}
	return true
}

// hostPaths chooses which address of the group host a worker connects to:
// the invitation's (often the public relay), a faster mesh address the host
// offered, or another worker's forwarder (a hop). The host's identity is
// checked the same way on every path (pinned certificate; the TLS server
// name stays the invitation's host), so a forwarding worker only ever sees
// encrypted bytes.
type hostPaths struct {
	primary string
	dir     string
	mu      sync.Mutex
	offered []string
	hops    []hop
	chosen  string // "" means primary
	via     string // the forwarding worker when chosen is a hop
	direct  string // best path that is not a hop ("" means primary)
	fails   int
	bad     map[string]time.Time
	kick    chan struct{}
	note    func(string)
}

func newHostPaths(dir, primary string, note func(string)) *hostPaths {
	return &hostPaths{primary: primary, dir: dir, bad: map[string]time.Time{}, kick: make(chan struct{}, 1), note: note}
}

// current returns the endpoint to connect to, its TLS settings, and the
// forwarding worker when it is a hop.
func (h *hostPaths) current(base *tls.Config) (string, *tls.Config, string) {
	h.mu.Lock()
	chosen, via := h.chosen, h.via
	h.mu.Unlock()
	if chosen == "" {
		return h.primary, base, ""
	}
	return chosen, h.config(base), via
}

// directEndpoint is where this worker's own forwarder sends other workers'
// connections: its best path that is not itself a hop, so a connection
// never passes through more than one worker.
func (h *hostPaths) directEndpoint() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.direct != "" {
		return h.direct
	}
	return h.primary
}

// config is base with the invitation's host as TLS server name, so the
// host certificate verifies on another address.
func (h *hostPaths) config(base *tls.Config) *tls.Config {
	u, err := url.Parse(h.primary)
	if err != nil || base == nil {
		return base
	}
	c := base.Clone()
	c.ServerName = u.Hostname()
	return c
}

// offer records the paths and hops the host just listed; new ones are
// probed soon.
func (h *hostPaths) offer(pathsHeader, hopsHeader string) {
	paths, hops := parsePaths(pathsHeader), parseHops(hopsHeader)
	key := func(p []string, hs []hop) string {
		k := strings.Join(p, ",")
		for _, x := range hs {
			k += "|" + x.endpoint + " " + x.via
		}
		return k
	}
	h.mu.Lock()
	changed := key(paths, hops) != key(h.offered, h.hops)
	h.offered, h.hops = paths, hops
	h.mu.Unlock()
	if changed && (len(paths) > 0 || len(hops) > 0) {
		select {
		case h.kick <- struct{}{}:
		default:
		}
	}
}

// result records how a connection on endpoint went; repeated failures on a
// mesh path or hop fall back to the best direct path, then the invitation's.
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
	if h.fails < pathFailures {
		return
	}
	h.bad[h.chosen] = time.Now().Add(pathBadFor)
	next := h.direct
	if next == h.chosen {
		next, h.direct = "", ""
	}
	what := "direct path " + hostOf(h.chosen)
	if h.via != "" {
		what = "path through worker " + short(h.via)
	}
	back := hostOf(h.primary)
	if next != "" {
		back = hostOf(next)
	}
	h.note(fmt.Sprintf("The %s stopped working; using %s again.", what, back))
	h.chosen, h.via, h.fails = next, "", 0
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

// probe times a TLS connection over every path and picks one. Among direct
// paths a mesh path wins unless the invitation's address is clearly faster
// (it skips the relay's detour and capacity limits); a hop through another
// worker wins only when it is clearly faster than every direct path, or
// when none works.
func (h *hostPaths) probe(ctx context.Context) {
	base, err := security.WorkerTLS(h.dir)
	if err != nil {
		return
	}
	h.mu.Lock()
	for p, until := range h.bad {
		if time.Now().After(until) {
			delete(h.bad, p)
		}
	}
	usable := func(p string) bool {
		_, bad := h.bad[p]
		return !bad && p != h.primary && keepPath(h.dir, p)
	}
	var mesh []string
	for _, p := range h.offered {
		if usable(p) {
			mesh = append(mesh, p)
		}
	}
	var hops []hop
	for _, x := range h.hops {
		if usable(x.endpoint) {
			hops = append(hops, x)
		}
	}
	previous, previousVia := h.chosen, h.via
	h.mu.Unlock()
	if len(mesh) == 0 && len(hops) == 0 {
		h.mu.Lock()
		if h.chosen != "" {
			h.chosen, h.via, h.direct = "", "", ""
		}
		h.mu.Unlock()
		return
	}
	primaryTime, primaryOK := timeTLS(ctx, h.primary, base)
	bestMesh, meshTime := "", time.Duration(0)
	for _, p := range mesh {
		if d, ok := timeTLS(ctx, p, h.config(base)); ok && (bestMesh == "" || d < meshTime) {
			bestMesh, meshTime = p, d
		}
	}
	direct, directTime, directOK := "", primaryTime, primaryOK
	if bestMesh != "" && (!primaryOK || meshTime <= primaryTime*6/5+2*time.Millisecond) {
		direct, directTime, directOK = bestMesh, meshTime, true
	}
	var bestHop hop
	hopTime := time.Duration(0)
	for _, x := range hops {
		if d, ok := timeTLS(ctx, x.endpoint, h.config(base)); ok && (bestHop.endpoint == "" || d < hopTime) {
			bestHop, hopTime = x, d
		}
	}
	choice, via := direct, ""
	if bestHop.endpoint != "" && (!directOK || hopClearlyFaster(hopTime, directTime)) {
		choice, via = bestHop.endpoint, bestHop.via
	}
	h.mu.Lock()
	h.chosen, h.via, h.direct = choice, via, direct
	if choice != previous {
		h.fails = 0
	}
	h.mu.Unlock()
	if choice == previous && via == previousVia {
		return
	}
	switch {
	case via != "":
		h.note(fmt.Sprintf("Reaching the group host through worker %s (%s per connection instead of %s directly).", short(via), roundMs(hopTime), roundMs(directTime)))
	case choice != "":
		h.note(fmt.Sprintf("Using the direct path %s to the group host (%s per connection instead of %s through %s).", hostOf(choice), roundMs(meshTime), roundMs(primaryTime), hostOf(h.primary)))
	default:
		h.note(fmt.Sprintf("Using %s to reach the group host.", hostOf(h.primary)))
	}
}

// hopClearlyFaster decides whether a path through another worker is worth
// it: at least twice as fast and 10 ms faster than the best direct path.
// A hop adds a computer that can go away, and on a busy machine handshake
// times jitter by milliseconds, so small gains are noise; the case it is
// for is a relay at tens or hundreds of ms against a LAN or mesh path.
func hopClearlyFaster(hop, direct time.Duration) bool {
	return hop*2 <= direct && direct-hop >= 10*time.Millisecond
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
