package backend

import (
	"io"
	"net/http"
	"strings"
	"unicode"
)

// maxErrorText bounds the part of an agent's error reply shown to a person.
const maxErrorText = 300

// ErrorText is the agent's own explanation from a non-200 reply (for example
// "every request slot of the pools serving qwen is busy; retry shortly"):
// the first line of a short plain-text body with control characters removed,
// or "" when the reply carries none. It reads at most a few hundred bytes.
func ErrorText(res *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(res.Body, maxErrorText*4))
	s := strings.TrimSpace(string(b))
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if strings.HasPrefix(s, "<") || strings.HasPrefix(s, "{") {
		return "" // a page or JSON, not a message meant for people
	}
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			return -1
		}
		return r
	}, s)
	if r := []rune(s); len(r) > maxErrorText {
		s = string(r[:maxErrorText]) + "…"
	}
	return s
}
