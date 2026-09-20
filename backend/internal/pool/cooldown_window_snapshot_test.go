package pool

import (
	"testing"
	"time"

	"freebucks-proxy/backend/internal/testutil"
	"freebucks-proxy/backend/internal/upstream"
)

// The pool snapshot is the surface the dashboard and /metrics read: a
// freebucks-window cooldown must carry WHY (kind + declared window length)
// and WHEN it lifts (upstream's reset instant) so an operator never has to
// open a log to explain a ~20h cooldown. A plain cooldown must keep the old
// shape.

// TestSnapshotExposesFreebucksWindowCooldown pins the three additive snapshot
// fields on the freebucks-window cooldown, next to the unchanged proxy
// deadline.
func TestSnapshotExposesFreebucksWindowCooldown(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	reset := time.Now().Add(20 * time.Hour).UTC().Truncate(time.Second)
	p.CooldownTokenRateLimit(0, &upstream.RateLimitError{
		Status:             "rate_limited",
		RetryAfter:         30 * time.Minute,
		ResetAt:            reset,
		WindowHours:        24,
		FreebucksShortfall: true,
	})

	snap := p.Snapshot()[0]
	if snap.CooldownKind != upstream.WindowKindFreebucks {
		t.Errorf("CooldownKind = %q, want %q", snap.CooldownKind, upstream.WindowKindFreebucks)
	}
	if snap.CooldownWindowHours != 24 {
		t.Errorf("CooldownWindowHours = %d, want 24", snap.CooldownWindowHours)
	}
	if !snap.CooldownResetsAt.Equal(reset) {
		t.Errorf("CooldownResetsAt = %v, want %v", snap.CooldownResetsAt, reset)
	}
	if !snap.CooldownUntil.After(time.Now()) {
		t.Errorf("CooldownUntil = %v, want a live proxy window", snap.CooldownUntil)
	}
}

// TestSnapshotPlainCooldownHasNoWindowReason pins backward compatibility: a
// plain retry-after cooldown carries no kind, no window length and no reset,
// so the payload keys stay absent.
func TestSnapshotPlainCooldownHasNoWindowReason(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	p.CooldownTokenRateLimit(0, &upstream.RateLimitError{Status: "rate_limited", RetryAfter: 30 * time.Minute})

	snap := p.Snapshot()[0]
	if snap.CooldownKind != "" {
		t.Errorf("CooldownKind = %q, want empty for a plain rate limit", snap.CooldownKind)
	}
	if snap.CooldownWindowHours != 0 {
		t.Errorf("CooldownWindowHours = %d, want 0", snap.CooldownWindowHours)
	}
	if !snap.CooldownResetsAt.IsZero() {
		t.Errorf("CooldownResetsAt = %v, want zero", snap.CooldownResetsAt)
	}
}
