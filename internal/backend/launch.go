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
}

// Run blocks until the foreground process exits. Context cancellation kills it.
func Run(ctx context.Context, c LaunchConfig) error {
	if !filepath.IsAbs(c.Binary) || c.Context < 1 || c.Context > 1048576 || c.GPULayers < 0 {
		return errors.New("absolute binary path and valid context/GPU layers required")
	}
	if _, err := New(c.Endpoint); err != nil {
		return err
	}
	u, _ := url.Parse(c.Endpoint)
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return errors.New("explicit valid server port required")
	}
	e, err := model.Get(c.RegistryDir, c.Model)
	if err != nil {
		return err
	}
	if err := model.Verify(e); err != nil {
		return err
	}
	args := []string{"--model", e.Path, "--alias", e.Name, "--host", u.Hostname(), "--port", strconv.Itoa(port), "--ctx-size", strconv.Itoa(c.Context), "--parallel", "1", "--n-gpu-layers", strconv.Itoa(c.GPULayers), "--no-context-shift"}
	for _, addr := range c.RPC {
		host, _, err := net.SplitHostPort(addr)
		if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
			return errors.New("RPC endpoints must be loopback forwarders")
		}
	}
	if len(c.RPC) > 0 {
		args = append(args, "--rpc", strings.Join(c.RPC, ","))
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
