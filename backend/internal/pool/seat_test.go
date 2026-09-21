package pool

// Seat accounting (seat.go) tests. The seat count is the gate the session's
// pre-emptive re-admit consults, so it must cover a turn from BEFORE its
// session admission until its lease is released, saturate at zero, and return
// to idle on every release path.

import (
	"context"
	"fmt"
	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/testutil"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// TestSeatCounterIdleAndSaturation pins the counter's two safety properties:
// the caller's own turn reads idle (so a lone request may rotate), a second
// turn does not, and a stray extra release can never drive the count negative
// — a negative count would read idle forever and silently reopen the very
// rotation race the gate exists to close.
func TestSeatCounterIdleAndSaturation(t *testing.T) {
	var s seatCounter
	if !s.idle() {
		t.Fatal("fresh counter must read idle")
	}
	s.acquire()
	if !s.idle() {
		t.Fatal("the caller's own turn must read idle")
	}
	s.acquire()
	if s.idle() {
		t.Fatal("two turns must not read idle")
	}
	s.release()
	s.release()
	s.release() // stray extra release must saturate, not underflow
	if got := s.n.Load(); got != 0 {
		t.Fatalf("count after over-release = %d, want 0", got)
	}
	if !s.idle() {
		t.Fatal("counter must read idle once every turn released")
	}
}

// TestPreemptiveReAdmitDefersBehindHeldLease is the regression test for the
// live 2026-09-21 pair of 503 session_superseded responses (token 7,
// deepseek/deepseek-v4-flash): with a lease in flight, a second turn's
// admission must NOT rotate the account's single upstream seat. Once the
// leases drain, the next turn rotates cleanly and is served by the new
// instance.
func TestPreemptiveReAdmitDefersBehindHeldLease(t *testing.T) {
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	var creates atomic.Int32
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			_, _ = fmt.Fprintf(w, `{"status":"active","instanceId":"inst-1","expiresAt":%q}`,
				time.Now().Add(30*time.Minute).UTC().Format(time.RFC3339))
			return
		}
		id := fmt.Sprintf("inst-%d", creates.Add(1))
		// 20s of life against the 60s lead below: every admission in this
		// test is inside the pre-expiry window.
		_, _ = fmt.Fprintf(w, `{"status":"active","instanceId":%q,"expiresAt":%q}`,
			id, time.Now().Add(20*time.Second).UTC().Format(time.RFC3339))
	}
	p := newSmartTestPool(t, func(c *config.Config) {
		c.SlotsPerAccount = 2
		c.SessionReAdmitLead = time.Minute
		c.QueueWait = 2 * time.Second
		c.QueueDepth = 16
	}, mock)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	holder, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if holder.SessionInstanceID != "inst-1" {
		t.Fatalf("holder instance = %q, want inst-1", holder.SessionInstanceID)
	}

	second, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if got := creates.Load(); got != 1 {
		t.Fatalf("session creates with a lease in flight = %d, want 1 (the re-admit must defer, not rotate)", got)
	}
	if second.SessionInstanceID != holder.SessionInstanceID {
		t.Fatalf("second lease instance = %q, want the held %q", second.SessionInstanceID, holder.SessionInstanceID)
	}

	// Drain the seat: the next turn finds it idle and rotates.
	p.LeaseRelease(holder)
	p.LeaseRelease(second)
	third, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("third acquire: %v", err)
	}
	if third.SessionInstanceID == holder.SessionInstanceID {
		t.Fatalf("third lease instance = %q, want a fresh admission once the seat drained", third.SessionInstanceID)
	}
	if got := creates.Load(); got != 2 {
		t.Fatalf("session creates after the seat drained = %d, want 2", got)
	}
	p.LeaseRelease(third)
}
