package store

import (
	"sync"
	"testing"
)

func TestBillingSettleEmitsDeltaOnce(t *testing.T) {
	s := NewBillingSession(100)
	delta, ok := s.Settle(150)
	if !ok || delta != 50 {
		t.Errorf("Settle(150) = (%d, %v), want (50, true)", delta, ok)
	}
	if got := s.State(); got != BillingSettled {
		t.Errorf("State() = %v, want settled", got)
	}
	if delta, ok := s.Settle(999); ok || delta != 0 {
		t.Errorf("second Settle = (%d, %v), want (0, false) no-op", delta, ok)
	}
}

func TestBillingSettleNegativeDelta(t *testing.T) {
	s := NewBillingSession(100)
	delta, ok := s.Settle(30)
	if !ok || delta != -70 {
		t.Errorf("Settle(30) = (%d, %v), want (-70, true) overshoot", delta, ok)
	}
}

func TestBillingRefundBeforeSettle(t *testing.T) {
	s := NewBillingSession(100)
	released, ok := s.Refund()
	if !ok || released != 100 {
		t.Errorf("Refund() = (%d, %v), want (100, true)", released, ok)
	}
	if got := s.State(); got != BillingRefunded {
		t.Errorf("State() = %v, want refunded", got)
	}
	if released, ok := s.Refund(); ok || released != 0 {
		t.Errorf("second Refund = (%d, %v), want (0, false) idempotent", released, ok)
	}
	// A settle after a refund still records: the turn happened, only the
	// estimate was released. The ledger sees it exactly once via settled.
	if _, ok := s.Settle(40); !ok {
		t.Error("Settle after Refund failed, want the actual still recorded once")
	}
}

func TestBillingRefundAfterSettleNoOp(t *testing.T) {
	s := NewBillingSession(100)
	if _, ok := s.Settle(120); !ok {
		t.Fatal("Settle failed")
	}
	if released, ok := s.Refund(); ok || released != 0 {
		t.Errorf("Refund after Settle = (%d, %v), want (0, false) — settled turns never refund", released, ok)
	}
	if got := s.State(); got != BillingSettled {
		t.Errorf("State() = %v, want settled (refund must not move it)", got)
	}
}

func TestBillingNegativeEstimateClamps(t *testing.T) {
	s := NewBillingSession(-50)
	if got := s.Reserved(); got != 0 {
		t.Errorf("Reserved() = %d, want 0 (clamped)", got)
	}
}

func TestBillingConcurrentSettleOnce(t *testing.T) {
	s := NewBillingSession(100)
	var wg sync.WaitGroup
	wins := 0
	var mu sync.Mutex
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := s.Settle(100); ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("concurrent Settle wins = %d, want exactly 1 (idempotent guard)", wins)
	}
}
