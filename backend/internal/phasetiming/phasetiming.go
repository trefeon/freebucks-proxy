// Package phasetiming carries per-request latency phases through the
// request context so each layer of the request path can record the phases
// it observes without depending on the others: the server records
// acquire/ttfb/total around its own calls, while the pool (and the session
// manager beneath it) records the session-refresh and run-acquire phases it
// performs inside Acquire. Zero dependencies beyond the standard library.
//
// Phases are recorded as millisecond durations computed from a caller-
// supplied start instant (Since), mirroring the upstream reference
// implementation (freebuff-reverse internal/orchestration/runner.go:
// session_acquire_ms / prepare_total_ms / transport_ttfb_ms). Recording is
// goroutine-safe, but in practice every writer runs on the request
// goroutine, so a phase is written exactly once per request.
package phasetiming

import (
	"context"
	"sync"
	"time"
)

// Phase names recorded by the request path.
const (
	// AcquireMS is the pool token acquisition call (session admission +
	// run acquire), recorded by the server around pool.Acquire.
	AcquireMS = "acquire_ms"
	// SessionRefreshMS is the upstream session EnsureSessionForModel call
	// inside Acquire, recorded by the pool per token attempt (last attempt
	// wins — that is the one that produced the lease).
	SessionRefreshMS = "session_refresh_ms"
	// RunAcquireMS is the run-slot acquire inside Acquire, recorded by the
	// pool per token attempt.
	RunAcquireMS = "run_acquire_ms"
	// QueueWaitMS is how long the request sat parked in an account's FIFO
	// live-turn queue (TOKEN_MAX_CONCURRENT slot wall, route_smart.go)
	// before a slot was granted, recorded by the pool around the slot
	// acquire. It is written ONLY when the request actually parked AND was
	// granted: a request that never queued — or one whose QUEUE_WAIT
	// elapsed without a slot — carries no such phase at all, so the
	// console's "no wait" case is unambiguous. This is queue time, not
	// acquire time: acquire_ms stays the whole pool.Acquire call.
	QueueWaitMS = "queue_wait_ms"
	// UpstreamTTFBMS is the time from the upstream chat call until the
	// first relayed chunk, recorded by the server in the relay loop.
	UpstreamTTFBMS = "upstream_ttfb_ms"
	// TotalMS is the whole request duration, recorded by the server.
	TotalMS = "total_ms"
)

type ctxKey struct{}

// Phases accumulates per-request latency phases. The zero value is not
// usable; construct with New or WithContext.
type Phases struct {
	start  time.Time
	mu     sync.Mutex
	phases map[string]int64
}

// New returns a Phases accumulator with its start pinned to now (total_ms
// is measured against this instant).
func New() *Phases {
	return &Phases{start: time.Now(), phases: make(map[string]int64)}
}

// WithContext installs a fresh Phases accumulator into ctx and returns both,
// so callers can record into it directly without a FromContext round-trip.
func WithContext(ctx context.Context) (context.Context, *Phases) {
	p := New()
	return context.WithValue(ctx, ctxKey{}, p), p
}

// FromContext returns the request's Phases accumulator, or nil when the
// context carries none (callers treat nil as a no-op: every method on a nil
// receiver is safe).
func FromContext(ctx context.Context) *Phases {
	if p, ok := ctx.Value(ctxKey{}).(*Phases); ok {
		return p
	}
	return nil
}

// Since records the duration of a phase that started at start, in
// milliseconds. A nil receiver is a no-op.
func (p *Phases) Since(name string, start time.Time) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.phases[name] = time.Since(start).Milliseconds()
}

// Start returns the request start instant (for total_ms and relative
// phases). The zero value on a nil receiver.
func (p *Phases) Start() time.Time {
	if p == nil {
		return time.Time{}
	}
	return p.start
}

// All returns a copy of the recorded phases keyed by name.
func (p *Phases) All() map[string]int64 {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]int64, len(p.phases))
	for k, v := range p.phases {
		out[k] = v
	}
	return out
}
