// spill_order.go - MASQ strict spill order: plain roster index order with
// eligibility filtering, no re-ranking.
//
// spillOrder returns the account visit order for one Acquire of model:
// ascending roster indexes 0..N with administratively locked, quarantined
// (terminal ban, lift-aware), pin-excluded and Freebucks-capped lanes
// filtered out. A precious holder stays at its own index - it is never
// boosted to the head and never demoted behind free lanes; order is
// strictly positional so drains are reproducible ([0,0,1,1,2,2] on a
// 3-account cap-2 pool) and a dashboard reorder is the only way the visit
// sequence changes.
//
// Cooling tokens stay IN the order: a transient cooldown is not terminal,
// and the failover loop records it in the rate-limit bucket there. Only
// terminal per-model caps (Freebucks) are filtered, with their reasons
// riding back as quotaLimited so a fully-capped pool still surfaces a
// real 429 instead of a generic combined error.
package pool

import (
	"freebucks-proxy/backend/internal/session"
)

// spillOrder computes the strict index order for one Acquire pass over the
// caller's snapshot (loaded once in Acquire): the order is built against
// the same snapshot the spill walk indexes, so an AddToken racing the call
// can never make the walk index past its own snapshot.
func (p *Pool) spillOrder(toks *[]*tokenEntry, model string) ([]int, []rateLimitEntry) {
	cfg := p.cfg.Load()
	reg := p.reg
	eligible := func(idx int) bool {
		tok := (*toks)[idx]
		if tok.locked.Load() {
			return false
		}
		// Lift-aware quarantine: an expired temporary ban rejoins the
		// order instead of staying excluded until an operator unlocks it.
		if q := tok.quarantine.Load(); q != nil && !p.clearLiftedQuarantine(tok) {
			return false
		}
		if pinnedOut(cfg, reg, idx, model) {
			tok.pinSkips.Add(1)
			return false
		}
		if capped, _ := freebucksCapped(tok, model); capped {
			return false
		}
		return true
	}

	var order []int
	for idx := range *toks {
		if eligible(idx) {
			order = append(order, idx)
		}
	}

	if len(order) == 0 {
		// Every lane filtered: fall back to the full index chain so the
		// spill walk still visits each account once and records its
		// refusal in the error buckets (ban/rate-limit/pin) instead of
		// surfacing a context-free generic error.
		order = make([]int, len(*toks))
		for i := range order {
			order[i] = i
		}
		return order, nil
	}
	// Freebucks-capped lanes excluded above are never visited by the walk,
	// so their rate-limit reasons must ride back with the order: when every
	// lane is capped the pool surfaces a real 429 with the earliest window
	// reset instead of a generic combined error.
	inOrder := make(map[int]struct{}, len(order))
	for _, idx := range order {
		inOrder[idx] = struct{}{}
	}
	var quotaLimited []rateLimitEntry
	for idx := range *toks {
		if _, ok := inOrder[idx]; ok {
			continue
		}
		if capped, _ := freebucksCapped((*toks)[idx], model); capped {
			quotaLimited = appendRateLimitEntry(quotaLimited, freebucksLimitError((*toks)[idx], model), idx)
		}
	}
	return order, quotaLimited
}

// bestWaitingRoom picks the queue entry with the lowest position; ties break
// on the lowest queue depth (PRD §3: best-waiting-room-position selection).
func bestWaitingRoom(entries []*session.WaitingRoomError) *session.WaitingRoomError {
	best := entries[0]
	for _, candidate := range entries[1:] {
		if betterWait(candidate, best) {
			best = candidate
		}
	}
	return best
}

// betterWait reports whether a outranks b. Positions <= 0 mean "unknown" and
// rank below any known position (mirrors freebuff2api-quorinex).
func betterWait(a, b *session.WaitingRoomError) bool {
	if b == nil {
		return true
	}
	if a.Position <= 0 {
		return false
	}
	if b.Position <= 0 {
		return true
	}
	if a.Position != b.Position {
		return a.Position < b.Position
	}
	return a.QueueDepth < b.QueueDepth
}
