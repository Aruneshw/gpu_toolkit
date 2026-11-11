package dashboard

import (
	"encoding/base64"
	"meshcompute/internal/sse"
	"meshcompute/internal/terminal"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// maxMulti bounds the sessions one multiplexed stream follows.
const maxMulti = terminal.MaxSessions

func (s *Server) gridRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/term/list", s.handleTermList)
	mux.HandleFunc("/api/term/multi", s.handleTermMulti)
}

// handleTermList reports the running shells and the computer each runs on.
func (s *Server) handleTermList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.term == nil {
		writeJSON(w, map[string]any{"sessions": []terminal.SessionInfo{}})
		return
	}
	writeJSON(w, map[string]any{"sessions": s.term.List()})
}

type follower struct {
	id     string
	sess   *terminal.Session
	offset int64
	detach func()
	ended  bool
}

// handleTermMulti relays the output of several shells over one stream of
// server-sent events: ?s=ID:OFFSET,ID:OFFSET. Browsers allow only about six
// connections per site, so a grid of terminals must share one.
func (s *Server) handleTermMulti(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.term == nil {
		writeError(w, http.StatusNotFound, "terminal is not enabled on this dashboard")
		return
	}
	var list []*follower
	var gone []string
	for _, part := range strings.Split(r.URL.Query().Get("s"), ",") {
		if part == "" {
			continue
		}
		if len(list)+len(gone) >= maxMulti {
			writeError(w, http.StatusBadRequest, "too many sessions in one stream")
			return
		}
		id, off, _ := strings.Cut(part, ":")
		offset, err := strconv.ParseInt(off, 10, 64)
		if err != nil {
			offset = -1
		}
		sess, ok := s.term.Get(id)
		if !ok {
			gone = append(gone, id)
			continue
		}
		list = append(list, &follower{id: id, sess: sess, offset: offset, detach: sess.Attach()})
	}
	defer func() {
		for _, f := range list {
			f.detach()
		}
	}()
	sse.Prepare(w)
	sw := sse.NewWriter(w)
	for _, id := range gone {
		if sw.Event("exit", map[string]any{"id": id, "code": -1, "gone": true}) != nil {
			return
		}
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		cases := []reflect.SelectCase{
			{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(r.Context().Done())},
			{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(heartbeat.C)},
		}
		live := 0
		for _, f := range list {
			if f.ended {
				continue
			}
			data, next, wake, exited := f.sess.Read(f.offset)
			for len(data) > 0 {
				chunk := data
				if len(chunk) > 48<<10 {
					chunk = chunk[:48<<10]
				}
				data = data[len(chunk):]
				if err := sw.Event("output", map[string]any{"id": f.id, "data": base64.StdEncoding.EncodeToString(chunk), "offset": next - int64(len(data))}); err != nil {
					return
				}
			}
			f.offset = next
			if exited {
				f.ended = true
				if sw.Event("exit", map[string]any{"id": f.id, "code": f.sess.ExitCode()}) != nil {
					return
				}
				continue
			}
			live++
			cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(wake)})
		}
		if live == 0 && len(list) > 0 {
			return
		}
		chosen, _, _ := reflect.Select(cases)
		switch chosen {
		case 0:
			return
		case 1:
			if sw.Event("ping", map[string]bool{"ok": true}) != nil {
				return
			}
		}
	}
}
