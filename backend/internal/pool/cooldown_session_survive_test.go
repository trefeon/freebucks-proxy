package pool

import (
	"context"
	"errors"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
	"testing"
)

// TestWindowCooldownParksWithoutDroppingSession pins the MASQ prod contract
// for a vendor 24h-window refusal (429 rate_limited, pacific_day body with
// windowHours + resetAt and a reset-reaching retryAfterMs): the refusal
// surfaces as ErrRateLimited with NO cooldown memory — MASQ writes no
// per-token 429 park (the upstream RetryAfter is only carried for surfacing,
// and a short jail is waited out same-lane), so the lane stays eligible —
// while a healthy cached session SURVIVES (precious: never proactively
// dropped). (Name is historical: pre-MASQ the token parked; under MASQ
// nothing parks.) The window refusal is a quota refusal, not a
// session-ending gate code (upstream FREEBUFF_GATE_CODES endsTheSession:true
// covers only waiting_room_required, session_expired, session_superseded and
// session_model_mismatch), so no invalidate may fire.
func TestWindowCooldownParksWithoutDroppingSession(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	ctx := context.Background()

	// Healthy admission: lease, then release. The session stays cached.
	lease, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("healthy acquire: %v", err)
	}
	heldInstance := lease.SessionInstanceID
	if heldInstance == "" {
		t.Fatal("healthy acquire yielded an empty session instance id")
	}
	agentID := lease.AgentID
	p.LeaseRelease(lease)

	entry := (*p.roster.Load())[0]
	if snap := entry.session.Snapshot(); !snap.Usable() {
		t.Fatalf("session not usable after healthy acquire+release (status %q)", snap.Status)
	}

	// Drop the run only (run-level invalidate, never the session), then
	// trip the 24h-window refusal on the next run START: retry_after=71766s
	// (~19.9h, prod 2026-09-16) with the pacific_day reset body.
	p.InvalidateLeaseRun(lease, agentID)
	mock.RateLimit = true
	mock.RateLimitRetryAfterMs = 71766000

	_, err = p.Acquire(ctx, modelA)
	if !errors.Is(err, upstream.ErrRateLimited) {
		t.Fatalf("window refusal: want ErrRateLimited, got %v", err)
	}

	// Half 1 — MASQ keeps no refusal memory: no remembered error, no parked
	// window. The caller saw the upstream 429 above and retries into a
	// fully eligible lane.
	if rle := entry.runs.RateLimitError(); rle != nil {
		t.Errorf("remembered rate-limit error %v after the window refusal (MASQ keeps no 429 memory)", rle)
	}
	if until := entry.runs.CooldownUntil(); !until.IsZero() {
		t.Errorf("cooldown until = %v, want zero (no 429 park under MASQ)", until)
	}

	// Half 2 — the healthy session survives the refusal.
	snap := entry.session.Snapshot()
	if !snap.Usable() {
		t.Fatalf("session dropped by the window refusal (status %q, instance %q)", snap.Status, snap.InstanceID)
	}
	if snap.InstanceID != heldInstance {
		t.Errorf("session instance = %q, want the surviving %q", snap.InstanceID, heldInstance)
	}

	// A window refusal is never terminal.
	if got := p.Snapshot()[0]; got.Quarantined {
		t.Errorf("Quarantined = true after a window refusal (refusal only, reason %q)", got.QuarantineReason)
	}
}
