package egress

import (
	"context"
	"sync"
	"time"
)

// Tracker keeps the detected egress region fresh in the background: it
// probes one Path on an interval, records every probe through the shared
// Cache, and exposes the last known result to callers that must not touch
// the network (the request path).
//
// Lifecycle: Start blocks until ctx is done. It probes once immediately,
// then once per interval, and returns only after the probe loop has exited,
// so no goroutine outlives it. The in-flight probe is cancelled with ctx, so
// shutdown does not wait out a ProbeTimeout. Each Start runs one loop;
// probes additionally hold a mutex, so even if Start were called twice from
// two goroutines a slow probe could never run concurrently with another.
//
// Reads never probe and never block on the network: Country and Result are
// served from the tracker's own snapshot (refreshed by every probe), and
// fall back to the shared Cache for a result some other surface stored
// first. The snapshot is what keeps the answer stable across a cache TTL
// boundary — a resolver asking mid-interval gets the last detected region
// rather than a blank.
type Tracker struct {
	cache    *Cache
	path     Path
	interval time.Duration

	probeMu sync.Mutex // serializes probes; a slow probe is never doubled

	mu      sync.RWMutex
	last    Result
	have    bool
	country string // last successful non-empty country, "" until one lands
}

// NewTracker returns a Tracker that probes path through the given cache
// every interval. A non-positive interval means DefaultTTL, the same
// cadence the cache treats as fresh.
func NewTracker(cache *Cache, path Path, interval time.Duration) *Tracker {
	if interval <= 0 {
		interval = DefaultTTL
	}
	return &Tracker{cache: cache, path: path, interval: interval}
}

// Start probes immediately, then every interval, until ctx is cancelled. It
// blocks for the lifetime of the tracker and returns promptly once ctx is
// done; see Tracker for the concurrency guarantees.
func (t *Tracker) Start(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTimer(0) // fire immediately
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			t.probe(ctx)
			timer.Reset(t.interval)
		}
	}()
	<-ctx.Done()
	<-done
}

// probe runs one egress probe and records it. It holds probeMu so two loops
// can never have a probe in flight at once, and skips the network entirely
// once ctx is done so shutdown does not trigger a fresh request.
func (t *Tracker) probe(ctx context.Context) {
	t.probeMu.Lock()
	defer t.probeMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	t.record(Probe(ctx, t.path.Dialer, ProbeTimeout))
}

// record stores a probe result in the shared cache and in the tracker's
// snapshot. A failed probe updates Result (the doctor's error surface) but
// never erases the last successfully detected country: a transient network
// blip does not mean the gateway's egress region moved.
func (t *Tracker) record(r Result) {
	t.cache.Set(t.path.Key, r)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.last = r
	t.have = true
	if r.Err == nil && r.Country != "" {
		t.country = r.Country
	}
}

// Country returns the last country code detected on this path ("" when
// unknown). It never probes and never blocks on the network.
func (t *Tracker) Country() string {
	t.mu.RLock()
	country := t.country
	t.mu.RUnlock()
	if country != "" {
		return country
	}
	if r, ok := t.cache.Get(t.path.Key); ok && r.Err == nil {
		return r.Country
	}
	return ""
}

// Result returns the most recent probe result for this path and whether any
// probe has been recorded yet. It never probes and never blocks on the
// network.
func (t *Tracker) Result() (Result, bool) {
	t.mu.RLock()
	last, have := t.last, t.have
	t.mu.RUnlock()
	if have {
		return last, true
	}
	return t.cache.Get(t.path.Key)
}
