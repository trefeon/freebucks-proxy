package pool

import (
	"freebuff-proxy/backend/internal/testutil"
	"testing"
	"time"
)

// TestSnapshotExpiredClearsLiveFields pins the expired-row contract: a row
// whose expiry and grace both passed (stale "active" cache, polls stopped)
// reports status "expired" with no live-session facts — instance, model,
// countdown, expiry, and queue facts all read like IDLE. Every terminal
// path (Invalidate/commit(nil), poll past-grace, store load) drops the
// slot, so the synthesized terminal state must match instead of parading
// the dead cache values.
//
// Repro: plant a stale active cache aged past grace and read the row.
func TestSnapshotExpiredClearsLiveFields(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	(*p.roster.Load())[0].session.SetSessionStateForTest(
		"active", "inst-expired-repro", modelA,
		time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour),
	)

	got := p.Snapshot()[0]
	if got.SessionStatus != "expired" {
		t.Fatalf("SessionStatus = %q, want expired (setup did not age the session past grace)", got.SessionStatus)
	}
	if got.SessionInstanceID != "" {
		t.Errorf("SessionInstanceID = %q on an expired row, want empty (dead session must read like IDLE)", got.SessionInstanceID)
	}
	if got.SessionModel != "" {
		t.Errorf("SessionModel = %q on an expired row, want empty (stale model)", got.SessionModel)
	}
	if got.SessionRemainingSeconds != 0 {
		t.Errorf("SessionRemainingSeconds = %d on an expired row, want 0 (stale countdown)", got.SessionRemainingSeconds)
	}
	if !got.SessionExpiresAt.IsZero() {
		t.Errorf("SessionExpiresAt = %v on an expired row, want zero (stale expiry)", got.SessionExpiresAt)
	}
	if got.SessionQueuePosition != 0 || got.SessionQueueDepth != 0 {
		t.Errorf("SessionQueuePosition/Depth = %d/%d on an expired row, want 0/0 (stale queue facts)",
			got.SessionQueuePosition, got.SessionQueueDepth)
	}
}

// TestSnapshotGraceKeepsLiveFields guards the other side of the same line:
// a row inside the grace drain still serves in-flight runs, so it keeps its
// live-session facts. Only the terminal "expired" state reads like IDLE.
func TestSnapshotGraceKeepsLiveFields(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	(*p.roster.Load())[0].session.SetSessionStateForTest(
		"active", "inst-grace-live", modelA,
		time.Now().Add(-time.Hour), time.Now().Add(30*time.Minute),
	)

	got := p.Snapshot()[0]
	if got.SessionStatus != "grace" {
		t.Fatalf("SessionStatus = %q, want grace (setup did not land inside the drain window)", got.SessionStatus)
	}
	if got.SessionInstanceID != "inst-grace-live" {
		t.Errorf("SessionInstanceID = %q in grace, want inst-grace-live (drain row stays usable)", got.SessionInstanceID)
	}
	if got.SessionModel != modelA {
		t.Errorf("SessionModel = %q in grace, want %q", got.SessionModel, modelA)
	}
}
