package runs

import (
	"testing"
	"time"

	"freebucks-proxy/backend/internal/testutil"
	"freebucks-proxy/backend/internal/upstream"
)

// The cooldown memory must remember WHY a token is cooling down when upstream
// refused it for the freebucks window: the kind, the window length upstream
// declared, and upstream's own reset instant — so the dashboard can say
// "freebucks window, resets 2026-09-17T07:00:00Z" instead of an anonymous
// cooldown_until. The RetryAfter-preferred cooldown ordering must NOT change:
// the proxy window still ends at now+RetryAfter.

// TestCooldownRateLimitRemembersFreebucksWindow pins the remembered window
// evidence and the untouched cooldown deadline: with a 30m RetryAfter and a
// 20h-out reset, the proxy window ends in 30m while the upstream reset stays
// a separate, honestly reported fact.
func TestCooldownRateLimitRemembersFreebucksWindow(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	m, _ := newTestManager(t, mock, time.Hour)

	reset := time.Now().Add(20 * time.Hour).UTC().Truncate(time.Second)
	m.CooldownRateLimit(&upstream.RateLimitError{
		Status:             "rate_limited",
		RetryAfter:         30 * time.Minute,
		ResetAt:            reset,
		WindowHours:        24,
		FreebucksShortfall: true,
	})

	snap := m.Snapshot()
	if snap.RateLimitKind != upstream.WindowKindFreebucks {
		t.Errorf("RateLimitKind = %q, want %q", snap.RateLimitKind, upstream.WindowKindFreebucks)
	}
	if snap.RateLimitWindowHours != 24 {
		t.Errorf("RateLimitWindowHours = %d, want 24", snap.RateLimitWindowHours)
	}
	if !snap.RateLimitResetsAt.Equal(reset) {
		t.Errorf("RateLimitResetsAt = %v, want %v (upstream reset remembered)", snap.RateLimitResetsAt, reset)
	}
	if until := snap.CooldownUntil; until.Before(time.Now().Add(29*time.Minute)) || until.After(time.Now().Add(31*time.Minute)) {
		t.Errorf("CooldownUntil = %v, want now+30m (RetryAfter still preferred)", until)
	}
}

// TestCooldownRateLimitPlainRefusalLeavesKindEmpty pins that a plain
// retry-after refusal carries no kind and no window evidence: the payload
// keys stay absent for every cooldown that is not the freebucks window.
func TestCooldownRateLimitPlainRefusalLeavesKindEmpty(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	m, _ := newTestManager(t, mock, time.Hour)

	m.CooldownRateLimit(&upstream.RateLimitError{Status: "rate_limited", RetryAfter: 10 * time.Minute})

	snap := m.Snapshot()
	if snap.RateLimitKind != "" {
		t.Errorf("RateLimitKind = %q, want empty for a plain rate limit", snap.RateLimitKind)
	}
	if snap.RateLimitWindowHours != 0 {
		t.Errorf("RateLimitWindowHours = %d, want 0", snap.RateLimitWindowHours)
	}
	if !snap.RateLimitResetsAt.IsZero() {
		t.Errorf("RateLimitResetsAt = %v, want zero (no reset in the body)", snap.RateLimitResetsAt)
	}
}

// TestCooldownRateLimitWindowEvidenceClearsAfterLift pins the live-window
// gate: once the cooldown has passed, the remembered kind is no longer
// reported (mirroring RateLimitError()), so a stale refusal never keeps
// rendering as the current reason.
func TestCooldownRateLimitWindowEvidenceClearsAfterLift(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	m, _ := newTestManager(t, mock, time.Hour)

	m.CooldownRateLimit(&upstream.RateLimitError{
		Status:             "rate_limited",
		RetryAfter:         10 * time.Second,
		WindowHours:        24,
		FreebucksShortfall: true,
	})
	m.mu.Lock()
	m.cooldownUntil = time.Now().Add(-time.Second)
	m.mu.Unlock()

	if got := m.Snapshot().RateLimitKind; got != "" {
		t.Errorf("RateLimitKind = %q after the window lifted, want empty", got)
	}
}
