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
	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/upstream"
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
