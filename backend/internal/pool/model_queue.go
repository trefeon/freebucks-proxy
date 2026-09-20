// model_queue.go — MASQ smart model queue: one work-conserving FIFO per
// model for pooled entries.
//
// A cold pool used to park every waiter on lane 0's per-lane queue: a burst
// of N requests across M accounts paid lane 0's QUEUE_WAIT even when other
// lanes stood open. The model queue parks waiters GLOBALLY per model
// instead: an arrival grants instantly on the first lane (strict index
// order) with a free slot AND an already-usable session for the model, and
// any pooled lane's Release hands its freed slot to the queue head (FIFO
// across lanes). A waiter that no lane can serve still waits out its single
// QUEUE_WAIT deadline from enqueue, then scales out exactly like today's
// spill (cold lanes in index order within the spill budget; concurrent
// admissions collapse via the session manager's per-entry single-flight).
//
// Bridge entries keep the legacy per-lane park in slot_ledger.go untouched:
// bridge has no spill walk, so its queue-exhausted signal goes straight
// back to the client. slotAcquire/slotQueued stay for that path (and their
// unit tests); pooled lanes never park lane-locally anymore.
//
// Locking: everything below runs under Pool.routeMu in short critical
// sections — cached snapshot/flag reads only, never network I/O. The walk
// (acquire_route.go) never holds routeMu across EnsureSessionForModel or
// run admission.
package pool

import (
	"container/list"
	"context"
	"freebuff-proxy/backend/internal/config"
	"time"
)

// modelWaiter is one waiter parked on a model's global FIFO queue. ch is
// closed exactly once on handoff grant (under Pool.routeMu); granted is set
// in the same critical section so a concurrent timeout/ctx-expiry either
// takes the grant or dequeues, never both and never neither. at is the
// enqueue instant, read under routeMu to report how long the oldest waiter
// has been parked.
type modelWaiter struct {
	ch      chan struct{}
	granted bool
	permit  *slotPermit // handoff permit for the freed lane (set on grant)
	lane    *tokenEntry // entry backing the freed lane (set on grant)
	laneIdx int         // roster index of the freed lane (-1 until granted)
	ctx     context.Context
	element *list.Element
	at      time.Time
}

// modelQueue is one model's global FIFO waiter queue (arrival order =
// grant order). Pooled waiters only; bridge entries never enqueue here.
type modelQueue struct {
	waiters *list.List // of *modelWaiter, front = head
}

// laneCarry is an I5 same-lane quota requeue result: a live-turn permit is
// already held for the lane, so the retry re-runs the walk gates and the
// admission on that lane and skips the slot take. (The legacy per-lane
// requeue discarded its permit and took a second slot on retry, leaking one
// live count per cycle — carrying the permit fixes that.)
type laneCarry struct {
	tok    *tokenEntry
	idx    int
	permit *slotPermit
}

// routeQueueLocked returns the model's queue, creating it. Caller holds
// p.routeMu.
func (p *Pool) routeQueueLocked(model string) *modelQueue {
	if p.routeQueues == nil {
		p.routeQueues = make(map[string]*modelQueue)
	}
	q, ok := p.routeQueues[model]
	if !ok || q == nil {
		q = &modelQueue{waiters: list.New()}
		p.routeQueues[model] = q
	}
	if q.waiters == nil {
		q.waiters = list.New()
	}
	return q
}

// removeModelWaiterLocked dequeues w and drops the queue itself once empty
// so idle models leave no residue. Caller holds p.routeMu.
func (p *Pool) removeModelWaiterLocked(model string, w *modelWaiter) {
	q, ok := p.routeQueues[model]
	if !ok || q == nil || q.waiters == nil {
		return
	}
	if w.element != nil {
		q.waiters.Remove(w.element)
		w.element = nil
	}
	if q.waiters.Len() == 0 {
		delete(p.routeQueues, model)
	}
}

// modelQueueDepth reports the model's parked waiter count (tests and the
// park-detection helpers).
func (p *Pool) modelQueueDepth(model string) int {
	p.routeMu.Lock()
	defer p.routeMu.Unlock()
	if q, ok := p.routeQueues[model]; ok && q != nil && q.waiters != nil {
		return q.waiters.Len()
	}
	return 0
}

// slotTry takes one live-turn slot without parking: fast path only. It
// reports the take and the lane's live count after it. Callers pass a
// positive cap (slotCap==0 bypasses slot gating entirely and never calls
// here).
func (p *Pool) slotTry(key slotKey, cap int) (permit *slotPermit, live int, ok bool) {
	p.routeMu.Lock()
	defer p.routeMu.Unlock()
	st := p.slotStateLocked(key)
	if st.live >= cap {
		return nil, st.live, false
	}
	st.live++
	return &slotPermit{pool: p, key: key}, st.live, true
}

// slotLiveLocked reports the lane's live-turn count. Caller holds p.routeMu.
func (p *Pool) slotLiveLocked(key slotKey) int {
	if st, ok := p.routeSlots[key]; ok && st != nil {
		return st.live
	}
	return 0
}

// sessionUsableForModel reports whether the entry holds an already-usable
// cached session for the model — a pure cached Snapshot read, no network
// I/O. Only such a lane grants instantly on the arrival scan; anything else
// parks and lets the scale-out admit it after QUEUE_WAIT.
func sessionUsableForModel(tok *tokenEntry, model string) bool {
	if tok == nil {
		return false
	}
	ss := tok.session.Snapshot()
	return ss.Usable() && (ss.Model == "" || ss.Model == model)
}

// laneAdmissible is the side-effect-free eligibility predicate: it mirrors
// walkGates' conditions exactly (locked, lift-aware quarantine, pin,
// Freebucks cap, cooldown/ban with the cross-model bypass, remembered
// per-model refusal, cooldown hint) but records nothing — no pinSkips
// increments, no quarantine clears, no bucket writes. Used by the Release
// handoff, the queue stats, and the timeout scope: paths that must observe
// eligibility without perturbing the walk's own accounting. walkGates stays
// authoritative for the walk itself.
//
// A lifted temporary ban falls through here WITHOUT clearing the marker
// (snapshot/handoff paths must not mutate); the walk clears via
// clearLiftedQuarantine when it visits the lane.
func (p *Pool) laneAdmissible(tok *tokenEntry, idx int, model string, cfg *config.Config, now time.Time, skipHinted bool) bool {
	if tok == nil || tok.locked.Load() {
		return false
	}
	if q := tok.quarantine.Load(); q != nil {
		if q.liftAt.IsZero() || now.Before(q.liftAt) {
			return false
		}
	}
	if pinnedOut(cfg, p.reg, idx, model) {
		return false
	}
	if capped, _ := freebucksCapped(tok, model); capped {
		return false
	}
	if until := tok.runs.CooldownUntil(); now.Before(until) || tok.runs.BanError() != nil {
		// Issue #155: a quota cap remembered for another model does not
		// park this one.
		if !canServeOtherModel(tok.runs.RateLimitError(), model) {
			return false
		}
	}
	if tok.runs.ModelRateLimit(model) != nil {
		return false
	}
	if skipHinted && tok.token != "" && p.cooldownHintFresh(poolTokenHash(tok.token), now) {
		return false
	}
	return true
}

// modelHeadLocked elects the model's head lane: the first lane in roster
// index order that passes the eligibility gates, with its live count and
// roster index (-1/0 when no lane is admissible). Caller holds p.routeMu.
// Display only — it scopes timeout/full messages (callers map -1 to the
// unnamed lane) and attributes the queue stats.
func (p *Pool) modelHeadLocked(model string) (idx int, live int) {
	toks := p.roster.Load()
	cfg := p.cfg.Load()
	now := time.Now()
	for i := range *toks {
		tok := (*toks)[i]
		if tok == nil {
			continue
		}
		if !p.laneAdmissible(tok, i, model, cfg, now, false) {
			continue
		}
		return i, p.slotLiveLocked(slotKey{entry: tok, model: model})
	}
	return -1, 0
}

// handoffModelQueueLocked grants the queue head the just-freed pooled slot:
// ctx-dead waiters are skipped (dequeued and dropped); the freed lane's
// eligibility gates are re-checked and a lane whose gates fail is skipped
// for this handoff (the waiter stays queued, the lane stays in the walk for
// future scale-outs). The live count transfers to the head — a third live
// turn never exists. Caller holds p.routeMu; st is the freed lane's state.
func (p *Pool) handoffModelQueueLocked(key slotKey, st *slotState) {
	tok, ok := key.entry.(*tokenEntry)
	if !ok || tok == nil || st == nil {
		return
	}
	q, ok := p.routeQueues[key.model]
	if !ok || q == nil || q.waiters == nil {
		return
	}
	cfg := p.cfg.Load()
	now := time.Now()
	for {
		front := q.waiters.Front()
		if front == nil {
			return
		}
		w := front.Value.(*modelWaiter)
		if w.ctx == nil || w.ctx.Err() != nil {
			q.waiters.Remove(front)
			w.element = nil
			continue
		}
		idx := p.indexOfEntry(tok)
		if idx < 0 || !p.laneAdmissible(tok, idx, key.model, cfg, now, false) {
			return
		}
		st.live++
		w.permit = &slotPermit{pool: p, key: key}
		w.lane = tok
		w.laneIdx = idx
		q.waiters.Remove(front)
		w.element = nil
		if q.waiters.Len() == 0 {
			delete(p.routeQueues, key.model)
		}
		w.granted = true
		close(w.ch)
		return
	}
}

// modelPark enqueues the caller on the model's global FIFO (tail for a fresh
// arrival, head for an I5 same-lane requeue — it already held a slot once)
// and parks until a lane's Release hands the head a slot, the QUEUE_WAIT
// deadline (running from this enqueue) elapses, or ctx expires. A queue at
// depth cap fails over at once with the unchanged "full" signal. On grant it
// returns the handoff permit plus the granting lane.
func (p *Pool) modelPark(ctx context.Context, model string, cap, depth int, wait time.Duration, front bool) (permit *slotPermit, lane *tokenEntry, laneIdx int, parked bool, err error) {
	p.routeMu.Lock()
	q := p.routeQueueLocked(model)
	if depth <= 0 || q.waiters.Len() >= depth {
		headIdx, headLive := p.modelHeadLocked(model)
		token := 0
		if headIdx >= 0 {
			token = headIdx + 1
		}
		p.routeMu.Unlock()
		return nil, nil, -1, false, &slotQueueExhaustedError{Reason: "full", Token: token, Cap: cap, Live: headLive}
	}
	w := &modelWaiter{ch: make(chan struct{}), ctx: ctx, laneIdx: -1, at: time.Now()}
	if front {
		w.element = q.waiters.PushFront(w)
	} else {
		w.element = q.waiters.PushBack(w)
	}
	p.routeMu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-w.ch:
		p.routeMu.Lock()
		permit, lane, laneIdx := w.permit, w.lane, w.laneIdx
		p.routeMu.Unlock()
		return permit, lane, laneIdx, true, nil
	case <-ctx.Done():
		p.routeMu.Lock()
		if w.granted {
			permit, lane, laneIdx := w.permit, w.lane, w.laneIdx
			p.routeMu.Unlock()
			return permit, lane, laneIdx, true, nil
		}
		p.removeModelWaiterLocked(model, w)
		p.routeMu.Unlock()
		return nil, nil, -1, true, ctx.Err()
	case <-timer.C:
		p.routeMu.Lock()
		if w.granted {
			permit, lane, laneIdx := w.permit, w.lane, w.laneIdx
			p.routeMu.Unlock()
			return permit, lane, laneIdx, true, nil
		}
		headIdx, headLive := p.modelHeadLocked(model)
		token := 0
		if headIdx >= 0 {
			token = headIdx + 1
		}
		p.removeModelWaiterLocked(model, w)
		p.routeMu.Unlock()
		return nil, nil, -1, true, &slotQueueExhaustedError{Reason: "timeout", Token: token, Cap: cap, Live: headLive, Wait: wait}
	}
}
