package session

// G5 same-claim admission retry tests: a 503/capacity/transient admission
// POST retries the SAME claim with backoff (20s doubling, 5m cap) before
// surfacing to the pool's failover — never failing over across tokens on a
// waiting-room without exhausting these first
// (freebuff-session-api.ts:68, polling-backoff.ts:15-44).

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

// zeroAdmissionBackoff runs the G5 retry loop with no real 20s stalls.
func zeroAdmissionBackoff(t *testing.T) {
	t.Helper()
	old := admissionRetryBackoff
	admissionRetryBackoff = func(int, time.Duration) time.Duration { return 0 }
	t.Cleanup(func() { admissionRetryBackoff = old })
}

// TestAdmissionRetries503SameClaim pins the waiting-room retry: two 503
// admission refusals then an active response admit successfully with exactly
// three POSTs, all rejoining on the same claim.
func TestAdmissionRetries503SameClaim(t *testing.T) {
	zeroAdmissionBackoff(t)
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	var posts atomic.Int32
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected %s (want only admission POSTs)", r.Method)
			http.NotFound(w, r)
			return
		}
		if n := posts.Add(1); n <= 2 {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "busy"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "active",
			"instanceId": "inst-retry",
			"expiresAt":  "2030-01-01T00:00:00Z",
		})
	}

	before := mgr.ensureClaim()
	inst, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatalf("admission after two 503s failed: %v (want same-claim retry to recover)", err)
	}
	if inst != "inst-retry" {
		t.Errorf("instance = %q, want inst-retry", inst)
	}
	if got := posts.Load(); got != 3 {
		t.Errorf("admission POSTs = %d, want 3 (initial + 2 same-claim retries)", got)
	}
	if got := mgr.ClaimIDForTest(); got != before {
		t.Errorf("claim after retried admission = %q, want stable %q", got, before)
	}
}

// TestAdmissionRetryExhaustsThenSurfaces pins the bound: a permanently
// saturated admission surfaces the waiting-room refusal only AFTER the
// same-claim budget (initial + admissionMaxRetries) is spent.
func TestAdmissionRetryExhaustsThenSurfaces(t *testing.T) {
	zeroAdmissionBackoff(t)
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	var posts atomic.Int32
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "busy"})
	}

	_, err := mgr.EnsureSession(context.Background())
	var wr *upstream.WaitingRoomError
	if !errors.As(err, &wr) {
		t.Fatalf("err = %v, want upstream WaitingRoomError after exhausted retries", err)
	}
	if got := posts.Load(); got != 1+admissionMaxRetries {
		t.Errorf("admission POSTs = %d, want %d (budget exhausted before failover)", got, 1+admissionMaxRetries)
	}
}

// TestAdmissionNoRetryOnRateLimited pins that a typed quota refusal is NOT
// retried: it carries a token cooldown the pool owns, so exactly one POST
// fires and the RateLimitError surfaces immediately.
func TestAdmissionNoRetryOnRateLimited(t *testing.T) {
	zeroAdmissionBackoff(t)
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	var posts atomic.Int32
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		posts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"model":"deepseek/deepseek-v4-flash","limit":3,"period":"pacific_day","resetTimeZone":"America/Los_Angeles","resetAt":"2026-08-12T07:00:00.000Z","recentCount":3.6,"status":"rate_limited","retryAfterMs":60000}`))
	}

	_, err := mgr.EnsureSession(context.Background())
	var rle *upstream.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("err = %v, want RateLimitError (no retry)", err)
	}
	if got := posts.Load(); got != 1 {
		t.Errorf("admission POSTs = %d, want 1 (quota refusals never retry)", got)
	}
}

// TestAdmissionNoRetryOnWaitingRoomRequired pins that a 428 chain demand is
// NOT retried: the pool must fire the ad-chain before any re-POST, so the
// refusal surfaces after exactly one POST.
func TestAdmissionNoRetryOnWaitingRoomRequired(t *testing.T) {
	zeroAdmissionBackoff(t)
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	var posts atomic.Int32
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		posts.Add(1)
		writeJSON(w, 428, map[string]any{"error": "waiting_room_required", "message": "walk the ads chain"})
	}

	_, err := mgr.EnsureSession(context.Background())
	if !errors.Is(err, upstream.ErrWaitingRoomRequired) {
		t.Fatalf("err = %v, want ErrWaitingRoomRequired (no retry)", err)
	}
	if got := posts.Load(); got != 1 {
		t.Errorf("admission POSTs = %d, want 1 (428 needs the chain first)", got)
	}
}

// TestAdmissionNoRetryOnTransport pins the unknown-disposition rule (vendor
// tip 57943aa71): a POST that never got a response may already have
// committed upstream, so it must NOT be replayed — exactly one POST fires.
func TestAdmissionNoRetryOnTransport(t *testing.T) {
	zeroAdmissionBackoff(t)
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	var posts atomic.Int32
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		posts.Add(1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("mock server does not support hijacking")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack failed: %v", err)
			return
		}
		_ = conn.Close()
	}

	_, err := mgr.EnsureSession(context.Background())
	if err == nil {
		t.Fatal("want transport error, got nil")
	}
	var wr *upstream.WaitingRoomError
	var cde *upstream.CapacityDeferredError
	if errors.As(err, &wr) || errors.As(err, &cde) {
		t.Fatalf("err = %v, want raw transport failure (never classified as retryable queue)", err)
	}
	if got := posts.Load(); got != 1 {
		t.Errorf("admission POSTs = %d, want 1 (unknown disposition never replays)", got)
	}
}

// TestAdmissionRetryHonorsCancel pins that a cancelled context aborts the
// backoff instead of firing more POSTs.
func TestAdmissionRetryHonorsCancel(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	var posts atomic.Int32
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "busy"})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := mgr.EnsureSession(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := posts.Load(); got != 0 {
		t.Errorf("admission POSTs = %d, want 0 (cancelled before the first POST)", got)
	}
}
