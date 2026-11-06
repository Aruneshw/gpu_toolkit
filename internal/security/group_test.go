package security

import (
	"path/filepath"
	"testing"
	"time"
)

func TestGroupIdentityAdvertiseAndOptIn(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "group")
	if InitGroup(dir, "127.0.0.1:7443", "https://[2001:db8::1]:7443", false) == nil {
		t.Fatal("public advertised address accepted without opt-in")
	}
	if err := InitGroup(dir, "127.0.0.1:7443", "https://[2001:db8::1]:7443", true); err != nil {
		t.Fatal(err)
	}
	code, err := Pair(dir, "https://[2001:db8::1]:7443", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	inv := decode(t, code)
	if !inv.Reverse || !inv.Public {
		t.Fatal("group settings absent from invitation")
	}
	s, err := load(dir)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := parseCert(s.Cert)
	if err != nil {
		t.Fatal(err)
	}
	if leaf.VerifyHostname("127.0.0.1") != nil || leaf.VerifyHostname("2001:db8::1") != nil {
		t.Fatal("listen/advertise certificate names missing")
	}
	if _, err := Pair(dir, "https://[2001:db8::2]:7443", time.Minute); err == nil {
		t.Fatal("unrelated endpoint accepted")
	}
}
