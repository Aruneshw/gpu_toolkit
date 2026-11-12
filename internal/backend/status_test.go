package backend

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func reply(body string) *http.Response {
	return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(body))}
}

func TestErrorText(t *testing.T) {
	cases := map[string]string{
		"every request slot of the pools serving qwen is busy; retry shortly\n": "every request slot of the pools serving qwen is busy; retry shortly",
		"first line\nsecond line":         "first line",
		"  padded  ":                      "padded",
		"bell\x07 and \x1b[31mescape":     "bell and [31mescape",
		"":                                "",
		"<html><body>proxy</body></html>": "",
		`{"error":"structured"}`:          "",
		strings.Repeat("x", 2000):         strings.Repeat("x", maxErrorText) + "…",
	}
	for in, want := range cases {
		if got := ErrorText(reply(in)); got != want {
			t.Errorf("ErrorText(%.40q) = %.60q, want %.60q", in, got, want)
		}
	}
}
