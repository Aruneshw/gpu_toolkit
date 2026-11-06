// Package sse implements the small subset of server-sent events used between
// the agent, the dashboard and llama.cpp. It deliberately keeps messages
// bounded and does not implement reconnection or event IDs.
package sse

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// MaxStreamBytes bounds the total decoded stream so a hostile or broken peer
// cannot make a reader allocate without limit.
const MaxStreamBytes = 8 << 20

const maxLineBytes = 1 << 20

// Done is returned by a Read handler to stop a stream cleanly.
var Done = errors.New("stream complete")

// Prepare sets the response headers required for an SSE response.
func Prepare(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
}

// Writer emits named events and flushes each one. A per-write deadline keeps a
// stalled client from holding a handler forever when the server has no global
// write timeout.
type Writer struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func NewWriter(w http.ResponseWriter) *Writer {
	return &Writer{w: w, rc: http.NewResponseController(w)}
}

func (s *Writer) Event(name string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_ = s.rc.SetWriteDeadline(time.Now().Add(time.Minute))
	// Unsupported deadlines (for example a recorder in tests) are harmless.
	if _, err = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, b); err != nil {
		return err
	}
	return s.rc.Flush()
}

// Raw writes a preformatted event. Callers must keep it bounded.
func (s *Writer) Raw(event, data string) error {
	_ = s.rc.SetWriteDeadline(time.Now().Add(time.Minute))
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return err
	}
	return s.rc.Flush()
}

// Read parses events until the handler returns Done, an error occurs, or the
// stream ends. An end without Done is an error so a truncated response can
// never be mistaken for a complete generation.
func Read(r io.Reader, handle func(event string, data []byte) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), maxLineBytes)
	event := ""
	total := 0
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		total += len(line) + 1
		if total > MaxStreamBytes {
			return errors.New("stream exceeds size limit")
		}
		if line == "" {
			event = ""
			continue
		}
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		err := handle(event, data)
		if errors.Is(err, Done) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return errors.New("stream ended without completion event")
}
