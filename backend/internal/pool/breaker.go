// breaker.go - per-(model, account) circuit breaker gating pool attempts.
//
// Precedent: llmrelay per-(provider,model) Breaker (30s window, 0.5
// threshold, 30s open, single-probe half-open) and the jdanzig
// consecutive-failure counter with pre-commit stream failover. This file
// takes the breaker-gates-each-attempt discipline from both; the counting
// rule below is an explicit choice between the two precedents:
//
// Chosen: consecutive-failure counting (jdanzig style), NOT a rolling
// window. Reasons: (1) no per-event timestamps or window sweeps on the
// Acquire hot path — one counter per (model, account); (2) a single success
// resets the count, which matches upstream refusal bursts (a 429 storm then
// recovery) better than a window that keeps punishing a recovered lane;
// (3) the pool already keeps windowed refusal memory elsewhere
// (runs.RememberModelRateLimit, cooldown windows) — the breaker is the
// fail-fast overlay, not the ledger.
//
// States: closed (attempts flow) → open after breakerFailThreshold
// consecutive failures (attempts rejected for breakerOpenTimeout) →
// half-open single probe (exactly one attempt flows; success closes,
// failure re-opens). A fresh breaker is closed, so the gate is a no-op on
// green paths: failover only ever triggers after failures.
package pool

import (
	"sync"
	"time"
)

const (
	// breakerFailThreshold is the consecutive-failure count that opens
	// the breaker for one (model, account) lane.
	breakerFailThreshold = 3
	// breakerOpenTimeout is how long an open breaker rejects attempts
	// before admitting a single half-open probe.
	breakerOpenTimeout = 30 * time.Second
)

// breakerState is one lane's gate position.
type breakerState int

const (
	breakerClosed breakerState = iota
	breakerOpen
	breakerHalfOpen
)

// Breaker gates attempts on one (model, account) lane. Zero value is
// usable (closed, system clock).
type Breaker struct {
	mu            sync.Mutex
	consecutive   int
	state         breakerState
	openedAt      time.Time
	probeInFlight bool
	now           func() time.Time
}

func (b *Breaker) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// Allow reports whether an attempt may flow. An open breaker whose timeout
// elapsed promotes to half-open and admits exactly one probe; further
// callers wait for the probe's verdict.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock()
	switch b.state {
	case breakerClosed:
		return true
	case breakerHalfOpen:
		return false
	case breakerOpen:
		if now.Sub(b.openedAt) >= breakerOpenTimeout {
			b.state = breakerHalfOpen
			b.probeInFlight = true
			return true
		}
		return false
	}
	return true
}

// RecordSuccess closes the breaker: the consecutive count resets and a
// half-open probe verdict of healthy restores full flow.
func (b *Breaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutive = 0
	b.state = breakerClosed
	b.probeInFlight = false
}

// RecordFailure counts one lane failure: at the threshold the breaker
// opens, and a failed half-open probe re-opens with a fresh timeout.
func (b *Breaker) RecordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutive++
	if b.state == breakerHalfOpen {
		b.state = breakerOpen
		b.openedAt = b.clock()
		b.probeInFlight = false
		return
	}
	if b.consecutive >= breakerFailThreshold {
		if b.state != breakerOpen {
			b.openedAt = b.clock()
		}
		b.state = breakerOpen
	}
}

// State reports the gate position for logs and metrics:
// "closed", "open", or "half-open".
func (b *Breaker) State() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	// A timed-out open breaker reads half-open: the next Allow admits
	// the probe, so reporting it open would lie about the gate.
	if b.state == breakerOpen && b.clock().Sub(b.openedAt) >= breakerOpenTimeout {
		return "half-open"
	}
	switch b.state {
	case breakerOpen:
		return "open"
	case breakerHalfOpen:
		return "half-open"
	default:
		return "closed"
	}
}

// breakerKey identifies one gated lane: the requested model plus the
// roster index serving it. A dashboard reorder re-keys attribution —
// acceptable for a transient 30s fail-fast overlay that never persists.
type breakerKey struct {
	model   string
	account int
}

// BreakerRegistry owns one Breaker per (model, account) lane. Safe for
// concurrent use; breakers are created lazily on first touch.
type BreakerRegistry struct {
	mu       sync.Mutex
	breakers map[breakerKey]*Breaker
	now      func() time.Time
}

// NewBreakerRegistry builds an empty registry on the system clock.
func NewBreakerRegistry() *BreakerRegistry {
	return &BreakerRegistry{breakers: make(map[breakerKey]*Breaker)}
}

// For returns the lane's breaker, creating it closed on first touch.
func (r *BreakerRegistry) For(model string, account int) *Breaker {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := breakerKey{model: model, account: account}
	if b, ok := r.breakers[k]; ok {
		return b
	}
	b := &Breaker{now: r.now}
	r.breakers[k] = b
	return b
}

// LaneCount reports how many lanes have breakers (test/metrics seam).
func (r *BreakerRegistry) LaneCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.breakers)
}
