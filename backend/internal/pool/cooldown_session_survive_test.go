package pool

import (
	"context"
	"errors"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

// TestWindowCooldownParksWithoutDroppingSession pins the prod 2026-09-16
// contract: a vendor 24h-window refusal (429 rate_limited, pacific_day body
// with windowHours + resetAt and a reset-reaching retryAfterMs) parks the
// token — cooldown memory plus the full retry_after window are preserved —
// while a healthy cached session SURVIVES. The window refusal is a quota
// refusal, not a session-ending gate code (upstream FREEBUFF_GATE_CODES
// endsTheSession:true covers only waiting_room_required, session_expired,
// session_superseded and session_model_mismatch), so no invalidate may fire.
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

	// Half 1 — the token parks with its cooldown memory intact.
	rle := entry.runs.RateLimitError()
	if rle == nil {
		t.Fatal("no remembered rate-limit error after the window refusal (token did not park)")
	} else if want := 71766 * time.Second; rle.RetryAfter != want {
		t.Errorf("remembered retry_after = %v, want %v (window truncated)", rle.RetryAfter, want)
	}
	if until := entry.runs.CooldownUntil(); time.Until(until) < 19*time.Hour {
		t.Errorf("cooldown window = %v, want ~19.9h parked (memory not preserved)", time.Until(until))
	}

	// Half 2 — the healthy session survives the park.
	snap := entry.session.Snapshot()
	if !snap.Usable() {
		t.Fatalf("session dropped by the window refusal (status %q, instance %q)", snap.Status, snap.InstanceID)
	}
	if snap.InstanceID != heldInstance {
		t.Errorf("session instance = %q, want the surviving %q", snap.InstanceID, heldInstance)
	}

	// A window refusal is cooldown-only, never terminal.
	if got := p.Snapshot()[0]; got.Quarantined {
		t.Errorf("Quarantined = true after a window refusal (cooldown only, reason %q)", got.QuarantineReason)
	}
}
