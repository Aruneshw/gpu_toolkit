package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfig(t *testing.T) {
	d := t.TempDir()
	if e := Install(d); e != nil {
		t.Fatal(e)
	}
	c, e := Load(d)
	if e != nil {
		t.Fatal(e)
	}
	c.Listen = "100.64.0.1:7443"
	c.Advertise = "https://100.64.0.1:7443"
	if e = Save(d, c); e != nil {
		t.Fatal(e)
	}
	if e = Install(d); e != nil {
		t.Fatal(e)
	}
	got, _ := Load(d)
	if got.Listen != c.Listen {
		t.Fatal("install overwrote config")
	}
}
func TestEndpoints(t *testing.T) {
	for _, s := range []string{"https://0.0.0.0:7443", "https://8.8.8.8:1", "http://127.0.0.1:1", "https://user:pass@127.0.0.1:1", "https://localhost:7443", "https://127.0.0.1:0", "https://127.0.0.1:1/x"} {
		if Endpoint(s, false) == nil {
			t.Errorf("accepted %s", s)
		}
	}
	if Endpoint("https://[::1]:7443", false) != nil {
		t.Fatal("IPv6 loopback rejected")
	}
	if Endpoint("http://192.168.1.1:8080", true) == nil {
		t.Fatal("remote backend accepted")
	}
}
func TestStrictConfig(t *testing.T) {
	d := t.TempDir()
	for _, s := range []string{`{"version":1,"surprise":true}`, `{} {}`} {
		if e := os.WriteFile(filepath.Join(d, "config.json"), []byte(s), 0600); e != nil {
			t.Fatal(e)
		}
		if _, e := Load(d); e == nil {
			t.Fatal("invalid config accepted")
		}
	}
}

func TestPoolValidation(t *testing.T) {
	w1 := "worker-" + strings.Repeat("a", 64)
	w2 := "worker-" + strings.Repeat("b", 64)
	good := PoolConfig{Name: "big-1", Model: "qwen", Context: 4096, Parallel: 4, Workers: []string{w1}, Split: map[string]float64{"local": 1, w1: 3}, CacheTypeK: "q8_0", CacheTypeV: "q4_0", FlashAttn: "on"}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []PoolConfig{
		{Name: "Big", Model: "m"},
		{Name: "a", Model: ""},
		{Name: "a", Model: "m", Context: 100},
		{Name: "a", Model: "m", Parallel: 65},
		{Name: "a", Model: "m", Context: 512, Parallel: 8},
		{Name: "a", Model: "m", Workers: []string{w1, w1}},
		{Name: "a", Model: "m", Workers: []string{"laptop"}},
		{Name: "a", Model: "m", Split: map[string]float64{"local": 0}},
		{Name: "a", Model: "m", Split: map[string]float64{"local": -1, w1: 2}},
		{Name: "a", Model: "m", Split: map[string]float64{"elsewhere": 1}},
		{Name: "a", Model: "m", CacheTypeK: "q2_k"},
		{Name: "a", Model: "m", FlashAttn: "maybe"},
	}
	for _, p := range bad {
		if p.Validate() == nil {
			t.Errorf("accepted %+v", p)
		}
	}
	c := Default()
	c.Pools = []PoolConfig{{Name: "a", Model: "m", Workers: []string{w1}}, {Name: "b", Model: "m", Workers: []string{w2}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Pools[1].Workers = []string{w1}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "one pool") {
		t.Fatalf("a worker in two pools: %v", err)
	}
	c.Pools[1] = PoolConfig{Name: "a", Model: "m"}
	if c.Validate() == nil {
		t.Fatal("duplicate pool names accepted")
	}
	c.Pools = nil
	c.ModelDirs = []string{"relative/dir"}
	if c.Validate() == nil {
		t.Fatal("relative model folder accepted")
	}
}

func TestPoolListReadsOldSinglePool(t *testing.T) {
	c := Default()
	if c.PoolList() != nil {
		t.Fatal("no pools expected")
	}
	c.PoolModel, c.PoolContext = "qwen", 2048
	got := c.PoolList()
	if len(got) != 1 || got[0].Name != "main" || got[0].Model != "qwen" || got[0].Context != 2048 || !got[0].Enabled {
		t.Fatalf("%+v", got)
	}
	c.Pools = []PoolConfig{{Name: "x", Model: "y"}}
	if got := c.PoolList(); len(got) != 1 || got[0].Name != "x" {
		t.Fatalf("explicit pools must win: %+v", got)
	}
}
