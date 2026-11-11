package resource

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type GPU struct {
	Index              int      `json:"index"`
	Name               string   `json:"name"`
	TotalBytes         *uint64  `json:"total_bytes"`
	FreeBytes          *uint64  `json:"free_bytes"`
	UtilizationPercent *float64 `json:"utilization_percent"`
	Driver             string   `json:"driver"`
	// TemperatureC, PowerWatts and PowerLimitWatts are nil when the driver
	// does not report them (many laptop GPUs have no power limit reading).
	TemperatureC    *float64 `json:"temperature_c,omitempty"`
	PowerWatts      *float64 `json:"power_watts,omitempty"`
	PowerLimitWatts *float64 `json:"power_limit_watts,omitempty"`
}
type Snapshot struct {
	OS                string  `json:"os"`
	Arch              string  `json:"arch"`
	CPU               string  `json:"cpu"`
	LogicalCPUs       int     `json:"logical_cpus"`
	RAMTotalBytes     *uint64 `json:"ram_total_bytes"`
	RAMAvailableBytes *uint64 `json:"ram_available_bytes"`
	// CPUPercent is the whole-machine CPU load since the previous discovery
	// (Linux) or as Windows reports it; nil on the first Linux sample.
	CPUPercent            *float64  `json:"cpu_percent,omitempty"`
	DiskTotalBytes        *uint64   `json:"disk_total_bytes"`
	DiskFreeBytes         *uint64   `json:"disk_free_bytes"`
	GPUs                  []GPU     `json:"gpus"`
	GPUDiscoveryAvailable bool      `json:"gpu_discovery_available"`
	CollectedAt           time.Time `json:"collected_at"`
	Issues                []string  `json:"issues"`
}

// command runs a short discovery probe. Windows WMI/CIM cold starts can take
// several seconds on a busy or freshly booted machine, so this is deliberately
// more generous than the disk probe.
func command(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return out, fmt.Errorf("%w: %s", err, msg)
		}
	}
	return out, err
}

// Discover returns best-effort measurements; Issues describes unavailable data.
func Discover(ctx context.Context, path string) (Snapshot, error) {
	s := Snapshot{OS: runtime.GOOS, Arch: runtime.GOARCH, CPU: "unknown", LogicalCPUs: runtime.NumCPU(), GPUs: []GPU{}, Issues: []string{}, CollectedAt: time.Now().UTC()}
	if err := ctx.Err(); err != nil {
		return s, err
	}
	if path == "" {
		path = "."
	}
	switch runtime.GOOS {
	case "linux":
		if b, e := os.ReadFile("/proc/meminfo"); e == nil {
			s.RAMTotalBytes, s.RAMAvailableBytes = parseMeminfo(string(b))
		} else {
			s.Issues = append(s.Issues, "RAM: "+e.Error())
		}
		if b, e := os.ReadFile("/proc/stat"); e == nil {
			s.CPUPercent = cpuLoad(string(b))
		}
		if b, e := os.ReadFile("/proc/cpuinfo"); e == nil {
			for _, line := range strings.Split(string(b), "\n") {
				k, v, ok := strings.Cut(line, ":")
				if ok && (strings.TrimSpace(k) == "model name" || strings.TrimSpace(k) == "Hardware") {
					s.CPU = strings.TrimSpace(v)
					break
				}
			}
		}
	case "windows":
		b, e := command(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", `$ProgressPreference='SilentlyContinue'; [Console]::OutputEncoding=[System.Text.Encoding]::UTF8; $ErrorActionPreference='Stop'; $o=Get-CimInstance Win32_OperatingSystem; $c=Get-CimInstance Win32_Processor | Select-Object -First 1; @{CPU=$c.Name; Load=$c.LoadPercentage; Total=[uint64]$o.TotalVisibleMemorySize*1024; Available=[uint64]$o.FreePhysicalMemory*1024} | ConvertTo-Json -Compress`)
		var w struct {
			CPU              string
			Load             *float64
			Total, Available *uint64
		}
		if e == nil {
			e = json.Unmarshal(decodePowerShell(b), &w)
		}
		if e != nil {
			s.Issues = append(s.Issues, "Windows hardware: "+e.Error())
		} else {
			if w.CPU != "" {
				s.CPU = w.CPU
			}
			s.RAMTotalBytes = w.Total
			s.RAMAvailableBytes = w.Available
			if w.Load != nil && *w.Load >= 0 && *w.Load <= 100 {
				s.CPUPercent = w.Load
			}
		}
	default:
		s.Issues = append(s.Issues, "hardware discovery unsupported on this OS")
	}
	total, free, e := disk(ctx, path)
	if e != nil {
		s.Issues = append(s.Issues, "disk: "+e.Error())
	} else {
		s.DiskTotalBytes = &total
		s.DiskFreeBytes = &free
	}
	b, e := command(ctx, "nvidia-smi", "--query-gpu=index,name,memory.total,memory.free,utilization.gpu,driver_version,temperature.gpu,power.draw,power.limit", "--format=csv,noheader,nounits")
	if e == nil {
		s.GPUs, e = parseGPUs(string(b))
		s.GPUDiscoveryAvailable = e == nil
	}
	if e != nil {
		s.Issues = append(s.Issues, "NVIDIA discovery: "+e.Error())
	}
	if s.RAMTotalBytes == nil || s.RAMAvailableBytes == nil {
		s.Issues = append(s.Issues, "RAM total or available unknown")
	}
	return s, ctx.Err()
}

func parseMeminfo(text string) (total, available *uint64) {
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) != 3 || f[2] != "kB" {
			continue
		}
		n, e := strconv.ParseUint(f[1], 10, 64)
		if e != nil || n > ^uint64(0)/1024 {
			continue
		}
		n *= 1024
		switch f[0] {
		case "MemTotal:":
			total = &n
		case "MemAvailable:":
			available = &n
		}
	}
	return
}
func parseGPUs(text string) ([]GPU, error) {
	rows, e := csv.NewReader(strings.NewReader(text)).ReadAll()
	if e != nil {
		return nil, e
	}
	out := []GPU{}
	for _, r := range rows {
		if len(r) != 6 && len(r) != 9 {
			return out, fmt.Errorf("unexpected NVIDIA column count")
		}
		for i := range r {
			r[i] = strings.TrimSpace(r[i])
		}
		index, e := strconv.Atoi(r[0])
		if e != nil || index < 0 {
			return out, fmt.Errorf("invalid GPU index")
		}
		g := GPU{Index: index, Name: r[1], Driver: r[5]}
		g.TotalBytes = parseMiB(r[2])
		g.FreeBytes = parseMiB(r[3])
		if n, e := strconv.ParseFloat(r[4], 64); e == nil && n >= 0 && n <= 100 {
			g.UtilizationPercent = &n
		}
		if len(r) == 9 {
			g.TemperatureC = number(r[6], 0, 150)
			g.PowerWatts = number(r[7], 0, 2000)
			g.PowerLimitWatts = number(r[8], 0, 2000)
		}
		out = append(out, g)
	}
	return out, nil
}
func parseMiB(s string) *uint64 {
	n, e := strconv.ParseUint(s, 10, 64)
	if e != nil || n > ^uint64(0)/(1024*1024) {
		return nil
	}
	n *= 1024 * 1024
	return &n
}

// number parses a reading such as "39" or "3.44"; "[N/A]" and values out of
// range give nil.
func number(s string, lo, hi float64) *float64 {
	n, e := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if e != nil || n < lo || n > hi {
		return nil
	}
	return &n
}

var cpuPrev struct {
	sync.Mutex
	busy, total uint64
}

// cpuLoad turns the "cpu" line of /proc/stat into the load since the
// previous call.
func cpuLoad(stat string) *float64 {
	line, _, _ := strings.Cut(stat, "\n")
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return nil
	}
	var total, idle uint64
	for i, v := range f[1:] {
		n, e := strconv.ParseUint(v, 10, 64)
		if e != nil {
			return nil
		}
		total += n
		if i == 3 || i == 4 { // idle, iowait
			idle += n
		}
	}
	busy := total - idle
	cpuPrev.Lock()
	defer cpuPrev.Unlock()
	pb, pt := cpuPrev.busy, cpuPrev.total
	cpuPrev.busy, cpuPrev.total = busy, total
	if pt == 0 || total <= pt || busy < pb {
		return nil
	}
	p := float64(busy-pb) * 100 / float64(total-pt)
	return &p
}
