package network

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

const ProbeBytes = 256 * 1024
const PingSamples = 3

var probePayload = bytes.Repeat([]byte("meshcomputeprobe"), ProbeBytes/16)

type Quality struct {
	MeasuredAt               time.Time     `json:"measured_at"`
	Latency                  time.Duration `json:"latency_ns"`
	Jitter                   time.Duration `json:"jitter_ns"`
	ThroughputBytesPerSecond float64       `json:"throughput_bytes_per_second"`
	Requests                 int           `json:"requests"`
	Failures                 int           `json:"failures"`
	RequestFailureRate       float64       `json:"request_failure_rate"`
	Available                bool          `json:"available"`
	Stable                   bool          `json:"stable"`
}

// Handler serves a fixed ping and a 256 KiB uncompressed deterministic probe.
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "pong\n")
	})
	mux.HandleFunc("GET /v1/probe", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprint(ProbeBytes))
		_, _ = w.Write(probePayload)
	})
	return mux
}

func Measure(ctx context.Context, client *http.Client, baseURL string) (Quality, error) {
	q := Quality{MeasuredAt: time.Now().UTC()}
	u, e := url.Parse(baseURL)
	if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return q, fmt.Errorf("base URL must be HTTP(S) without credentials, query or fragment")
	}
	if client == nil {
		client = http.DefaultClient
	}
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	var lastErr error
	request := func(path string, want []byte) (time.Duration, error) {
		child, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		req, e := http.NewRequestWithContext(child, http.MethodGet, strings.TrimRight(baseURL, "/")+path, nil)
		if e != nil {
			return 0, e
		}
		req.Header.Set("Accept-Encoding", "identity")
		req.Header.Set("Cache-Control", "no-cache")
		start := time.Now()
		resp, e := c.Do(req)
		if e != nil {
			return 0, e
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return 0, fmt.Errorf("probe HTTP status %d", resp.StatusCode)
		}
		body, e := io.ReadAll(io.LimitReader(resp.Body, int64(len(want)+1)))
		if e != nil {
			return 0, e
		}
		if !bytes.Equal(body, want) {
			return 0, fmt.Errorf("unexpected probe payload")
		}
		return time.Since(start), nil
	}
	var sum, min, max time.Duration
	success := 0
	for i := 0; i < PingSamples; i++ {
		q.Requests++
		d, e := request("/v1/ping", []byte("pong\n"))
		if e != nil {
			q.Failures++
			lastErr = e
			continue
		}
		sum += d
		success++
		if min == 0 || d < min {
			min = d
		}
		if d > max {
			max = d
		}
	}
	if success > 0 {
		q.Latency = sum / time.Duration(success)
		q.Jitter = max - min
	}
	// Some platforms expose a coarse monotonic clock, so one fast probe can
	// measure as zero. Repeat a bounded number of times and divide the total
	// transferred bytes by the aggregate elapsed time.
	q.Requests++
	probeBytes := 0
	probeStart := time.Now()
	var probeElapsed time.Duration
	for attempt := 0; attempt < 32; attempt++ {
		if _, e := request("/v1/probe", probePayload); e != nil {
			q.Failures++
			lastErr = e
			break
		}
		probeBytes += ProbeBytes
		probeElapsed = time.Since(probeStart)
		if probeElapsed > 0 {
			break
		}
	}
	if probeElapsed > 0 && probeBytes > 0 {
		q.ThroughputBytesPerSecond = float64(probeBytes) / probeElapsed.Seconds()
	}
	q.RequestFailureRate = float64(q.Failures) / float64(q.Requests)
	q.Available = success > 0 && q.ThroughputBytesPerSecond > 0
	q.Stable = q.Available && q.Failures == 0 && q.Jitter <= 100*time.Millisecond
	return q, lastErr
}

type Provider struct {
	Name      string   `json:"name"`
	Available bool     `json:"available"`
	State     string   `json:"state"`
	Addresses []string `json:"addresses"`
}

// DetectTailscale inspects status only; it never connects or changes configuration.
func DetectTailscale(ctx context.Context) (Provider, error) {
	p := Provider{Name: "tailscale", State: "unknown", Addresses: []string{}}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	b, e := exec.CommandContext(ctx, "tailscale", "status", "--json").Output()
	if e != nil {
		return p, e
	}
	var status struct {
		BackendState string
		TailscaleIPs []string
	}
	if e = json.Unmarshal(b, &status); e != nil {
		return p, e
	}
	p.State = status.BackendState
	p.Addresses = status.TailscaleIPs
	p.Available = status.BackendState == "Running"
	return p, nil
}
