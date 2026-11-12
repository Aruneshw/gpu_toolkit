package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A refused run must tell the person why (busy slots, context too large),
// not only the HTTP status.
func TestRequestShowsAgentReason(t *testing.T) {
	const why = "every request slot of the pools serving qwen is busy; retry shortly"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/plain" {
			w.WriteHeader(503)
			return
		}
		http.Error(w, why, 503)
	}))
	defer srv.Close()
	var out map[string]any
	err := request(context.Background(), srv.Client(), srv.URL, "/v1/run", map[string]string{"model": "qwen"}, &out)
	if err == nil || !strings.Contains(err.Error(), why) || !strings.Contains(err.Error(), "503") {
		t.Fatalf("error does not carry the agent's reason: %v", err)
	}
	err = request(context.Background(), srv.Client(), srv.URL, "/plain", nil, &out)
	if err == nil || !strings.Contains(err.Error(), "check backend readiness") {
		t.Fatalf("an empty refusal should keep the generic hint: %v", err)
	}
}
