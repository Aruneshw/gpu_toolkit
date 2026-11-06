package resource

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type GPU struct {
	Index              int      `json:"index"`
	Name               string   `json:"name"`
	TotalBytes         *uint64  `json:"total_bytes"`
	FreeBytes          *uint64  `json:"free_bytes"`
	UtilizationPercent *float64 `json:"utilization_percent"`
	Driver             string   `json:"driver"`
}
type Snapshot struct {
	OS                    string    `json:"os"`
	Arch                  string    `json:"arch"`
	CPU                   string    `json:"cpu"`
	LogicalCPUs           int       `json:"logical_cpus"`
	RAMTotalBytes         *uint64   `json:"ram_total_bytes"`
	RAMAvailableBytes     *uint64   `json:"ram_available_bytes"`
	DiskTotalBytes        *uint64   `json:"disk_total_bytes"`
	DiskFreeBytes         *uint64   `json:"disk_free_bytes"`
	GPUs                  []GPU     `json:"gpus"`
	GPUDiscoveryAvailable bool      `json:"gpu_discovery_available"`
	CollectedAt           time.Time `json:"collected_at"`
	Issues                []string  `json:"issues"`
}

func command(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
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
		b, e := command(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", `$ErrorActionPreference='Stop'; $o=Get-CimInstance Win32_OperatingSystem; $c=Get-CimInstance Win32_Processor | Select-Object -First 1; @{CPU=$c.Name; Total=[uint64]$o.TotalVisibleMemorySize*1024; Available=[uint64]$o.FreePhysicalMemory*1024} | ConvertTo-Json -Compress`)
		var w struct {
			CPU              string
			Total, Available *uint64
		}
		if e == nil {
			e = json.Unmarshal(b, &w)
		}
		if e != nil {
			s.Issues = append(s.Issues, "Windows hardware: "+e.Error())
		} else {
			if w.CPU != "" {
				s.CPU = w.CPU
			}
			s.RAMTotalBytes = w.Total
			s.RAMAvailableBytes = w.Available
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
	b, e := command(ctx, "nvidia-smi", "--query-gpu=index,name,memory.total,memory.free,utilization.gpu,driver_version", "--format=csv,noheader,nounits")
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
		if len(r) != 6 {
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
