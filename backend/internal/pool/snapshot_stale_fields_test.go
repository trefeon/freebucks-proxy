package pool

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/testutil"
)

// TestSnapshotClearsStaleFieldsWithoutInstance pins the pool snapshot half:
// a token row with no live instance must not carry a stale Model or
// Remaining. The session layer stashes the last-seen countdown across
// invalidation by design (quota/glmPromo/remainingMs survive commit(nil) for
// the restart-resume path), so the pool must zero the live-session fields
// when there is no instance to attach them to.
//
// Repro: admit a session whose body carries remainingMs, drop the cache
// (Invalidate, no DELETE), and read the row. Model is already "" on a
// nil-state snapshot (session half does NOT reproduce); Remaining renders
// the stashed countdown (pool half reproduces).
func TestSnapshotClearsStaleFieldsWithoutInstance(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// Serve an active admission carrying the wire countdown, like the real
	// upstream does (the stock mock body omits remainingMs).
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":      "active",
			"instanceId":  "inst-stale-repro",
			"expiresAt":   time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339),
			"remainingMs": 3600000,
		})
	}
	p := newTestPool(t, mock)
	ctx := context.Background()

	lease, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("healthy acquire: %v", err)
	}
	p.LeaseRelease(lease)

	// Drop the cached session without an upstream DELETE: the row now has
	// no live instance, but the stashed countdown survives in the manager.
	(*p.roster.Load())[0].session.Invalidate()

	got := p.Snapshot()[0]
	if got.SessionInstanceID != "" {
		t.Fatalf("SessionInstanceID = %q, want empty (setup did not drop the session)", got.SessionInstanceID)
	}
	if got.SessionModel != "" {
		t.Errorf("SessionModel = %q with no live instance, want empty (stale model)", got.SessionModel)
	}
	if got.SessionRemainingSeconds != 0 {
		t.Errorf("SessionRemainingSeconds = %d with no live instance, want 0 (stale countdown)", got.SessionRemainingSeconds)
	}
	if !got.SessionExpiresAt.IsZero() {
		t.Errorf("SessionExpiresAt = %v with no live instance, want zero (stale expiry)", got.SessionExpiresAt)
	}
}
