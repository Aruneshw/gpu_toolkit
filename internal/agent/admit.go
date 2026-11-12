package agent

import (
	"fmt"
	"meshcompute/internal/backend"
	"strings"
)

// CanServe checks that a computer with status st can answer r now: a ready
// server for r.Model (a pool or the plain backend), a free request slot,
// and enough context. Without an explicit context it lowers r.Context to
// what one request slot holds. It returns why not, or "".
func CanServe(st Status, r *backend.Request, explicitContext bool) string {
	if !st.BackendReady || st.Backend == nil {
		return "no model server is ready there; start one (pool start MODEL on a group host, or backend serve)"
	}
	served := false
	for _, m := range st.Backend.Models {
		served = served || m == r.Model
	}
	if !served {
		return fmt.Sprintf("model %s is not being served there (serving: %s)", r.Model, strings.Join(st.Backend.Models, ", "))
	}
	pooled := false
	for _, p := range st.Pools {
		pooled = pooled || p.State == "ready" && p.Model == r.Model
	}
	if r.Pool != "" {
		return canServePool(st, r, explicitContext)
	}
	if st.Busy && !pooled {
		return "that computer is busy with another request; retry shortly"
	}
	if !explicitContext && st.Backend.Context > 0 && st.Backend.Context < r.Context {
		r.Context = st.Backend.Context
	}
	if st.Backend.Context < r.Context {
		return fmt.Sprintf("context %d is more than one request slot holds there (%d); use --context %d", r.Context, st.Backend.Context, st.Backend.Context)
	}
	if r.MaxTokens >= r.Context {
		return fmt.Sprintf("--max-tokens %d must be below the context (%d)", r.MaxTokens, r.Context)
	}
	return ""
}

// canServePool checks a request for one named pool: it must exist on that
// host, serve the model, be ready, and its request slots must hold the context.
func canServePool(st Status, r *backend.Request, explicitContext bool) string {
	var names []string
	for _, p := range st.Pools {
		names = append(names, p.Name)
		if p.Name != r.Pool {
			continue
		}
		if p.Model != r.Model {
			return fmt.Sprintf("pool %s serves %s, not %s", p.Name, p.Model, r.Model)
		}
		if p.State != "ready" {
			return fmt.Sprintf("pool %s is not ready (%s)", p.Name, p.State)
		}
		slot := slotContext(p.Settings)
		if !explicitContext && slot < r.Context {
			r.Context = slot
		}
		if slot < r.Context {
			return fmt.Sprintf("context %d is more than one request slot of pool %s holds (%d); use --context %d", r.Context, p.Name, slot, slot)
		}
		if r.MaxTokens >= r.Context {
			return fmt.Sprintf("--max-tokens %d must be below the context (%d)", r.MaxTokens, r.Context)
		}
		return ""
	}
	if len(names) == 0 {
		return fmt.Sprintf("there is no pool named %s there; that computer runs no pools", r.Pool)
	}
	return fmt.Sprintf("there is no pool named %s there (pools: %s)", r.Pool, strings.Join(names, ", "))
}
