package runs

import (
	"testing"
	"time"

	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

// TestHardBanNeverSelfHeals pins the hard-ban retirement: a ban without a
// valid resumes_at (the upstream hard-ban shape — an unreversed ban carries
// past_enforcement permanently) must stay live past the old 24h safety
// window, because a timed retry only re-contacts a dead account and
// generates 403 signal traffic. Only the operator unlock (ClearCooldowns /
// dashboard UnlockToken) or a temporary resume lifts it.
func TestHardBanNeverSelfHeals(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newTestManager(t, mock, time.Hour)

	mgr.CooldownBan(&upstream.BanError{Body: "banned"})
	if be := mgr.BanError(); be == nil {
		t.Fatal("BanError() = nil for a hard ban, want remembered ban")
		return
	}
	snap := mgr.Snapshot()
	if snap.BanError == nil || !snap.BannedUntil.IsZero() {
		t.Errorf("hard-ban snapshot = %v/%v, want ban + zero BannedUntil", snap.BanError, snap.BannedUntil)
	}

	// A past resumes_at keeps a short floor memory (banLiftFloor): the
	// quarantine gate still fires once and the first post-lift admission
	// probes before POSTing (prod 20:53-21:05 lift-then-burn loop).
	mgr.ClearCooldowns()
	mgr.CooldownBan(&upstream.BanError{Body: "banned", ResumesAt: time.Now().Add(-time.Hour)})
	if be := mgr.BanError(); be == nil {
		t.Errorf("BanError() = nil for past-resumes ban, want floor memory")
	} else {
		if until := mgr.CooldownUntil(); until.IsZero() || time.Until(until) > banLiftFloor+5*time.Second {
			t.Errorf("CooldownUntil = %v for past-resumes ban, want ~60s floor", until)
		}
	}
	// Floor expiry auto-lifts (simulate by aging the window past).
	mgr.mu.Lock()
	mgr.banUntil = time.Now().Add(-time.Second)
	mgr.cooldownUntil = mgr.banUntil
	mgr.mu.Unlock()
	if be := mgr.BanError(); be != nil {
		t.Errorf("BanError() = %v after floor expiry, want nil (auto-lift)", be)
	}

	// Operator unlock clears it.
	mgr.ClearCooldowns()
	if be := mgr.BanError(); be != nil {
		t.Errorf("BanError() = %v after ClearCooldowns, want nil (unlocked)", be)
	}

	// A temporary ban (valid future resumes_at) still auto-lifts.
	mgr.CooldownBan(&upstream.BanError{Body: "banned", ResumesAt: time.Now().Add(time.Minute)})
	if be := mgr.BanError(); be == nil {
		t.Fatal("BanError() = nil for temporary ban, want remembered")
		return
	}
	mgr.mu.Lock()
	mgr.banUntil = time.Now().Add(-time.Second)
	mgr.mu.Unlock()
	if be := mgr.BanError(); be != nil {
		t.Errorf("BanError() = %v after temporary window expiry, want nil (auto-lift)", be)
	}
}
