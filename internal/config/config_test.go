package config

import (
	"os"
	"path/filepath"
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
