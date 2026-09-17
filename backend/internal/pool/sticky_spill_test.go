package pool

import (
	"context"
	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// longWindow429Body is a 1h admission refusal with a future reset: past the
// 30s lane budget, so the walk records it instead of requeueing.
func longWindow429Body() string {
	reset := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	return `{"status":"rate_limited","limit":3,"recentCount":3,"period":"pacific_day","resetAt":"` + reset + `","retryAfterMs":3600000}`
}

func activeBody(instance, model string) string {
	expires := time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339)
	return `{"status":"active","instanceId":"` + instance + `","model":"` + model + `","expiresAt":"` + expires + `"}`
}

// TestStickySpillLongWindowSticksToHealthyLane is the core regression test:
// a long-window admission 429 on lane #1 is remembered, so the next
// same-model request skips the dead lane with zero upstream contact and
// reuses the healthy lane's live session — instead of burning a fresh
// account per request.
func TestStickySpillLongWindowSticksToHealthyLane(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	var posts0 atomic.Int64
	mock0.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts0.Add(1)
			w.WriteHeader(429)
			_, _ = io.WriteString(w, longWindow429Body())
			return
		}
		w.WriteHeader(200)
		_, _ = io.WriteString(w, activeBody("inst-0", modelA))
	}
	p := newSmartTestPool(t, nil, mock0, mock1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	lease1, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if lease1.Token != 1 {
		t.Fatalf("first lease token = %d, want 1 (failover past the 429 lane)", lease1.Token)
	}
	entry0 := (*p.roster.Load())[0]
	if got := entry0.runs.ModelRateLimit(modelA); got == nil {
		t.Fatal("lane #1 ModelRateLimit(modelA) = nil after long-window 429, want remembered refusal")
	}
	if got := posts0.Load(); got != 1 {
		t.Fatalf("lane #1 session POSTs = %d, want 1", got)
	}
	p.LeaseRelease(lease1)

	lease2, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	defer p.LeaseRelease(lease2)
	if lease2.Token != 1 {
		t.Fatalf("second lease token = %d, want 1 (dead lane skipped, healthy lane reused)", lease2.Token)
	}
	if got := posts0.Load(); got != 1 {
		t.Fatalf("lane #1 session POSTs = %d after two acquires, want 1 (remembered, no upstream contact)", got)
	}
	if got := mock1.SessionCreatesSnapshot(); got != 1 {
		t.Fatalf("lane #2 session creates = %d, want 1 (live session reused, zero admission)", got)
	}
}

// TestStickySpillMemoryIsPerModel pins that the refusal memory never parks
// the whole account: a request for a different model reaches the
// remembered lane again. The admission POST carries x-freebuff-model, so the
// mock can refuse modelA while serving modelB on the same lane.
func TestStickySpillMemoryIsPerModel(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	var posts0 atomic.Int64
	mock0.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts0.Add(1)
			if r.Header.Get("x-freebuff-model") == modelA {
				w.WriteHeader(429)
				_, _ = io.WriteString(w, longWindow429Body())
				return
			}
			w.WriteHeader(200)
			_, _ = io.WriteString(w, activeBody("inst-mock0", r.Header.Get("x-freebuff-model")))
			return
		}
		w.WriteHeader(200)
		_, _ = io.WriteString(w, activeBody("inst-mock0", modelA))
	}
	p := newSmartTestPool(t, nil, mock0, mock1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	lease1, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("acquire modelA: %v", err)
	}
	if lease1.Token != 1 {
		t.Fatalf("modelA lease token = %d, want 1 (failover past the 429 lane)", lease1.Token)
	}
	p.LeaseRelease(lease1)

	leaseB, err := p.Acquire(ctx, modelB)
	if err != nil {
		t.Fatalf("acquire modelB: %v", err)
	}
	defer p.LeaseRelease(leaseB)
	if leaseB.Token != 0 {
		t.Fatalf("modelB lease token = %d, want 0 (per-model memory, lane reached again)", leaseB.Token)
	}
	if leaseB.SessionInstanceID != "inst-mock0" {
		t.Fatalf("modelB instance = %q, want inst-mock0 (fresh admission on lane #1)", leaseB.SessionInstanceID)
	}
	if got := posts0.Load(); got != 2 {
		t.Fatalf("lane #1 session POSTs = %d, want 2 (modelA refusal + modelB admission)", got)
	}
}

// TestStickySpillShortWindowExpires pins the window lifecycle end to end: a
// short refusal is remembered, then re-attempted live once its window
// lapses — the second request still succeeds via failover, never hanging.
func TestStickySpillShortWindowExpires(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	var posts0 atomic.Int64
	mock0.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts0.Add(1)
			// 120ms exceeds the 50ms lane budget below, so the walk
			// records (not requeues) — then the window lapses mid-test.
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"status":"rate_limited","retryAfterMs":120}`)
			return
		}
		w.WriteHeader(200)
		_, _ = io.WriteString(w, activeBody("inst-0", modelA))
	}
	p := newSmartTestPool(t, func(c *config.Config) {
		c.QueueWait = 50 * time.Millisecond
	}, mock0, mock1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	lease1, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if lease1.Token != 1 {
		t.Fatalf("first lease token = %d, want 1", lease1.Token)
	}
	entry0 := (*p.roster.Load())[0]
	if got := entry0.runs.ModelRateLimit(modelA); got == nil {
		t.Fatal("ModelRateLimit = nil right after 120ms refusal, want remembered")
	}
	p.LeaseRelease(lease1)

	time.Sleep(500 * time.Millisecond)
	if got := entry0.runs.ModelRateLimit(modelA); got != nil {
		t.Fatalf("ModelRateLimit = %v after the window lapsed, want nil (live re-attempt)", got)
	}

	lease2, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	defer p.LeaseRelease(lease2)
	if lease2.Token != 1 {
		t.Fatalf("second lease token = %d, want 1 (failover after live re-attempt)", lease2.Token)
	}
	if got := posts0.Load(); got != 2 {
		t.Fatalf("lane #1 session POSTs = %d, want 2 (re-attempted after expiry)", got)
	}
}

// TestStickySpillOpaqueNeverRemembered pins that a 429 without any expiry
// signal is retried live every time: both requests hit the lane.
//
// Shape note: the body is deliberately non-JSON. A JSON admission 429 is
// always floored to a 1m RetryAfter by the session layer (statusError)
// before the pool sees it, so it is never pool-opaque; only a non-JSON
// 429 (edge-infra style) reaches the walk with zero expiry — exactly the
// shape that must never park the lane.
func TestStickySpillOpaqueNeverRemembered(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	var posts0 atomic.Int64
	mock0.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts0.Add(1)
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `edge rate limit, retry later`)
			return
		}
		w.WriteHeader(200)
		_, _ = io.WriteString(w, activeBody("inst-0", modelA))
	}
	p := newSmartTestPool(t, nil, mock0, mock1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	lease1, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	p.LeaseRelease(lease1)
	entry0 := (*p.roster.Load())[0]
	if got := entry0.runs.ModelRateLimit(modelA); got != nil {
		t.Fatalf("ModelRateLimit = %v after opaque 429, want nil (never parked)", got)
	}

	lease2, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	defer p.LeaseRelease(lease2)
	if got := posts0.Load(); got != 2 {
		t.Fatalf("lane #1 session POSTs = %d, want 2 (opaque refusal retried live)", got)
	}
}

// TestStickySpillFreebucksCapBypass pins the reuse bypass: a zero-balance
// lane holding a live reusable session for the requested model is never
// capped, while the same numbers without (or for another model's) session
// stay capped.
func TestStickySpillFreebucksCapBypass(t *testing.T) {
	zeroFb := func() *upstream.FreebucksInfo {
		return &upstream.FreebucksInfo{
			Balance: 0,
			Daily:   upstream.FreebucksWindow{Limit: 20, Spent: 20, Remaining: 0, ResetAt: time.Now().Add(6 * time.Hour)},
			Wallet:  upstream.FreebucksWallet{},
			Prices:  map[string]float64{modelA: 2},
		}
	}
	live := session.SessionSnapshot{
		Status: "active", InstanceID: "inst-1", Model: modelA,
		ExpiresAt: time.Now().Add(30 * time.Minute), Freebucks: zeroFb(),
	}
	if capped, _ := freebucksCappedForSnapshot(live, modelA); capped {
		t.Error("capped with a live reusable session for the model, want bypass (reuse costs zero admission)")
	}
	bare := session.SessionSnapshot{Freebucks: zeroFb()}
	if capped, retry := freebucksCappedForSnapshot(bare, modelA); !capped || retry <= 0 {
		t.Errorf("capped = %v retry = %v without a session, want capped with a future retry", capped, retry)
	}
	other := live
	other.Model = modelB
	if capped, _ := freebucksCappedForSnapshot(other, modelA); !capped {
		t.Error("not capped with a live session bound to another model, want still capped")
	}
	// A monthly-spent lane is bypassed alike while its session lives: the
	// gate predicts a fresh admission's cost, and a live session pays none.
	monthlyFb := zeroFb()
	monthlyFb.Balance = 5
	monthlyFb.Monthly = &upstream.FreebucksMonthlyAllowance{
		LimitUsd: 10, SpentUsd: 10, RemainingUsd: 0, ResetAt: time.Now().Add(24 * time.Hour),
	}
	monthlyLive := live
	monthlyLive.Freebucks = monthlyFb
	if capped, _ := freebucksCappedForSnapshot(monthlyLive, modelA); capped {
		t.Error("capped with a live session under a spent monthly allowance, want bypass")
	}
	monthlyBare := session.SessionSnapshot{Freebucks: monthlyFb}
	if capped, _ := freebucksCappedForSnapshot(monthlyBare, modelA); !capped {
		t.Error("not capped with a spent monthly allowance and no session, want capped")
	}
}
