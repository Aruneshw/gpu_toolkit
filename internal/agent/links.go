package agent

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"meshcompute/internal/network"
	"meshcompute/internal/tunnel"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// StreamEcho is a tiny echo stream the host keeps open to each worker to
// measure the link's round-trip time over the same path the pool uses.
const StreamEcho = "/v1/stream/echo"

const (
	linkEvery    = 2 * time.Second
	echoFrame    = 16
	linkWindow   = 15               // samples for the typical (median) round trip
	echoIdle     = 30 * time.Second // the worker closes an echo stream left idle this long
	retryOldPeer = 5 * time.Minute  // a worker without the echo stream is asked again after this
)

// LinkStatus describes the host's link to one worker.
type LinkStatus struct {
	// Path is how the worker's connections reach the host: "relay" (the
	// public TCP relay), "mesh" (a mesh VPN such as Tailscale or WireGuard),
	// "direct" (LAN or internet address) or "local" (this computer).
	Path string `json:"path,omitempty"`
	// Via is the host address the worker's latest connection arrived at.
	Via string `json:"via,omitempty"`
	// HopVia is the worker it came through when Path is "hop".
	HopVia string `json:"hop_via,omitempty"`
	// RTTMillis is the median of the recent round trips through the tunnel
	// (the path the pool's RPC traffic takes), LastMillis the latest one.
	RTTMillis  *float64 `json:"rtt_ms,omitempty"`
	LastMillis *float64 `json:"last_rtt_ms,omitempty"`
	// Measured is "tunnel" when the echo stream works, "unsupported" for a
	// worker from an older release (the dashboard then times a status
	// request instead), or "" before the first answer.
	Measured string `json:"measured,omitempty"`
	// llama.cpp RPC traffic to this worker over the last interval.
	RPCUpBytesPerSec   float64 `json:"rpc_up_bps"`
	RPCDownBytesPerSec float64 `json:"rpc_down_bps"`
	RPCRoundTripsPerS  float64 `json:"rpc_round_trips_per_s"`
	// WaitShare estimates the share of time the pool spends waiting for
	// this link: RPC round trips per second times the round-trip time.
	WaitShare  *float64 `json:"wait_share,omitempty"`
	RPCUpTotal uint64   `json:"rpc_up_total"`
	RPCDnTotal uint64   `json:"rpc_down_total"`
}

type link struct {
	stream    net.Conn
	seq       uint64
	rtts      []float64
	retryAt   time.Time
	meter     tunnel.Meter
	prev      tunnel.MeterCounts
	prevAt    time.Time
	status    LinkStatus
	probing   bool
	lastProbe time.Time
}

type links struct {
	a  *Agent
	mu sync.Mutex
	m  map[string]*link
}

func newLinks(a *Agent) *links { return &links{a: a, m: map[string]*link{}} }

func (l *links) get(id string) *link {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := l.m[id]
	if k == nil {
		k = &link{}
		l.m[id] = k
	}
	return k
}

// meter is the RPC traffic counter of one worker's link.
func (l *links) meter(id string) *tunnel.Meter {
	if l == nil {
		return nil
	}
	return &l.get(id).meter
}

// setPath records how a worker's latest connection arrived.
func (l *links) setPath(id, path string, via net.Addr) {
	if l == nil || path == "" {
		return
	}
	k := l.get(id)
	l.mu.Lock()
	k.status.Path = path
	if via != nil {
		k.status.Via = via.String()
	}
	l.mu.Unlock()
}

// setHop marks a worker's latest connection as coming through worker via
// (Path "hop"), or clears that when via is "".
func (l *links) setHop(id, via string) {
	if l == nil {
		return
	}
	k := l.get(id)
	l.mu.Lock()
	defer l.mu.Unlock()
	k.status.HopVia = via
	if via != "" {
		k.status.Path = "hop"
	}
}

// Status returns every known worker's link.
func (l *links) Status() map[string]LinkStatus {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]LinkStatus, len(l.m))
	for id, k := range l.m {
		out[id] = k.status
	}
	return out
}

// run measures every connected worker's link every linkEvery.
func (l *links) run(ctx context.Context) {
	t := time.NewTicker(linkEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			l.mu.Lock()
			for _, k := range l.m {
				if k.stream != nil {
					k.stream.Close()
				}
			}
			l.mu.Unlock()
			return
		case <-t.C:
		}
		connected := map[string]bool{}
		for _, id := range l.a.broker.Workers() {
			connected[id] = true
			k := l.get(id)
			l.mu.Lock()
			busy := k.probing
			k.probing = true
			l.mu.Unlock()
			if !busy {
				go func() {
					l.probe(ctx, id, k)
					l.mu.Lock()
					k.probing = false
					l.mu.Unlock()
				}()
			}
		}
		l.mu.Lock()
		for id, k := range l.m {
			if !connected[id] && !k.probing {
				if k.stream != nil {
					k.stream.Close()
				}
				delete(l.m, id)
			}
		}
		l.mu.Unlock()
	}
}

// probe times one echo through the worker's tunnel and updates the RPC
// traffic rates.
func (l *links) probe(ctx context.Context, id string, k *link) {
	now := time.Now()
	counts := k.meter.Counts()
	l.mu.Lock()
	if !k.prevAt.IsZero() {
		dt := now.Sub(k.prevAt).Seconds()
		if dt > 0 {
			k.status.RPCUpBytesPerSec = float64(counts.Up-k.prev.Up) / dt
			k.status.RPCDownBytesPerSec = float64(counts.Down-k.prev.Down) / dt
			k.status.RPCRoundTripsPerS = float64(counts.Turns-k.prev.Turns) / dt
		}
	}
	k.status.RPCUpTotal, k.status.RPCDnTotal = counts.Up, counts.Down
	k.prev, k.prevAt = counts, now
	stream, retryAt := k.stream, k.retryAt
	k.seq++
	seq := k.seq
	l.mu.Unlock()

	if stream == nil {
		if now.Before(retryAt) {
			l.finish(k, nil)
			return
		}
		octx, cancel := context.WithTimeout(ctx, 5*time.Second)
		s, err := l.a.broker.OpenStream(octx, id, StreamEcho, l.a.verifyWorker)
		cancel()
		if err != nil {
			l.mu.Lock()
			if strings.Contains(err.Error(), "HTTP 404") {
				// A worker from an older release: no echo stream.
				k.status.Measured, k.retryAt = "unsupported", now.Add(retryOldPeer)
			}
			l.mu.Unlock()
			l.finish(k, nil)
			return
		}
		stream = s
		l.mu.Lock()
		k.stream = s
		l.mu.Unlock()
	}
	var out [echoFrame]byte
	binary.BigEndian.PutUint64(out[:8], seq)
	binary.BigEndian.PutUint64(out[8:], uint64(now.UnixNano()))
	start := time.Now()
	stream.SetDeadline(start.Add(5 * time.Second))
	var in [echoFrame]byte
	_, err := stream.Write(out[:])
	if err == nil {
		_, err = io.ReadFull(stream, in[:])
	}
	rtt := time.Since(start)
	if err != nil || !bytes.Equal(in[:], out[:]) {
		stream.Close()
		l.mu.Lock()
		if k.stream == stream {
			k.stream = nil
		}
		l.mu.Unlock()
		l.finish(k, nil)
		return
	}
	ms := float64(rtt.Microseconds()) / 1000
	l.finish(k, &ms)
}

// finish records one probe's round trip (nil: none) and derives the
// summary.
func (l *links) finish(k *link, ms *float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	k.lastProbe = time.Now()
	k.status.LastMillis = ms
	if ms == nil {
		if k.stream == nil && k.status.Measured != "unsupported" {
			k.rtts = k.rtts[:0]
			k.status.RTTMillis, k.status.WaitShare = nil, nil
		}
		return
	}
	k.status.Measured = "tunnel"
	k.rtts = append(k.rtts, *ms)
	if len(k.rtts) > linkWindow {
		k.rtts = k.rtts[len(k.rtts)-linkWindow:]
	}
	sorted := append([]float64{}, k.rtts...)
	sort.Float64s(sorted)
	median := sorted[len(sorted)/2]
	k.status.RTTMillis = &median
	if k.status.RPCRoundTripsPerS > 0 {
		share := k.status.RPCRoundTripsPerS * median / 1000
		if share > 1 {
			share = 1
		}
		k.status.WaitShare = &share
	} else {
		k.status.WaitShare = nil
	}
}

// serveEcho is the worker side of StreamEcho: it answers each frame with
// the same bytes and ends when the host goes quiet.
func serveEcho(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	conn, err := tunnel.AcceptStream(w, r)
	if err != nil {
		return
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	var buf [echoFrame]byte
	for {
		conn.SetDeadline(time.Now().Add(echoIdle))
		if _, err := io.ReadFull(conn, buf[:]); err != nil {
			return
		}
		if _, err := conn.Write(buf[:]); err != nil {
			return
		}
	}
}

// classifyPath names how a worker's connection reached the host from the
// address it arrived on.
func classifyPath(local, remote net.Addr, relay bool) string {
	ipOf := func(a net.Addr) net.IP {
		if a == nil {
			return nil
		}
		host, _, err := net.SplitHostPort(a.String())
		if err != nil {
			return nil
		}
		return net.ParseIP(host)
	}
	lip, rip := ipOf(local), ipOf(remote)
	switch {
	case rip != nil && rip.IsLoopback():
		if relay {
			return "relay" // the relay client hands visitors over on loopback
		}
		return "local"
	case network.IsMeshIP(lip) || network.IsMeshIP(rip):
		return "mesh"
	case lip != nil:
		return "direct"
	}
	return ""
}

// linkWarnings explains which workers' links slow a pool down. llama.cpp
// RPC is a star: every token waits for each worker in turn, so one slow link
// slows the whole pool.
func linkWarnings(workers []string, all map[string]LinkStatus) []string {
	var known []float64
	for _, id := range workers {
		if s, ok := all[id]; ok && s.RTTMillis != nil {
			known = append(known, *s.RTTMillis)
		}
	}
	sort.Float64s(known)
	var out []string
	for _, id := range workers {
		s, ok := all[id]
		if !ok || s.RTTMillis == nil {
			continue
		}
		rtt := *s.RTTMillis
		others := othersMedian(known, rtt)
		switch {
		case s.WaitShare != nil && *s.WaitShare >= 0.4:
			out = append(out, fmt.Sprintf("%s: about %.0f%% of generation time waits on this link (%.0f round trips/s × %.1f ms)%s",
				short(id), *s.WaitShare*100, s.RPCRoundTripsPerS, rtt, pathHint(s.Path)))
		case s.Path == "relay" && rtt >= 20:
			out = append(out, fmt.Sprintf("%s: reaches the host through the public relay (%.0f ms per round trip); every token waits for it%s",
				short(id), rtt, pathHint(s.Path)))
		case others > 0 && rtt > 3*others && rtt-others >= 10:
			out = append(out, fmt.Sprintf("%s: its link (%.0f ms) is %.0f× slower than the other workers' (%.1f ms); the pool waits for it on every token, so it may be faster without it",
				short(id), rtt, rtt/others, others))
		}
		// About one RPC round trip per token: a large answer per round trip
		// from a worker means it computes the output layer and sends the
		// host every token's logits (vocabulary size × 4 bytes).
		if s.Path != "local" && s.RPCRoundTripsPerS >= 1 {
			if per := s.RPCDownBytesPerSec / s.RPCRoundTripsPerS; per >= logitsBytes {
				out = append(out, fmt.Sprintf("%s: sends the host about %.0f KB per token (the output layer's logits, %.1f MB/s now); on Wi-Fi or the relay that limits speed. Run the output layer on the host (pool editor, or pool start --output-layer host) to send a few KB instead",
					short(id), per/1024, s.RPCDownBytesPerSec/(1<<20)))
			}
		}
	}
	return out
}

// logitsBytes is the per-token answer above which a worker is taken to be
// sending logits: 32k vocabulary × 4 bytes.
const logitsBytes = 128 << 10

// othersMedian is the median of the sorted round trips without one copy of
// self; 0 when there are no others.
func othersMedian(sorted []float64, self float64) float64 {
	rest := make([]float64, 0, len(sorted))
	removed := false
	for _, v := range sorted {
		if !removed && v == self {
			removed = true
			continue
		}
		rest = append(rest, v)
	}
	if len(rest) == 0 {
		return 0
	}
	return rest[len(rest)/2]
}

func pathHint(path string) string {
	if path == "relay" {
		return "; a mesh VPN (Tailscale/WireGuard) or the same LAN avoids the relay"
	}
	return ""
}
