package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"meshcompute/internal/model"
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("MESH_BACKEND_HELPER") == "1" {
		if os.Getenv("LLAMA_ARG_RPC") != "" {
			os.Exit(3)
		}
		json.NewEncoder(os.Stdout).Encode(os.Args[1:])
		os.Exit(0)
	}
	os.Exit(m.Run())
}
func TestForegroundLaunch(t *testing.T) {
	t.Setenv("MESH_BACKEND_HELPER", "1")
	t.Setenv("LLAMA_ARG_RPC", "unwanted:1234")
	dir := t.TempDir()
	path := filepath.Join(dir, "model with spaces.gguf")
	if err := os.WriteFile(path, []byte("GGUFfixture"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := filepath.Join(dir, "registry")
	e, err := model.Add(reg, "tiny", path)
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	c := LaunchConfig{Binary: binary, RegistryDir: reg, Model: "tiny", Endpoint: "http://127.0.0.1:8080", Context: 2048, Stdout: &out}
	if err := Run(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	var args []string
	if err := json.Unmarshal(out.Bytes(), &args); err != nil {
		t.Fatal(err)
	}
	if len(args) < 4 || args[0] != "--model" || args[1] != e.Path || args[2] != "--alias" || args[3] != "tiny" {
		t.Fatalf("bad args %v", args)
	}
	if err := os.WriteFile(path, []byte("GGUFchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), c); err == nil {
		t.Fatal("launched changed model")
	}
}
