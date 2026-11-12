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
	waited     bool          // a request already gave up waiting for the first one
}

const resourceMaxAge = 4 * time.Second

// firstWait bounds how long a status request waits for the very first
// discovery; after that it answers with resource.Basic until discovery ends.
var firstWait = 2 * time.Second

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
	waited := c.waited
	c.mu.Unlock()
	if fresh || wait == nil {
		return snap, err
	}
	// First discovery still running: wait a little, then answer with what is
	// instant rather than let a slow nvidia-smi make this computer look
	// unreachable.
	if waited {
		return resource.Basic(ctx, dir), nil
	}
	timer := time.NewTimer(firstWait)
	defer timer.Stop()
	select {
	case <-wait:
	case <-timer.C:
		c.mu.Lock()
		c.waited = true
		c.mu.Unlock()
		return resource.Basic(ctx, dir), nil
	case <-ctx.Done():
		return resource.Snapshot{}, ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snap, c.err
}
