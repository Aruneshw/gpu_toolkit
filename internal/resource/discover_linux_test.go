//go:build linux

package resource

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverLinux(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '0, Test GPU, 8192, 4096, 0, test-driver\\n'\n"
	if e := os.WriteFile(filepath.Join(dir, "nvidia-smi"), []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", dir)
	s, e := Discover(context.Background(), dir)
	if e != nil || s.OS != "linux" || s.RAMTotalBytes == nil || s.DiskTotalBytes == nil || !s.GPUDiscoveryAvailable || len(s.GPUs) != 1 || s.GPUs[0].Driver != "test-driver" {
		t.Fatalf("%+v %v", s, e)
	}
	t.Setenv("PATH", t.TempDir())
	s, e = Discover(context.Background(), dir+"/missing")
	if e != nil || s.GPUDiscoveryAvailable || s.DiskFreeBytes != nil || len(s.Issues) < 2 {
		t.Fatalf("%+v %v", s, e)
	}
}
