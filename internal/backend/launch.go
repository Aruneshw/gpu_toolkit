package backend

import (
	"context"
	"errors"
	"io"
	"meshcompute/internal/model"
	"net"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type LaunchConfig struct {
	Binary, RegistryDir, Model, Endpoint string
	Context, GPULayers                   int
	Stdout, Stderr                       io.Writer
	// RPC lists loopback addresses of forwarders to workers' llama.cpp RPC
	// servers. Layers are then spread over this computer and those workers.
	RPC []string
	// Devices, when set, is the exact --device list in order (for example
	// Vulkan0,RPC0,RPC1); ["none"] keeps every layer on the CPU.
	Devices []string
	// TensorSplit gives one relative weight per entry of Devices.
	TensorSplit []float64
	// Parallel is the number of request slots (0 means 1).
	Parallel int
	// CacheTypeK/V are KV cache types; empty leaves llama.cpp's default.
	CacheTypeK, CacheTypeV string
	// NoKVOffload keeps the KV cache in host RAM.
	NoKVOffload bool
	// FlashAttn is "", auto, on or off.
	FlashAttn string
	// OutputOn, when set, places the output layer (output.weight, or the
	// tied token_embd.weight) on this buffer type: CPU or a device name.
	OutputOn string
	// Verbose raises the log level so per-device buffer sizes are printed.
	Verbose bool
}

// cacheTypes mirrors config.CacheTypes; llama.cpp rejects anything else.
var cacheTypes = map[string]bool{"f32": true, "f16": true, "bf16": true, "q8_0": true, "q4_0": true, "q4_1": true, "iq4_nl": true, "q5_0": true, "q5_1": true}

var deviceName = regexp.MustCompile(`^(none|[A-Za-z]+[0-9]+)$`)

// Args returns llama-server's arguments for c with the model at modelPath.
func Args(c LaunchConfig, modelPath, alias string) ([]string, error) {
	u, err := url.Parse(c.Endpoint)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("explicit valid server port required")
	}
	parallel := c.Parallel
	if parallel == 0 {
		parallel = 1
	}
	if parallel < 1 || parallel > 64 {
		return nil, errors.New("parallel must be 1-64")
	}
	args := []string{"--model", modelPath, "--alias", alias, "--host", u.Hostname(), "--port", strconv.Itoa(port), "--ctx-size", strconv.Itoa(c.Context), "--parallel", strconv.Itoa(parallel), "--n-gpu-layers", strconv.Itoa(c.GPULayers), "--no-context-shift"}
	for _, addr := range c.RPC {
		host, _, err := net.SplitHostPort(addr)
		if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
			return nil, errors.New("RPC endpoints must be loopback forwarders")
		}
	}
	if len(c.RPC) > 0 {
		args = append(args, "--rpc", strings.Join(c.RPC, ","))
	}
	if len(c.Devices) > 0 {
		for _, d := range c.Devices {
			if !deviceName.MatchString(d) {
				return nil, errors.New("invalid device name " + d)
			}
		}
		args = append(args, "--device", strings.Join(c.Devices, ","))
	}
	if len(c.TensorSplit) > 0 {
		if len(c.TensorSplit) != len(c.Devices) || c.Devices[0] == "none" {
			return nil, errors.New("tensor split needs one weight per device")
		}
		parts := make([]string, len(c.TensorSplit))
		positive := false
		for i, w := range c.TensorSplit {
			if w < 0 || w > 1000 || w != w {
				return nil, errors.New("invalid split weight")
			}
			positive = positive || w > 0
			parts[i] = strconv.FormatFloat(w, 'f', -1, 64)
		}
		if !positive {
			return nil, errors.New("at least one split weight must be above 0")
		}
		args = append(args, "--tensor-split", strings.Join(parts, ","))
	}
	for _, kv := range [][2]string{{"--cache-type-k", c.CacheTypeK}, {"--cache-type-v", c.CacheTypeV}} {
		flag, t := kv[0], kv[1]
		if t == "" {
			continue
		}
		if !cacheTypes[t] {
			return nil, errors.New("unknown KV cache type " + t)
		}
		args = append(args, flag, t)
	}
	if c.NoKVOffload {
		args = append(args, "--no-kv-offload")
	}
	switch c.FlashAttn {
	case "":
	case "auto", "on", "off":
		args = append(args, "--flash-attn", c.FlashAttn)
	default:
		return nil, errors.New("flash attention must be auto, on or off")
	}
	if c.OutputOn != "" {
		if c.OutputOn != "CPU" && (!deviceName.MatchString(c.OutputOn) || c.OutputOn == "none") {
			return nil, errors.New("invalid output layer device " + c.OutputOn)
		}
		// Anchored: an unanchored "output.weight" also matches every
		// attn_output.weight. A model with tied embeddings uses
		// token_embd.weight as its output layer.
		args = append(args, "--override-tensor", `^(output|token_embd)\.weight$=`+c.OutputOn)
	}
	if c.Verbose {
		args = append(args, "--log-verbosity", "4")
	}
	return args, nil
}

// Run blocks until the foreground process exits. Context cancellation kills it.
func Run(ctx context.Context, c LaunchConfig) error {
	if !filepath.IsAbs(c.Binary) || c.Context < 1 || c.Context > 1048576 || c.GPULayers < 0 {
		return errors.New("absolute binary path and valid context/GPU layers required")
	}
	if _, err := New(c.Endpoint); err != nil {
		return err
	}
	e, err := model.Get(c.RegistryDir, c.Model)
	if err != nil {
		return err
	}
	if err := model.Verify(e); err != nil {
		return err
	}
	args, err := Args(c, e.Path, e.Name)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, c.Binary, args...)
	cmd.Dir = filepath.Dir(c.Binary)
	cmd.WaitDelay = 5 * time.Second
	// Ambient llama configuration must not enable downloads, RPC or extra
	// listeners; only the explicit arguments above do.
	cmd.Env = toolEnv(c.Binary)
	cmd.Stdout = c.Stdout
	cmd.Stderr = c.Stderr
	return cmd.Run()
}
