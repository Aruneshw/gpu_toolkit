package backend

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DeviceUse is the memory llama-server placed on one device while loading,
// as its own log reports it.
type DeviceUse struct {
	// Device is llama.cpp's name: "RPC0[127.0.0.1:41961]", "Vulkan0", "CPU".
	Device string `json:"device"`
	// Endpoint is the RPC address for RPC devices.
	Endpoint   string  `json:"endpoint,omitempty"`
	WeightsMiB float64 `json:"weights_mib"`
	KVMiB      float64 `json:"kv_mib"`
	ComputeMiB float64 `json:"compute_mib"`
}

// Allocation collects DeviceUse from llama-server's log lines as they are
// written. It is an io.Writer, so it can sit beside the log tail.
type Allocation struct {
	mu        sync.Mutex
	part      string
	devices   []DeviceUse
	offloaded string
}

var (
	bufferLine  = regexp.MustCompile(`(load_tensors|llama_kv_cache|sched_reserve):\s+(\S+)\s+(model|KV|compute) buffer size =\s+([0-9.]+) MiB`)
	offloadLine = regexp.MustCompile(`offloaded (\d+/\d+) layers to GPU`)
	rpcDevice   = regexp.MustCompile(`^RPC\d+\[([^\]]+)\]$`)
)

func (a *Allocation) Write(p []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	text := a.part + string(p)
	lines := strings.Split(text, "\n")
	a.part = lines[len(lines)-1]
	if len(a.part) > 4096 {
		a.part = ""
	}
	for _, l := range lines[:len(lines)-1] {
		a.line(l)
	}
	return len(p), nil
}

func (a *Allocation) line(l string) {
	if m := offloadLine.FindStringSubmatch(l); m != nil {
		a.offloaded = m[1]
		return
	}
	m := bufferLine.FindStringSubmatch(l)
	if m == nil {
		return
	}
	mib, err := strconv.ParseFloat(m[4], 64)
	if err != nil {
		return
	}
	name := m[2]
	if name == "CPU_Mapped" || name == "CPU_REPACK" {
		name = "CPU"
	}
	var d *DeviceUse
	for i := range a.devices {
		if a.devices[i].Device == name {
			d = &a.devices[i]
		}
	}
	if d == nil {
		if len(a.devices) >= 128 {
			return
		}
		a.devices = append(a.devices, DeviceUse{Device: name})
		d = &a.devices[len(a.devices)-1]
		if r := rpcDevice.FindStringSubmatch(name); r != nil {
			d.Endpoint = r[1]
		}
	}
	switch m[3] {
	case "model":
		d.WeightsMiB += mib
	case "KV":
		d.KVMiB += mib
	case "compute":
		d.ComputeMiB += mib
	}
}

// Devices returns what was placed so far and the "N/M layers offloaded"
// summary, if printed.
func (a *Allocation) Devices() ([]DeviceUse, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]DeviceUse(nil), a.devices...), a.offloaded
}

var (
	devicesMu    sync.Mutex
	devicesCache = map[string][]string{}
	listLine     = regexp.MustCompile(`^\s+([A-Za-z]+[0-9]+):`)
)

// LocalDevices lists the accelerators llama-server can use on this computer
// (for example Vulkan0, CUDA0), in llama.cpp's order. A CPU-only build has
// none. The answer is cached per binary.
func LocalDevices(ctx context.Context, binary string) ([]string, error) {
	devicesMu.Lock()
	cached, ok := devicesCache[binary]
	devicesMu.Unlock()
	if ok {
		return cached, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--list-devices")
	cmd.Dir = filepath.Dir(binary)
	cmd.Env = toolEnv(binary)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil && !strings.Contains(out.String(), "Available devices") {
		return nil, errors.New("llama-server --list-devices failed: " + strings.TrimSpace(lastLine(out.String())))
	}
	names := ParseDeviceList(out.String())
	devicesMu.Lock()
	devicesCache[binary] = names
	devicesMu.Unlock()
	return names, nil
}

// ParseDeviceList reads llama-server --list-devices output, skipping RPC
// devices (those are the workers).
func ParseDeviceList(text string) []string {
	names := []string{}
	in := false
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(l, "Available devices") {
			in = true
			continue
		}
		if m := listLine.FindStringSubmatch(l); in && m != nil && !strings.HasPrefix(m[1], "RPC") {
			names = append(names, m[1])
		}
	}
	return names
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// ForgetDevices clears the cached device lists, for example after llama.cpp
// was reinstalled with GPU support.
func ForgetDevices() {
	devicesMu.Lock()
	devicesCache = map[string][]string{}
	devicesMu.Unlock()
}
