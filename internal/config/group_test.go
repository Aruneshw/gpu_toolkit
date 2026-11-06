package config

import "testing"

func TestPublicGroupOptIn(t *testing.T) {
	c := Default()
	c.Listen = "[2001:db8::1]:7443"
	c.Advertise = "https://" + c.Listen
	if c.Validate() == nil {
		t.Fatal("public address accepted by legacy default")
	}
	c.Public = true
	if c.Validate() == nil {
		t.Fatal("public mode accepted without group hosting")
	}
	c.GroupHost = true
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Listen = "192.168.1.20:7443"
	if err := c.Validate(); err != nil {
		t.Fatalf("explicit forwarded address: %v", err)
	}
	c.BackendURL = "http://8.8.8.8:8080"
	if c.Validate() == nil {
		t.Fatal("public backend allowed")
	}
	for _, s := range []string{"https://0.0.0.0:7443", "https://[::]:7443", "https://[ff02::1]:7443", "https://[fe80::1]:7443", "https://example.com:7443", "https://8.8.8.8:7443/path", "https://8.8.8.8:7443?", "https://user@8.8.8.8:7443"} {
		if RemoteEndpoint(s, true) == nil {
			t.Errorf("accepted unsafe endpoint %q", s)
		}
	}
}
