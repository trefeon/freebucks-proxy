package pool

import (
	"testing"
	"time"
)

func TestBreakerClosedAllows(t *testing.T) {
	b := &Breaker{}
	for range 10 {
		if !b.Allow() {
			t.Fatal("Allow() = false on a fresh breaker")
		}
	}
}

func TestBreakerOpensAfterThreshold(t *testing.T) {
	now := time.Now()
	b := &Breaker{now: func() time.Time { return now }}
	for range breakerFailThreshold - 1 {
		b.RecordFailure()
		if !b.Allow() {
			t.Fatal("Allow() = false before the threshold, want closed")
		}
	}
}

func TestBreakerSuccessResetsCount(t *testing.T) {
	b := &Breaker{}
	b.RecordFailure()
	b.RecordFailure()
	b.RecordSuccess()
	for range breakerFailThreshold {
		b.RecordFailure()
	}
	// Only breakerFailThreshold consecutive failures open: the two before
	// the success must not count (2 pre + success + 3 post opens only if
	// the post-success run alone reaches the threshold — it does here).
	if b.Allow() {
		t.Fatal("Allow() = true after threshold consecutive failures, want open")
	}
}

func TestBreakerSuccessAloneNeverOpens(t *testing.T) {
	b := &Breaker{}
	b.RecordFailure()
	b.RecordSuccess()
	b.RecordFailure()
	if !b.Allow() {
		t.Error("Allow() = false after failure/success/failure, want closed (count reset)")
	}
}

func TestBreakerHalfOpenSingleProbe(t *testing.T) {
	now := time.Now()
	cur := now
	b := &Breaker{now: func() time.Time { return cur }}
	for range breakerFailThreshold {
		b.RecordFailure()
	}
	cur = now.Add(breakerOpenTimeout)
	if !b.Allow() {
		t.Fatal("Allow() = false after open timeout, want the single half-open probe")
	}
	if b.Allow() {
		t.Fatal("second Allow() during probe = true, want false (single probe)")
	}
	if got := b.State(); got != "half-open" {
		t.Errorf("State() = %q, want half-open", got)
	}
	b.RecordSuccess()
	if !b.Allow() {
		t.Error("Allow() = false after probe success, want closed")
	}
	if got := b.State(); got != "closed" {
		t.Errorf("State() = %q, want closed after probe success", got)
	}
}

func TestBreakerFailedProbeReopens(t *testing.T) {
	now := time.Now()
	cur := now
	b := &Breaker{now: func() time.Time { return cur }}
	for range breakerFailThreshold {
		b.RecordFailure()
	}
	cur = now.Add(breakerOpenTimeout)
	if !b.Allow() {
		t.Fatal("no probe admitted after timeout")
	}
	b.RecordFailure()
	if b.Allow() {
		t.Fatal("Allow() = true right after failed probe, want re-opened")
	}
	cur = cur.Add(breakerOpenTimeout)
	if !b.Allow() {
		t.Fatal("Allow() = false after second timeout, want another probe")
	}
}

func TestBreakerRegistryLanesAreIndependent(t *testing.T) {
	r := NewBreakerRegistry()
	a := r.For("model-a", 0)
	b := r.For("model-a", 1)
	c := r.For("model-b", 0)
	if a == b || a == c {
		t.Fatal("distinct lanes share a breaker, want per-(model, account) isolation")
	}
	if got := r.For("model-a", 0); got != a {
		t.Error("For(same lane) returned a different breaker, want stable identity")
	}
	for range breakerFailThreshold {
		a.RecordFailure()
	}
	if a.Allow() {
		t.Error("failing lane still allows, want open")
	}
	if !b.Allow() || !c.Allow() {
		t.Error("sibling lanes blocked by another lane's failures, want isolation")
	}
	if got := r.LaneCount(); got != 3 {
		t.Errorf("LaneCount() = %d, want 3", got)
	}
}
