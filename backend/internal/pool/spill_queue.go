// spill_queue.go — MASQ spill budget with scale-out on wait expiry.
//
// One Acquire scans the account lanes in roster index order (#1..#N) for
// the requested model. A lane with a free slot and an already-usable
// session grants instantly; otherwise the request parks on the model's
// global FIFO queue (model_queue.go). Only when the queue's QUEUE_WAIT
// budget elapses does the request scale out to the next cold lane. A full
// lane is NOT an upstream refusal, so scaled lanes write no rate-limit
// bucket entry and record no error string — the scale-out is silent. When
// no lane is left, the last signal surfaces once as the existing 429
// rate-limit shape.
//
// MAX_SPILL_ACCOUNTS bounds the walk: the head lane plus that many
// continuation accounts (0 = unbounded, the full index chain). A 429 quota
// requeue (I5) never consumes spill budget: waiting out a quota window on
// the same lane is not a spill hop. The same-lane requeue itself lives in
// acquire_route.go (modelRequeue): after the jail it retakes its own lane's
// free slot when one is still open, else retries the immediate-grant scan
// and otherwise rejoins the model queue at the head.
package pool

import (
	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/upstream"
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
