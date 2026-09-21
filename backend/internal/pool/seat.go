package pool

import "sync/atomic"

// seatCounter counts the chat turns that have begun session admission on one
// account (a pooled token entry or a bridge entry) and are about to dispatch,
// or are already dispatching, upstream work on the account's single session
// seat.
//
// Why it exists: upstream keeps ONE session per account for CLI/web clients —
// the vendored contract documents freebuff_multi_session as a Desktop-only
// opt-in (freebuff-models.ts), so EVERY fresh admission (POST
// /freebuff/session/admission) rewrites the account's active_instance_id. The
// previously admitted instance's next chat completion is then refused with
// 409 session_superseded ("Your session ended before this request started"),
// which the gateway surfaces as 503 + Retry-After (server/errors.go).
//
// Ordering is the whole point: the count is taken BEFORE the session
// admission (admitOnLane / AcquireBridge) and released only when the lease is
// released, so any turn that could lose its instance to a rotation is visible
// to the rotation decision. The runs manager's inflight count cannot serve
// here: it is incremented AFTER the admission returns the instance id, which
// leaves a window in which a rotation races a turn that is already holding
// the old instance.
//
// The counter must never go negative (a negative value would open the gate
// forever and silently restore the bug), so release saturates at zero.
type seatCounter struct {
	n atomic.Int64
}

// acquire marks one turn as begun. Called before the session admission.
func (s *seatCounter) acquire() {
	s.n.Add(1)
}

// release marks one turn as finished (lease release, or a lane that never
// granted a lease). Saturating: a stray extra release cannot corrupt the
// count into opening the gate.
func (s *seatCounter) release() {
	for {
		cur := s.n.Load()
		if cur <= 0 {
			return
		}
		if s.n.CompareAndSwap(cur, cur-1) {
			return
		}
	}
}

// idle reports whether no turn other than the caller's own holds the seat —
// the precondition for a pre-emptive re-admit, which rotates the seat and
// therefore kills anything still in flight on the old instance.
//
// The caller counts itself (it acquired before its own admission), so "1" is
// the caller alone. Callers that do not take a seat (tests, and only tests:
// every production admission path counts itself) must treat a false result as
// "defer", never as an error.
func (s *seatCounter) idle() bool {
	return s.n.Load() <= 1
}

// releaseSeat drops the seat count held by this lease. Nil-safe; paired with
// the acquire inside admitOnLane / AcquireBridge. LeaseRelease and
// LeaseAbandon are the only release paths, and a lease is released by one of
// them (a double call is absorbed by the saturating release).
func (l *Lease) releaseSeat() {
	if l == nil {
		return
	}
	switch {
	case l.entry != nil:
		l.entry.seat.release()
	case l.Bridge != nil:
		l.Bridge.seat.release()
	}
}
