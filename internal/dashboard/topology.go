package dashboard

import (
	"context"
	"errors"
	"meshcompute/internal/agent"
	"meshcompute/internal/security"
	"net/http"
	"time"
)

// TopologyReader reads the group host's latency map.
type TopologyReader interface {
	Topology(ctx context.Context) (agent.Topology, error)
}

// Topology fetches the map from this computer's own agent (host only).
func (r *Remote) Topology(ctx context.Context) (agent.Topology, error) {
	var t agent.Topology
	if !r.c.GroupHost {
		return t, errors.New("the latency map is kept by the group host")
	}
	cl, err := security.Client(r.dir)
	if err != nil {
		return t, err
	}
	defer cl.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err = readJSON(ctx, cl, r.c.LocalEndpoint(), "/v1/topology", nil, &t)
	return t, err
}

func (s *Server) topologyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/topology", s.handleTopology)
}

func (s *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tr, ok := s.cluster.(TopologyReader)
	if !ok || s.worker != nil {
		writeError(w, http.StatusNotFound, "the latency map is kept by the group host")
		return
	}
	t, err := tr.Topology(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, cleanAgentError(err))
		return
	}
	writeJSON(w, t)
}
