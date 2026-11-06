//go:build !windows

package network

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestProvider(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	p, e := DetectTailscale(context.Background())
	if e == nil || p.Available || p.State != "unknown" {
		t.Fatalf("%+v %v", p, e)
	}
	for _, tc := range []struct {
		body                 string
		available, wantError bool
	}{{`{"BackendState":"Running","TailscaleIPs":["100.64.0.1"]}`, true, false}, {`{"BackendState":"Stopped"}`, false, false}, {`not-json`, false, true}} {
		script := "#!/bin/sh\n[ \"$1\" = status ] && [ \"$2\" = --json ] || exit 2\nprintf '%s' '" + tc.body + "'\n"
		if e := os.WriteFile(filepath.Join(dir, "tailscale"), []byte(script), 0700); e != nil {
			t.Fatal(e)
		}
		p, e := DetectTailscale(context.Background())
		if (e != nil) != tc.wantError || p.Available != tc.available {
			t.Fatalf("%+v %v", p, e)
		}
	}
}
