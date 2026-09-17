// route_smart.go — live-turn slot semaphore with a FIFO waiter queue
// behind the ROUTING_SMART master switch.
//
// TOKEN_MAX_CONCURRENT (default 2; 0 = unlimited) is a hard wall per
// account on live turns: a lease is granted only while the token holds
// fewer live turns than the cap; LeaseRelease/LeaseAbandon returns the
// slot and wakes the FIFO head. QUEUE_WAIT (default 30s) deadline and the
// QUEUE_DEPTH (default 16) cap. Overflow and timeout return the typed
// queue-exhausted signal below, which the failover loop maps to the
// existing 429 rate-limit shape — never a new client error code.
// ROUTING_SMART off == the legacy path: the loop's slot hooks are skipped.
// Bridge mode gets the SAME hard wall: one slot state per bridge entry
// (keyed by *bridgeEntry exactly as a pooled lane is keyed by
// *tokenEntry). With ROUTING_SMART off the bridge path keeps its
// per-entry single-flight alone (no slots, no queueing), exactly as before.
//
// Slot/queue state is in-memory only and resets to zero on restart: it
// rides no pool_state rows and invents no SQL.
package pool

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/upstream"
	"sync/atomic"
	"time"
)

// routeQueueHint is the Retry-After hint carried by queue-exhausted 429s: a
// minimal local backoff (failover to a free token is the first resort, so
// the hint only paces single-token pools).
const routeQueueHint = time.Second

// routeQueueExhaustedError is the typed queue-exhausted signal: a lane's
// live-turn slots were full and the waiter either found a full queue
// (reason "full") or ran out of QUEUE_WAIT (reason "timeout"). The failover
// loop maps it to the existing 429 rate-limit shape; it never reaches a
// client verbatim. Token is the 1-based pooled display index; 0 means the
// lane has no index to name (bridge entries), and the message then omits
// the token scope.
type routeQueueExhaustedError struct {
	Reason string // "full" or "timeout"
	Token  int    // 1-based display index; 0 = unnamed lane (bridge)
	Cap    int    // live-turn cap in force
	Live   int    // live turns observed
	Wait   time.Duration
}

func (e *routeQueueExhaustedError) Error() string {
	scope := ""
	if e.Token > 0 {
		scope = fmt.Sprintf("token-%d ", e.Token)
	}
	if e.Reason == "timeout" {
		return fmt.Sprintf("pool: %slive-turn queue wait (%s) elapsed with %d live turns (cap %d)", scope, e.Wait, e.Live, e.Cap)
	}
	return fmt.Sprintf("pool: %slive-turn queue full (%d live turns, cap %d)", scope, e.Live, e.Cap)
}

// routeSlotParams resolves the live slot cap, queue depth and wait bound
// for one acquire. A nil config yields the documented defaults; the loader
// floors a negative TOKEN_MAX_CONCURRENT to 0 and rejects negative
// QUEUE_DEPTH, so the defensive branches below only fire for hand-built
// configs that bypass Load (unit tests). A cap <= 0 means UNLIMITED: the
// acquire hook skips slot gating entirely (no counter, no queue).
func routeSlotParams(cfg *config.Config) (cap, depth int, wait time.Duration) {
	cap, depth, wait = 2, 16, 30*time.Second
	if cfg == nil {
		return cap, depth, wait
	}
	cap = cfg.TokenMaxConcurrent
	if cfg.QueueDepth >= 0 {
		depth = cfg.QueueDepth
	}
	if cfg.QueueWait > 0 {
		wait = cfg.QueueWait
	}
	return cap, depth, wait
}

// routeSlotWaiter is one parked FIFO waiter. ch is closed exactly once on
// grant (under Pool.routeMu); granted is set in the same critical section
// so a concurrent timeout/ctx-expiry either takes the grant or dequeues,
// never both and never neither. at is the arrival instant, read under
// routeMu to report how long the oldest waiter has been parked.
type routeSlotWaiter struct {
	ch      chan struct{}
	granted bool
	element *list.Element
	at      time.Time
}

// routeSlotState is one token's live-turn counter plus its FIFO waiter
// queue (arrival order = grant order).
type routeSlotState struct {
	live    int
	waiters *list.List // of *routeSlotWaiter, front = head
}

// routeSlotPermit is one held live-turn slot. Release returns it: with a
// non-empty queue the slot transfers directly to the FIFO head (the live
// count is unchanged — a third live turn never exists); otherwise the live
// count decrements. Nil-safe (legacy/off-path and synthetic leases carry
// no permit) and idempotent.
type routeSlotPermit struct {
	pool     *Pool
	key      any // *tokenEntry (pooled) or *bridgeEntry (bridge)
	released atomic.Bool
}

// Release returns the live-turn slot, waking the FIFO head when waiters
// park. Nil-safe and idempotent.
func (s *routeSlotPermit) Release() {
	if s == nil || s.pool == nil || !s.released.CompareAndSwap(false, true) {
		return
	}
	p := s.pool
	p.routeMu.Lock()
	defer p.routeMu.Unlock()
	st, ok := p.routeSlots[s.key]
	if !ok || st == nil {
		return
	}
	if front := st.waiters.Front(); front != nil {
		w := front.Value.(*routeSlotWaiter)
		st.waiters.Remove(front)
		w.granted = true
		close(w.ch)
		return
	}
	if st.live > 0 {
		st.live--
	}
	if st.live == 0 {
		delete(p.routeSlots, s.key)
	}
}

// routeSlotStateLocked returns the key's slot state, creating it. Caller
// holds p.routeMu.
func (p *Pool) routeSlotStateLocked(key any) *routeSlotState {
	if p.routeSlots == nil {
		p.routeSlots = make(map[any]*routeSlotState)
	}
	st, ok := p.routeSlots[key]
	if !ok || st == nil {
		st = &routeSlotState{waiters: list.New()}
		p.routeSlots[key] = st
	}
	if st.waiters == nil {
		st.waiters = list.New()
	}
	return st
}

// routeSlotAcquire takes one live-turn slot for the key — a pooled
// *tokenEntry or a bridge *bridgeEntry — parking FIFO when full. The fast
// path (free slot) grants immediately; otherwise the caller queues behind
// earlier waiters until the head is granted, the caller ctx expires, or
// wait elapses. It returns the permit, whether the caller parked, and
// either a *routeQueueExhaustedError (full queue or wait elapsed — the
// caller maps it to the existing 429 shape) or ctx.Err() (the caller's own
// deadline, matching the retired gates' behavior). displayIdx is the
// pooled 1-based token number for the error message; bridge passes 0.
func (p *Pool) routeSlotAcquire(ctx context.Context, key any, displayIdx int, cap, depth int, wait time.Duration) (*routeSlotPermit, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cap < 1 {
		cap = 1
	}
	p.routeMu.Lock()
	st := p.routeSlotStateLocked(key)
	if st.live < cap {
		st.live++
		p.routeMu.Unlock()
		return &routeSlotPermit{pool: p, key: key}, false, nil
	}
	if depth <= 0 || st.waiters.Len() >= depth {
		qerr := &routeQueueExhaustedError{Reason: "full", Token: displayIdx, Cap: cap, Live: st.live}
		p.routeMu.Unlock()
		return nil, false, qerr
	}
	w := &routeSlotWaiter{ch: make(chan struct{}), at: time.Now()}
	w.element = st.waiters.PushBack(w)
	live := st.live
	p.routeMu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-w.ch:
		return &routeSlotPermit{pool: p, key: key}, true, nil
	case <-ctx.Done():
		p.routeMu.Lock()
		if w.granted {
			p.routeMu.Unlock()
			return &routeSlotPermit{pool: p, key: key}, true, nil
		}
		if w.element != nil {
			st.waiters.Remove(w.element)
			w.element = nil
		}
		p.routeMu.Unlock()
		return nil, true, ctx.Err()
	case <-timer.C:
		p.routeMu.Lock()
		if w.granted {
			p.routeMu.Unlock()
			return &routeSlotPermit{pool: p, key: key}, true, nil
		}
		if w.element != nil {
			st.waiters.Remove(w.element)
			w.element = nil
		}
		p.routeMu.Unlock()
		return nil, true, &routeQueueExhaustedError{Reason: "timeout", Token: displayIdx, Cap: cap, Live: live, Wait: wait}
	}
}

// routeSlotLive reports the key's current live-turn count (tests and the
// scorer's free-slot partition).
func (p *Pool) routeSlotLive(key any) int {
	p.routeMu.Lock()
	defer p.routeMu.Unlock()
	if st, ok := p.routeSlots[key]; ok && st != nil {
		return st.live
	}
	return 0
}

// routeSlotQueued reports the key's parked waiter count (tests).
func (p *Pool) routeSlotQueued(key any) int {
	p.routeMu.Lock()
	defer p.routeMu.Unlock()
	if st, ok := p.routeSlots[key]; ok && st != nil && st.waiters != nil {
		return st.waiters.Len()
	}
	return 0
}

// routeSlotStats reports the key's live-turn count, parked waiter count and
// how long the oldest (front) waiter has been parked — zero when nothing is
// queued. This is the telemetry view of one lane: TokenSnapshot rides it so
// the dashboard can show a saturated account (live turns at the cap with
// waiters behind them) versus a free one.
func (p *Pool) routeSlotStats(key any) (live, queued int, oldestWait time.Duration) {
	p.routeMu.Lock()
	defer p.routeMu.Unlock()
	st, ok := p.routeSlots[key]
	if !ok || st == nil {
		return 0, 0, 0
	}
	if st.waiters == nil {
		return st.live, 0, 0
	}
	queued = st.waiters.Len()
	if queued > 0 {
		if head, ok := st.waiters.Front().Value.(*routeSlotWaiter); ok && !head.at.IsZero() {
			oldestWait = time.Since(head.at)
		}
	}
	return st.live, queued, oldestWait
}

// routeQueueRateLimit maps a queue-exhausted signal to the existing 429
// rate-limit shape (failover bucket): same code the pool surfaces for its
// per-minute cap, with the local queue hint as Retry-After.
func routeQueueRateLimit(qerr *routeQueueExhaustedError, model string, cap int, live int) *upstream.RateLimitError {
	body := ""
	if qerr != nil {
		body = qerr.Error()
	}
	return &upstream.RateLimitError{
		Status:      "rate_limited",
		Model:       model,
		RetryAfter:  routeQueueHint,
		Limit:       float64(cap),
		RecentCount: float64(live),
		Body:        body,
	}
}

// routeIsQueueExhausted reports whether err is the typed queue-exhausted
// signal (overflow or wait timeout), as opposed to the caller's own ctx
// expiry.
func routeIsQueueExhausted(err error) bool {
	var qerr *routeQueueExhaustedError
	return errors.As(err, &qerr)
}
