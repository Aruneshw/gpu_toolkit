package agent

import "time"

// SetPathFilter hides offered paths from one worker in end-to-end tests:
// keep reports whether the worker with state directory dir may use endpoint.
func SetPathFilter(keep func(dir, endpoint string) bool) (restore func()) {
	old := pathFilter.Swap(&keep)
	return func() { pathFilter.Store(old) }
}

// SetTopologyInterval shortens how often the host refreshes its map; agents
// started afterwards use it.
func SetTopologyInterval(d time.Duration) (restore func()) {
	old := topoStatus.Swap(int64(d))
	return func() { topoStatus.Store(old) }
}
