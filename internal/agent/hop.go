package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"meshcompute/internal/network"
	"meshcompute/internal/tunnel"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// A hop: a worker whose owner allowed it (allow_hop) listens on its LAN and
// mesh-VPN addresses and forwards other workers' connections to the group
// host over its own direct path. TLS runs end to end between the other
// worker and the host, so the hop only moves encrypted bytes; it forwards to
// the host and nowhere else.

const (
	maxHopConns = 64
	probePath   = "/v1/peers/probe"
	maxProbes   = 16
	probeDial   = 2 * time.Second
	probeEvery  = 5 * time.Second // a worker measures at most this often

	topoMeasure  = 30 * time.Second
	hopEndpoints = 2 // endpoints per hop worker offered or measured
)

// topoStatus is how often the host refreshes which workers forward, in
// nanoseconds (tests shorten it).
var topoStatus atomic.Int64

func topoEvery() time.Duration {
	if d := time.Duration(topoStatus.Load()); d > 0 {
		return d
	}
	return 15 * time.Second
}

// HopStatus is a worker's forwarder, as reported in its status.
type HopStatus struct {
	// Endpoints are the addresses other workers can connect to.
	Endpoints []string `json:"endpoints"`
	// Active connections being forwarded now, and all forwarded so far.
	Active int    `json:"active"`
	Served uint64 `json:"served"`
	// Bytes forwarded towards the host and back.
	BytesUp   uint64 `json:"bytes_up"`
	BytesDown uint64 `json:"bytes_down"`
}

type hopServer struct {
	mu        sync.Mutex
	endpoints []string
	active    int
	served    uint64
	meter     tunnel.Meter
	slots     chan struct{}
}

func (h *hopServer) status() *HopStatus {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.meter.Counts()
	return &HopStatus{Endpoints: append([]string{}, h.endpoints...), Active: h.active, Served: h.served, BytesUp: c.Up, BytesDown: c.Down}
}

// hopAddresses are this computer's private LAN IPv4 addresses and mesh-VPN
// addresses, where other workers might reach it.
func hopAddresses() []string {
	var out []string
	seen := map[string]bool{}
	add := func(ip string) {
		if !seen[ip] {
			seen[ip] = true
			out = append(out, ip)
		}
	}
	for _, m := range network.MeshAddresses() {
		add(m.IP)
	}
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || virtualBridge(iface.Name) {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && n.IP.IsPrivate() {
				add(n.IP.String())
			}
		}
	}
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}

// virtualBridge reports interfaces that only reach containers or virtual
// machines on this computer (Docker, libvirt, VirtualBox, VMware, WSL,
// Hyper-V), never another laptop.
func virtualBridge(name string) bool {
	n := strings.ToLower(name)
	for _, p := range []string{"docker", "br-", "veth", "virbr", "vboxnet", "vmnet", "lxcbr", "lxdbr", "cni", "flannel", "podman", "vethernet (wsl", "vethernet (default switch", "vmware", "virtualbox"} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// startHop opens the forwarder when the owner allowed it.
func (a *Agent) startHop(ctx context.Context) {
	a.mu.Lock()
	allowed, listen := a.c.AllowHop, append([]string{}, a.c.HopListen...)
	a.mu.Unlock()
	if !allowed {
		return
	}
	if len(listen) == 0 {
		for _, ip := range hopAddresses() {
			listen = append(listen, net.JoinHostPort(ip, "0"))
		}
	}
	h := &hopServer{slots: make(chan struct{}, maxHopConns)}
	port := ""
	for _, addr := range listen {
		host, p, _ := net.SplitHostPort(addr)
		if p == "0" && port != "" {
			p = port // the same port on every address when possible
		}
		l, err := net.Listen("tcp", net.JoinHostPort(host, p))
		if err != nil && p != "0" {
			l, err = net.Listen("tcp", net.JoinHostPort(host, "0"))
		}
		if err != nil {
			a.log.Printf("event=hop state=unavailable addr=%s", addr)
			continue
		}
		_, got, _ := net.SplitHostPort(l.Addr().String())
		if port == "" {
			port = got
		}
		h.endpoints = append(h.endpoints, "https://"+l.Addr().String())
		go a.serveHop(ctx, h, l)
	}
	if len(h.endpoints) == 0 {
		return
	}
	a.mu.Lock()
	a.hop = h
	a.mu.Unlock()
	a.log.Printf("event=hop state=listening endpoints=%q", strings.Join(h.endpoints, ","))
	a.say("Hop: ON. Other workers of this group may reach the host through this computer at %s; it forwards their encrypted connections and cannot read them. Turn it off with: meshcompute config set allow_hop false", strings.Join(h.endpoints, ", "))
}

func (a *Agent) serveHop(ctx context.Context, h *hopServer, l net.Listener) {
	stop := context.AfterFunc(ctx, func() { l.Close() })
	defer stop()
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		select {
		case h.slots <- struct{}{}:
		default:
			c.Close() // at capacity
			continue
		}
		go func() {
			defer func() { <-h.slots }()
			a.forwardHop(ctx, h, c)
		}()
	}
}

// forwardHop carries one connection to the host over this worker's own
// direct path.
func (a *Agent) forwardHop(ctx context.Context, h *hopServer, c net.Conn) {
	defer c.Close()
	paths := a.hostPaths
	if paths == nil {
		return
	}
	u, err := url.Parse(paths.directEndpoint())
	if err != nil {
		return
	}
	d := net.Dialer{Timeout: 5 * time.Second, KeepAlive: 15 * time.Second}
	up, err := d.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return
	}
	h.mu.Lock()
	h.active++
	h.served++
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.active--
		h.mu.Unlock()
	}()
	stop := context.AfterFunc(ctx, func() { c.Close(); up.Close() })
	defer stop()
	tunnel.MeteredPipe(c, up, &h.meter)
}

// probeRequest asks a worker to time connections to other workers' hops.
type probeRequest struct {
	Targets []string `json:"targets"`
}

type probeResponse struct {
	// Results maps each target to the fastest of three TCP connection
	// times in milliseconds, or null when it could not be reached.
	Results map[string]*float64 `json:"results"`
}

// handlePeerProbe is the worker side of the latency map. Only the group host
// reaches it (through the worker's own reverse connections), only a worker
// whose owner allowed hops answers, and it is bounded: at most maxProbes
// IP-literal targets, one run every probeEvery.
func (a *Agent) handlePeerProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	a.mu.Lock()
	allowed := a.worker && a.c.AllowHop
	wait := time.Until(a.lastProbe.Add(probeEvery))
	if allowed && wait <= 0 {
		a.lastProbe = time.Now()
	}
	a.mu.Unlock()
	if !allowed {
		http.Error(w, "this worker's owner has not allowed hops (allow_hop)", http.StatusForbidden)
		return
	}
	if wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait/time.Second)+1))
		http.Error(w, "measured too recently", http.StatusTooManyRequests)
		return
	}
	var req probeRequest
	if !decodeSmall(w, r, &req) {
		return
	}
	targets := parsePaths(strings.Join(req.Targets, ","))
	if len(req.Targets) > maxProbes || len(targets) != len(req.Targets) {
		http.Error(w, "invalid targets", 400)
		return
	}
	res := probeResponse{Results: map[string]*float64{}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Add(1)
		go func(t string) {
			defer wg.Done()
			ms := timeTCP(r.Context(), t)
			mu.Lock()
			res.Results[t] = ms
			mu.Unlock()
		}(t)
	}
	wg.Wait()
	encode(w, res)
}

// timeTCP is the fastest of three TCP connection set-ups to endpoint (one
// network round trip each), in milliseconds; nil when unreachable.
func timeTCP(ctx context.Context, endpoint string) *float64 {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil
	}
	var best *float64
	for i := 0; i < 3; i++ {
		d := net.Dialer{Timeout: probeDial}
		start := time.Now()
		c, err := d.DialContext(ctx, "tcp", u.Host)
		took := time.Since(start)
		if err != nil {
			continue
		}
		c.Close()
		ms := float64(took.Microseconds()) / 1000
		if best == nil || ms < *best {
			best = &ms
		}
	}
	return best
}

// Topology is the host's map of its group (GET /v1/topology, host only).
type Topology struct {
	MeasuredAt time.Time  `json:"measured_at,omitempty"`
	Nodes      []TopoNode `json:"nodes"`
	// Pairs are worker-to-worker connection times measured by the first
	// worker towards the second's hop.
	Pairs []TopoPair `json:"pairs"`
}

// TopoNode is one worker on the map.
type TopoNode struct {
	ID string `json:"id"`
	// Path, RTTMillis and HopVia describe its link to the host (see
	// LinkStatus).
	Path      string   `json:"path,omitempty"`
	HopVia    string   `json:"hop_via,omitempty"`
	RTTMillis *float64 `json:"rtt_ms,omitempty"`
	// Hop is set when its owner allowed hops.
	Hop *HopStatus `json:"hop,omitempty"`
}

// TopoPair is one measured worker-to-worker connection time.
type TopoPair struct {
	From   string   `json:"from"`
	To     string   `json:"to"`
	Millis *float64 `json:"ms"`
}

type topology struct {
	mu         sync.Mutex
	hops       map[string]HopStatus // connected workers that forward
	pairs      map[[2]string]*float64
	measuredAt time.Time
}

func newTopology() *topology {
	return &topology{hops: map[string]HopStatus{}, pairs: map[[2]string]*float64{}}
}

// runTopology keeps the host's picture of which workers forward fresh and
// has each of them time the others every topoMeasure.
func (a *Agent) runTopology(ctx context.Context) {
	every := topoEvery()
	t := time.NewTicker(every)
	defer t.Stop()
	lastMeasure := time.Time{}
	for {
		a.refreshHops(ctx)
		if time.Since(lastMeasure) >= min(topoMeasure, 2*every)-time.Second/2 {
			if a.measurePairs(ctx) {
				lastMeasure = time.Now()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// refreshHops asks every connected worker for its status and remembers the
// ones that forward.
func (a *Agent) refreshHops(ctx context.Context) {
	connected := a.broker.Workers()
	hops := map[string]HopStatus{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, id := range connected {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			st, err := a.workerStatus(ctx, id)
			if err != nil {
				a.mu.Lock()
				cached, ok := a.workerCache[id]
				a.mu.Unlock()
				if !ok {
					return
				}
				st = cached
			}
			if st.Hop != nil && len(st.Hop.Endpoints) > 0 {
				mu.Lock()
				hops[id] = *st.Hop
				mu.Unlock()
			}
		}(id)
	}
	wg.Wait()
	a.topo.mu.Lock()
	a.topo.hops = hops
	for k := range a.topo.pairs {
		if _, ok := hops[k[0]]; !ok {
			delete(a.topo.pairs, k)
		} else if _, ok := hops[k[1]]; !ok {
			delete(a.topo.pairs, k)
		}
	}
	a.topo.mu.Unlock()
}

// measurePairs has every forwarding worker time the other forwarding
// workers. It reports whether a measurement ran.
func (a *Agent) measurePairs(ctx context.Context) bool {
	a.topo.mu.Lock()
	hops := make(map[string]HopStatus, len(a.topo.hops))
	for id, h := range a.topo.hops {
		hops[id] = h
	}
	a.topo.mu.Unlock()
	if len(hops) < 2 {
		return false
	}
	ids := make([]string, 0, len(hops))
	for id := range hops {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	type result struct {
		from string
		ms   map[string]*float64 // to-worker -> best time
	}
	out := make(chan result, len(ids))
	for _, from := range ids {
		go func(from string) {
			owner := map[string]string{}
			var targets []string
			for _, to := range ids {
				if to == from {
					continue
				}
				eps := hops[to].Endpoints
				if len(eps) > hopEndpoints {
					eps = eps[:hopEndpoints]
				}
				for _, e := range eps {
					if len(targets) < maxProbes {
						targets = append(targets, e)
						owner[e] = to
					}
				}
			}
			res, err := a.probePeers(ctx, from, targets)
			ms := map[string]*float64{}
			for _, to := range ids {
				if to != from {
					ms[to] = nil
				}
			}
			if err == nil {
				for e, v := range res.Results {
					to, ok := owner[e]
					if ok && v != nil && (ms[to] == nil || *v < *ms[to]) {
						v := *v
						ms[to] = &v
					}
				}
			} else {
				ms = nil // keep the previous answer
			}
			out <- result{from, ms}
		}(from)
	}
	a.topo.mu.Lock()
	defer a.topo.mu.Unlock()
	for range ids {
		r := <-out
		for to, v := range r.ms {
			a.topo.pairs[[2]string{r.from, to}] = v
		}
	}
	a.topo.measuredAt = time.Now().UTC()
	return true
}

func (a *Agent) probePeers(ctx context.Context, id string, targets []string) (probeResponse, error) {
	var res probeResponse
	body, _ := json.Marshal(probeRequest{Targets: targets})
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://worker.internal"+probePath, strings.NewReader(string(body)))
	if err != nil {
		return res, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.broker.RoundTrip(req, id, a.verifyWorker)
	if err != nil {
		return res, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return res, errors.New("probe refused: " + resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return res, err
	}
	return res, json.Unmarshal(b, &res)
}

// hopsHeader offers worker id the other workers' forwarders, those with the
// fastest link to the host first.
func (a *Agent) hopsHeader(id string) string {
	if a.topo == nil {
		return ""
	}
	a.topo.mu.Lock()
	type offer struct {
		id  string
		rtt float64
		eps []string
	}
	var offers []offer
	for other, h := range a.topo.hops {
		if other == id {
			continue
		}
		offers = append(offers, offer{id: other, rtt: 1e9, eps: h.Endpoints})
	}
	a.topo.mu.Unlock()
	links := a.links.Status()
	for i := range offers {
		if l, ok := links[offers[i].id]; ok && l.RTTMillis != nil && l.Path != "hop" {
			offers[i].rtt = *l.RTTMillis
		}
	}
	sort.Slice(offers, func(i, j int) bool {
		if offers[i].rtt != offers[j].rtt {
			return offers[i].rtt < offers[j].rtt
		}
		return offers[i].id < offers[j].id
	})
	var parts []string
	for _, o := range offers {
		eps := o.eps
		if len(eps) > hopEndpoints {
			eps = eps[:hopEndpoints]
		}
		for _, e := range eps {
			if len(parts) < maxHops {
				parts = append(parts, e+" "+o.id)
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return HopsHeader + ": " + strings.Join(parts, ", ") + "\r\n"
}

// isHop reports whether id is a connected worker that forwards.
func (a *Agent) isHop(id string) bool {
	if a.topo == nil {
		return false
	}
	a.topo.mu.Lock()
	defer a.topo.mu.Unlock()
	_, ok := a.topo.hops[id]
	return ok
}

// Topology returns the current map.
func (a *Agent) Topology() Topology {
	t := Topology{Nodes: []TopoNode{}, Pairs: []TopoPair{}}
	if a.topo == nil {
		return t
	}
	links := a.links.Status()
	a.topo.mu.Lock()
	defer a.topo.mu.Unlock()
	t.MeasuredAt = a.topo.measuredAt
	for _, id := range a.broker.Workers() {
		n := TopoNode{ID: id}
		if l, ok := links[id]; ok {
			n.Path, n.HopVia, n.RTTMillis = l.Path, l.HopVia, l.RTTMillis
		}
		if h, ok := a.topo.hops[id]; ok {
			h := h
			n.Hop = &h
		}
		t.Nodes = append(t.Nodes, n)
	}
	for k, v := range a.topo.pairs {
		t.Pairs = append(t.Pairs, TopoPair{From: k[0], To: k[1], Millis: v})
	}
	sort.Slice(t.Pairs, func(i, j int) bool {
		if t.Pairs[i].From != t.Pairs[j].From {
			return t.Pairs[i].From < t.Pairs[j].From
		}
		return t.Pairs[i].To < t.Pairs[j].To
	})
	return t
}

func (a *Agent) handleTopology(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	encode(w, a.Topology())
}

// say prints a line for the person running this computer.
func (a *Agent) say(format string, args ...any) {
	fmt.Fprintf(a.log.Writer(), format+"\n", args...)
}
