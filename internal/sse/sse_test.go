package sse

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadEvents(t *testing.T) {
	var got []string
	err := Read(strings.NewReader("event: token\ndata: {\"a\":1}\n\nevent: done\ndata: {}\n\n"), func(event string, data []byte) error {
		got = append(got, event+":"+string(data))
		if event == "done" {
			return Done
		}
		return nil
	})
	if err != nil || strings.Join(got, "|") != `token:{"a":1}|done:{}` {
		t.Fatalf("%v %v", got, err)
	}
}

func TestReadCRLFAndComments(t *testing.T) {
	var got []string
	err := Read(strings.NewReader(": comment\r\nevent: token\r\ndata: hello\r\n\r\n"), func(event string, data []byte) error {
		got = append(got, string(data))
		return Done
	})
	if err != nil || len(got) != 1 || got[0] != "hello" {
		t.Fatalf("%v %v", got, err)
	}
}

func TestReadTruncated(t *testing.T) {
	if err := Read(strings.NewReader("event: token\ndata: x\n\n"), func(string, []byte) error { return nil }); err == nil {
		t.Fatal("accepted a stream without completion")
	}
}

func TestReadOversized(t *testing.T) {
	line := "event: token\ndata: " + strings.Repeat("x", maxLineBytes+1) + "\n\n"
	if err := Read(strings.NewReader(line), func(string, []byte) error { return nil }); err == nil {
		t.Fatal("accepted a line over the limit")
	}
}

func TestReadHandlerError(t *testing.T) {
	stop := errors.New("stop")
	err := Read(strings.NewReader("event: token\ndata: x\n\n"), func(string, []byte) error { return stop })
	if !errors.Is(err, stop) {
		t.Fatalf("%v", err)
	}
}

func TestWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	Prepare(rec)
	w := NewWriter(rec)
	if err := w.Event("token", map[string]string{"content": "hi"}); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: token\n") || !strings.Contains(body, `"content":"hi"`) || !strings.HasSuffix(body, "\n\n") {
		t.Fatalf("body %q", body)
	}
	if rec.Header().Get("Content-Type") != "text/event-stream" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("headers %+v", rec.Header())
	}
}
