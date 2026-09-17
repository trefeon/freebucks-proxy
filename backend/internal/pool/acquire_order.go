package pool

import (
	"freebuff-proxy/backend/internal/session"
)

// acquireOrder computes the token iteration order for one Acquire pass:
// plain roster index order over the eligible tokens. toks is the caller's
// snapshot (loaded once in Acquire) — the order is built against the same
// snapshot the failover loop indexes, so an AddToken racing the call can
// never make the loop index past its own snapshot. start is ignored
// (kept for the signature); model filters locked-out and Freebucks-capped
// tokens. Cooling, banned and quarantined tokens stay in the order — the
// failover loop skips them and records their errors there.
func (p *Pool) acquireOrder(toks *[]*tokenEntry, start int, model string) ([]int, []rateLimitEntry) {
	// eligible mirrors the per-token checks the failover loop applies:
	// not cooling down, under the daily message cap, and not
	// Freebucks-capped for the requested model (ADR-0027: session-count
	// caps are gone). It never records the exclusion reasons — the caller
	// does that in one place.
	eligible := func(idx int) bool {
		tok := (*toks)[idx]
		// Administratively locked tokens are never eligible for leasing.
		if tok.locked.Load() {
			return false
		}
		// Model-allowlist routing (MODEL_LOCKS, issue #325): slots locked
		// to other models are skipped for this request (as if unavailable),
		// never demoted or punished. Unlocked slots serve anything.
		if lockedOutByModel(p.cfg.Load(), p.reg, idx, model) {
			tok.allowlistSkips.Add(1)
			return false
		}
		if capped, _ := freebucksCapped(tok, model); capped {
			return false
		}
		return true
	}

	// Plain index order: every eligible token in roster order.
	var order []int
	for idx := range *toks {
		if eligible(idx) {
			order = append(order, idx)
		}
	}

	if len(order) == 0 {
		// All tokens are cooling down or capped: fallback to round-robin
		// so the failover loop visits them and records their errors.
		order = make([]int, len(*toks))
		for i := range order {
			order[i] = (start + i) % len(*toks)
		}
		return order, nil
	}
	// The Freebucks-capped tokens excluded above are never visited by the
	// failover loop, so their rate-limit reasons must ride back with the
	// order: when every token is capped the pool surfaces a real 429 with
	// the earliest window reset instead of a generic combined error.
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
