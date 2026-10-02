// billing.go - BillingSession reserve→settle/refund machine (universal-gateway
// Phase 2.4).
//
// Decision record (plan §2.4 either/or): the proxy keeps record-only spend
// (the pool ledger counts usage tokens; upstream $ ceilings stay
// server-enforced) plus DELETE freebucksRefund replay (session EndSession
// receipts) — and this machine tracks one turn's reservation delta ONLY.
// It is never a balance, never persisted, and never consulted for
// admission: no shadow ledgers (invariant §2.8) either way.
//
// Lifecycle: Reserve (pre-consume estimate at turn start) → Settle(actual)
// on success (delta = actual − reserved feeds the record-only ledger) or
// Refund() on failure (releases the estimate). Idempotent guards: settle
// is a no-op after settle, refund is a no-op after settle, and repeats of
// either are safe — the deferred refund on a failed turn can never
// double-count a settled one.
package store

import (
	"sync"
)

// BillingState is one turn reservation's position.
type BillingState int

const (
	// BillingReserved: estimate held, turn in flight.
	BillingReserved BillingState = iota
	// BillingSettled: actual recorded, delta emitted once.
	BillingSettled
	// BillingRefunded: estimate released, nothing recorded.
	BillingRefunded
)

// BillingSession tracks one turn's pre-consume → settle/refund with
// idempotent guards. Zero value is unusable; NewBillingSession first.
type BillingSession struct {
	mu       sync.Mutex
	state    BillingState
	reserved int64
	actual   int64
	settled  bool
}

// NewBillingSession reserves an estimate for one turn. Negative estimates
// clamp to zero (an estimate is a magnitude, never a credit).
func NewBillingSession(preConsumed int64) *BillingSession {
	if preConsumed < 0 {
		preConsumed = 0
	}
	return &BillingSession{state: BillingReserved, reserved: preConsumed}
}

// Settle records the actual turn cost and returns the ledger delta
// (actual − reserved, possibly negative when the estimate overshot).
// A second Settle is a no-op returning 0 with settled=false: the ledger
// must see each turn exactly once.
func (s *BillingSession) Settle(actual int64) (delta int64, settled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settled {
		return 0, false
	}
	if actual < 0 {
		actual = 0
	}
	s.actual = actual
	s.settled = true
	s.state = BillingSettled
	return actual - s.reserved, true
}

// Refund releases the reservation after a failed turn. It reports the
// released estimate with ok=true exactly once; after a Settle it is a
// no-op (ok=false) — a settled turn is already accounted and must never
// be refunded.
func (s *BillingSession) Refund() (released int64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settled || s.state == BillingRefunded {
		return 0, false
	}
	s.state = BillingRefunded
	return s.reserved, true
}

// State reports the session position for logs and tests.
func (s *BillingSession) State() BillingState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Reserved reports the held estimate.
func (s *BillingSession) Reserved() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reserved
}
