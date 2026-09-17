// spill_queue.go — MASQ ordered lanes with spill-on-wait-expiry.
//
// One Acquire walks the account lanes in roster index order (#1..#N) for
// the requested model. A lane whose slots are full parks the caller FIFO
// (slot_ledger.go); only when the lane's QUEUE_WAIT budget elapses does
// the request spill to the next lane. A full lane is NOT an upstream
// refusal, so spilled lanes write no rate-limit bucket entry and record
// no error string — the spill is silent. When every lane spills, the last
// lane's signal surfaces once as the existing 429 rate-limit shape.
//
// MAX_SPILL_ACCOUNTS bounds the walk: the head lane plus that many
// continuation accounts (0 = unbounded, the full index chain). A 429 quota
// requeue (I5) never consumes spill budget: waiting out a quota window on
// the same lane is not a spill hop.
package pool

import (
	"context"
	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/upstream"
	"time"
)

// spillChain tracks one Acquire's walk across lanes: how many spill hops
// it took against MAX_SPILL_ACCOUNTS and the last lane-exhausted signal
// for the end-of-chain surface.
type spillChain struct {
	model string
	max   int // MAX_SPILL_ACCOUNTS; <=0 = unbounded
	hops  int // spill hops taken so far
	err   *slotQueueExhaustedError
	cap   int
}

// newSpillChain starts the lane walk for one Acquire of model.
func newSpillChain(cfg *config.Config, model string) *spillChain {
	max := 0
	if cfg != nil {
		max = cfg.MaxSpillAccounts
	}
	return &spillChain{model: model, max: max}
}

// note records one lane spilling (slots full, queue budget elapsed) and
// reports whether the walk may continue to the next lane. Hops beyond
// MAX_SPILL_ACCOUNTS stop the walk: the caller surfaces exhausted().
func (s *spillChain) note(qerr *slotQueueExhaustedError, cap int) bool {
	s.err = qerr
	s.cap = cap
	s.hops++
	return s.max <= 0 || s.hops <= s.max
}

// exhausted surfaces the walk's end: the last lane's signal as the
// existing 429 rate-limit shape, or nil when no lane spilled (other
// buckets own the error).
func (s *spillChain) exhausted() *upstream.RateLimitError {
	if s.err == nil {
		return nil
	}
	return slotQueueRateLimit(s.err, s.model, s.cap, s.err.Live)
}

// quotaRequeueNotBefore decides whether a quota 429 is waited out on the
// same lane: only a short jail is requeued — 0 < RetryAfter <= laneWait,
// the lane's QUEUE_WAIT budget, so a request never parks longer than its
// lane budget for quota. Longer (or windowless) windows degrade to the
// legacy per-lane record. It returns the instant the lane may be retried:
// the upstream RetryAfter passed through unchanged.
func quotaRequeueNotBefore(rle *upstream.RateLimitError, laneWait time.Duration) (time.Time, bool) {
	if rle == nil || rle.RetryAfter <= 0 || rle.RetryAfter > laneWait {
		return time.Time{}, false
	}
	return time.Now().Add(rle.RetryAfter), true
}

// slotRequeue parks the caller for a same-lane quota requeue (I5): it
// sleeps until notBefore (the quota jail expiry), then acquires on the
// same lane normally — tail of the lane queue when contended, immediate
// grant when the lane drained meanwhile. It never touches the spill chain
// (waiting out a quota window is not a spill hop) and writes no cooldown:
// the upstream RetryAfter is the only clock. The caller's ctx bounds the
// whole wait. It reports whether the caller waited at all (jail sleep or
// lane park) for queue-wait telemetry.
func (p *Pool) slotRequeue(ctx context.Context, key slotKey, displayIdx int, cap, depth int, wait time.Duration, notBefore time.Time) (*slotPermit, bool, error) {
	slept := false
	if delay := time.Until(notBefore); delay > 0 {
		slept = true
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, true, ctx.Err()
		case <-timer.C:
		}
	}
	permit, parked, err := p.slotAcquire(ctx, key, displayIdx, cap, depth, wait)
	if err != nil {
		return nil, true, err
	}
	return permit, slept || parked, nil
}
