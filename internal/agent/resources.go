package agent

import (
	"context"
	"meshcompute/internal/resource"
	"sync"
	"time"
)

// resourceCache keeps status requests fast. GPU discovery runs nvidia-smi,
// which can take several seconds on some drivers; running it for every status
// request made the host, the pool and the dashboard time each other out. At
// most one discovery runs at a time, and a recent snapshot is served while a
// newer one is collected in the background.
type resourceCache struct {
	mu         sync.Mutex
	snap       resource.Snapshot
	err        error
	at         time.Time
	refreshing chan struct{} // non-nil while a discovery runs
}

const resourceMaxAge = 4 * time.Second

func (c *resourceCache) get(ctx context.Context, dir string) (resource.Snapshot, error) {
	c.mu.Lock()
	fresh := !c.at.IsZero()
	if c.refreshing == nil && time.Since(c.at) > resourceMaxAge {
		done := make(chan struct{})
		c.refreshing = done
		go func() {
			// Not tied to one request: other requests wait on the same run.
			probe, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			snap, err := resource.Discover(probe, dir)
			cancel()
			c.mu.Lock()
			c.snap, c.err, c.at, c.refreshing = snap, err, time.Now(), nil
			c.mu.Unlock()
			close(done)
		}()
	}
	wait := c.refreshing
	snap, err := c.snap, c.err
	c.mu.Unlock()
	if fresh || wait == nil {
		return snap, err
	}
	// First call: nothing to serve yet.
	select {
	case <-wait:
	case <-ctx.Done():
		return resource.Snapshot{}, ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snap, c.err
}
