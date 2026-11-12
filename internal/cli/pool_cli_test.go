package cli

import (
	"meshcompute/internal/agent"
	"meshcompute/internal/backend"
	"strings"
	"testing"
)

func TestResolveWorker(t *testing.T) {
	a := "worker-aaaa" + "1111111111111111111111111111111111111111111111111111111111aa"[:60]
	b := "worker-aaab" + "2222222222222222222222222222222222222222222222222222222222bb"[:60]
	known := []string{a, b, a}
	if got, err := resolveWorker("aaaa", known); err != nil || got != a {
		t.Fatalf("%q %v", got, err)
	}
	if got, err := resolveWorker("worker-aaab", known); err != nil || got != b {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := resolveWorker("aaa", known); err == nil {
		t.Fatal("ambiguous prefix accepted")
	}
	if _, err := resolveWorker("zzz", known); err == nil {
		t.Fatal("unknown prefix accepted")
	}
	full := "worker-" + "c0ffee0000000000000000000000000000000000000000000000000000000000"
	if got, err := resolveWorker(full, known); err != nil || got != full {
		t.Fatalf("an offline worker's full ID must be accepted: %q %v", got, err)
	}
}

func TestAdmitRun(t *testing.T) {
	pooled := agent.Status{BackendReady: true, Busy: true, Backend: &backend.Capabilities{Models: []string{"a", "b"}, Context: 1024},
		Pools: []agent.PoolStatus{{Name: "p", State: "ready", Model: "b"}}}
	r := backend.Request{Model: "b", MaxTokens: 64, Context: 2048}
	if why := admitRun(pooled, nil, &r, false); why != "" || r.Context != 1024 {
		t.Fatalf("pooled model with a free slot: %q context %d", why, r.Context)
	}
	r = backend.Request{Model: "b", MaxTokens: 64, Context: 2048}
	if why := admitRun(pooled, nil, &r, true); !strings.Contains(why, "--context 1024") {
		t.Fatalf("explicit context too large: %q", why)
	}
	r = backend.Request{Model: "a", MaxTokens: 64, Context: 512}
	if why := admitRun(pooled, nil, &r, true); !strings.Contains(why, "busy") {
		t.Fatalf("plain backend busy: %q", why)
	}
	r = backend.Request{Model: "z", MaxTokens: 64, Context: 512}
	if why := admitRun(pooled, nil, &r, true); !strings.Contains(why, "serving: a, b") {
		t.Fatalf("unknown model: %q", why)
	}
	r = backend.Request{Model: "b", MaxTokens: 1500, Context: 2048}
	if why := admitRun(pooled, nil, &r, false); !strings.Contains(why, "max-tokens") {
		t.Fatalf("output larger than the slot: %q", why)
	}
}
