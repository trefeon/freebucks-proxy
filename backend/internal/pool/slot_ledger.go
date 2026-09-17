// slot_ledger.go — MASQ slot ledger: live-turn slot semaphore with a FIFO
// waiter queue, keyed per (account, model).
//
// SLOTS_PER_ACCOUNT (default 2; 0 = unlimited) is a hard wall per
// (account, model) on live turns: a lease is granted only while the token
// holds fewer live turns for that model than the cap; LeaseRelease/
// LeaseAbandon returns the slot and wakes the FIFO head. One account may
// hold 2 turns of model A and 2 turns of model B at the same time — lanes
// for different models never share counters or queues. QUEUE_WAIT
// (default 30s) deadline and the QUEUE_DEPTH (default 16) cap. Overflow
// and timeout return the typed queue-exhausted signal below, which the
// spill loop consumes — never a new client error code.
// Bridge mode gets the SAME hard wall: one slot state per (bridge entry,
// model) exactly as a pooled lane is keyed per (token entry, model).
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

// slotQueueHint is the Retry-After hint carried by end-of-chain
// queue-exhausted 429s: a minimal local backoff (spill to a free account
// is the first resort, so the hint only paces single-account pools).
const slotQueueHint = time.Second

// slotQueueExhaustedError is the typed queue-exhausted signal: a lane's
// live-turn slots were full and the waiter either found a full queue
// (reason "full") or ran out of QUEUE_WAIT (reason "timeout"). The spill
// loop consumes it; only when no lane is left does it surface as the
// existing 429 rate-limit shape — never verbatim. Token is the 1-based
// pooled display index; 0 means the lane has no index to name (bridge
// entries), and the message then omits the token scope.
type slotQueueExhaustedError struct {
	Reason string // "full" or "timeout"
	Token  int    // 1-based display index; 0 = unnamed lane (bridge)
	Cap    int    // live-turn cap in force
	Live   int    // live turns observed
	Wait   time.Duration
}

func (e *slotQueueExhaustedError) Error() string {
	scope := ""
	if e.Token > 0 {
		scope = fmt.Sprintf("token-%d ", e.Token)
	}
	if e.Reason == "timeout" {
		return fmt.Sprintf("pool: %slive-turn queue wait (%s) elapsed with %d live turns (cap %d)", scope, e.Wait, e.Live, e.Cap)
	}
	return fmt.Sprintf("pool: %slive-turn queue full (%d live turns, cap %d)", scope, e.Live, e.Cap)
}

// slotKey is one ledger lane: the entry (*tokenEntry pooled, *bridgeEntry
// bridge) plus the model. Slots and FIFO queues are tracked per lane, so
// one account's turns for model A never consume model B's cap.
type slotKey struct {
	entry any
	model string
}

// slotParams resolves the live slot cap, queue depth and wait bound for
// one acquire. A nil config yields the documented defaults; the loader
// floors a negative SLOTS_PER_ACCOUNT to 0 and rejects negative
// QUEUE_DEPTH, so the defensive branches below only fire for hand-built
// configs that bypass Load (unit tests). A cap <= 0 means UNLIMITED: the
// acquire hook skips slot gating entirely (no counter, no queue).
func slotParams(cfg *config.Config) (cap, depth int, wait time.Duration) {
	cap, depth, wait = 2, 16, 30*time.Second
	if cfg == nil {
		return cap, depth, wait
	}
	cap = cfg.SlotsPerAccount
	if cfg.QueueDepth >= 0 {
		depth = cfg.QueueDepth
	}
	if cfg.QueueWait > 0 {
		wait = cfg.QueueWait
	}
	return cap, depth, wait
}

// slotWaiter is one parked FIFO waiter. ch is closed exactly once on
// grant (under Pool.routeMu); granted is set in the same critical section
// so a concurrent timeout/ctx-expiry either takes the grant or dequeues,
// never both and never neither. at is the arrival instant, read under
// routeMu to report how long the oldest waiter has been parked.
type slotWaiter struct {
	ch      chan struct{}
	granted bool
	element *list.Element
	at      time.Time
}

// slotState is one lane's live-turn counter plus its FIFO waiter queue
// (arrival order = grant order).
type slotState struct {
	live    int
	waiters *list.List // of *slotWaiter, front = head
}

// slotPermit is one held live-turn slot. Release returns it: with a
// non-empty queue the slot transfers directly to the FIFO head (the live
// count is unchanged — a third live turn never exists); otherwise the live
// count decrements. Nil-safe (legacy/off-path and synthetic leases carry
// no permit) and idempotent.
type slotPermit struct {
	pool     *Pool
	key      slotKey
	released atomic.Bool
}

// Release returns the live-turn slot, waking the FIFO head when waiters
// park. Nil-safe and idempotent.
func (s *slotPermit) Release() {
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
		w := front.Value.(*slotWaiter)
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

// slotStateLocked returns the lane's slot state, creating it. Caller
// holds p.routeMu.
func (p *Pool) slotStateLocked(key slotKey) *slotState {
	if p.routeSlots == nil {
		p.routeSlots = make(map[slotKey]*slotState)
	}
	st, ok := p.routeSlots[key]
	if !ok || st == nil {
		st = &slotState{waiters: list.New()}
		p.routeSlots[key] = st
	}
	if st.waiters == nil {
		st.waiters = list.New()
	}
	return st
}

// slotAcquire takes one live-turn slot for the lane — a pooled token entry
// or a bridge entry paired with the requested model — parking FIFO when
// full. The fast path (free slot) grants immediately; otherwise the caller
// queues behind earlier waiters until the head is granted, the caller ctx
// expires, or wait elapses. It returns the permit, whether the caller
// parked, and either a *slotQueueExhaustedError (full queue or wait
// elapsed — the caller spills to the next lane) or ctx.Err() (the caller's
// own deadline, matching the retired gates' behavior). displayIdx is the
// pooled 1-based token number for the error message; bridge passes 0.
func (p *Pool) slotAcquire(ctx context.Context, key slotKey, displayIdx int, cap, depth int, wait time.Duration) (*slotPermit, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cap < 1 {
		cap = 1
	}
	p.routeMu.Lock()
	st := p.slotStateLocked(key)
	if st.live < cap {
		st.live++
		p.routeMu.Unlock()
		return &slotPermit{pool: p, key: key}, false, nil
	}
	if depth <= 0 || st.waiters.Len() >= depth {
		qerr := &slotQueueExhaustedError{Reason: "full", Token: displayIdx, Cap: cap, Live: st.live}
		p.routeMu.Unlock()
		return nil, false, qerr
	}
	w := &slotWaiter{ch: make(chan struct{}), at: time.Now()}
	w.element = st.waiters.PushBack(w)
	live := st.live
	laneWait := wait
	p.routeMu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-w.ch:
		return &slotPermit{pool: p, key: key}, true, nil
	case <-ctx.Done():
		p.routeMu.Lock()
		if w.granted {
			p.routeMu.Unlock()
			return &slotPermit{pool: p, key: key}, true, nil
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
			return &slotPermit{pool: p, key: key}, true, nil
		}
		if w.element != nil {
			st.waiters.Remove(w.element)
			w.element = nil
		}
		p.routeMu.Unlock()
		return nil, true, &slotQueueExhaustedError{Reason: "timeout", Token: displayIdx, Cap: cap, Live: live, Wait: laneWait}
	}
}

// slotLive reports the lane's current live-turn count (tests and
// telemetry).
func (p *Pool) slotLive(key slotKey) int {
	p.routeMu.Lock()
	defer p.routeMu.Unlock()
	if st, ok := p.routeSlots[key]; ok && st != nil {
		return st.live
	}
	return 0
}

// slotQueued reports the lane's parked waiter count (tests).
func (p *Pool) slotQueued(key slotKey) int {
	p.routeMu.Lock()
	defer p.routeMu.Unlock()
	if st, ok := p.routeSlots[key]; ok && st != nil && st.waiters != nil {
		return st.waiters.Len()
	}
	return 0
}

// slotEntryStats aggregates every model lane of one entry (pooled token or
// bridge) into a single live/queued/oldest triple for the per-account
// snapshot: live and queued sum across models, oldest is the longest-parked
// waiter of any lane.
func (p *Pool) slotEntryStats(entry any) (live, queued int, oldestWait time.Duration) {
	p.routeMu.Lock()
	defer p.routeMu.Unlock()
	for key, st := range p.routeSlots {
		if st == nil || key.entry != entry {
			continue
		}
		live += st.live
		if st.waiters != nil {
			queued += st.waiters.Len()
			if st.waiters.Len() > 0 {
				if head, ok := st.waiters.Front().Value.(*slotWaiter); ok && !head.at.IsZero() {
					if d := time.Since(head.at); d > oldestWait {
						oldestWait = d
					}
				}
			}
		}
	}
	return live, queued, oldestWait
}

// slotQueueRateLimit maps a queue-exhausted signal to the existing 429
// rate-limit shape (spill-exhausted bucket): same code the pool surfaces
// for its per-minute cap, with the local queue hint as Retry-After.
func slotQueueRateLimit(qerr *slotQueueExhaustedError, model string, cap int, live int) *upstream.RateLimitError {
	body := ""
	if qerr != nil {
		body = qerr.Error()
	}
	return &upstream.RateLimitError{
		Status:      "rate_limited",
		Model:       model,
		RetryAfter:  slotQueueHint,
		Limit:       float64(cap),
		RecentCount: float64(live),
		Body:        body,
	}
}

// slotIsQueueExhausted reports whether err is the typed queue-exhausted
// signal (overflow or wait timeout), as opposed to the caller's own ctx
// expiry.
func slotIsQueueExhausted(err error) bool {
	var qerr *slotQueueExhaustedError
	return errors.As(err, &qerr)
}
