package backend

import (
	"context"
	"errors"
	"io"
	"meshcompute/internal/model"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type LaunchConfig struct {
	Binary, RegistryDir, Model, Endpoint string
	Context, GPULayers                   int
	Stdout, Stderr                       io.Writer
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
	cmd := exec.CommandContext(ctx, c.Binary, "--model", e.Path, "--alias", e.Name, "--host", u.Hostname(), "--port", strconv.Itoa(port), "--ctx-size", strconv.Itoa(c.Context), "--parallel", "1", "--n-gpu-layers", strconv.Itoa(c.GPULayers), "--no-context-shift")
	// Ambient llama configuration must not enable downloads, RPC or extra listeners.
	for _, env := range os.Environ() {
		key := strings.ToUpper(strings.SplitN(env, "=", 2)[0])
		if !strings.HasPrefix(key, "LLAMA_") && !strings.HasPrefix(key, "GGML_RPC") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	cmd.Stdout = c.Stdout
	cmd.Stderr = c.Stderr
	return cmd.Run()
}
