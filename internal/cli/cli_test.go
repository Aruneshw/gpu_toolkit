package cli

import (
	"bytes"
	"context"
	"meshcompute/internal/config"
	"strings"
	"testing"
)

func TestCLIParsing(t *testing.T) {
	d := t.TempDir()
	if e := config.Install(d); e != nil {
		t.Fatal(e)
	}
	for _, args := range [][]string{{"--invalid"}, {"bogus"}, {"install", "extra"}, {"cluster"}, {"pair", "--ttl", "no"}, {"join"}, {"peer", "add"}, {"monitor", "--interval", "0s"}, {"run"}, {"run", "tiny", "--unknown"}, {"status", "extra"}, {"config", "set", "listen", "0.0.0.0:7443"}, {"model", "add", "../../bad", "none"}} {
		var out bytes.Buffer
		full := append([]string{"--home", d}, args...)
		if e := Run(context.Background(), full, strings.NewReader(""), &out, "test"); e == nil {
			t.Errorf("accepted %v", args)
		}
	}
}
func TestCLIInstallAndSimulation(t *testing.T) {
	d := t.TempDir()
	for _, command := range [][]string{{"install"}, {"version"}, {"simulate"}, {"model", "list"}} {
		var out bytes.Buffer
		if e := Run(context.Background(), append([]string{"--home", d}, command...), strings.NewReader(""), &out, "test-build"); e != nil {
			t.Fatal(e)
		}
		if command[0] == "simulate" && (!strings.Contains(out.String(), `"simulation": true`) || !strings.Contains(out.String(), `"inference_performed": false`)) {
			t.Fatal("simulation mislabeled")
		}
	}
}
func TestHelp(t *testing.T) {
	var out bytes.Buffer
	if e := Run(context.Background(), []string{"--help"}, nil, &out, "test"); e != nil || !strings.Contains(out.String(), "MeshCompute") {
		t.Fatal(e)
	}
}
