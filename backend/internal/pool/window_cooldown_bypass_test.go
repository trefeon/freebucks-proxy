package pool

import (
	"context"
	"errors"
	"testing"
	"time"

	"freebucks-proxy/backend/internal/testutil"
	"freebucks-proxy/backend/internal/upstream"
)

// TestWindowCooldownParksCrossModelWithoutEndingSession pins the in-window
// session-killer from the #583 verdict: a token holding a live modelA session
// that sits in the vendor 24h freebucks-window cooldown must PARK a modelB
// request — the account-wide ceiling cannot be bypassed per model — and the
// refresh path must NOT reach EndSession for the surviving instance.
//
// Pre-fix the per-model bypass (issue #155/#178) admits the cross-model
// attempt: refresh's release-then-create switch DELETEs the modelA slot via
// releaseHeldSlotForTarget and the doomed modelB create burns a create, so
// the surviving session is gone.
func TestWindowCooldownParksCrossModelWithoutEndingSession(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	ctx := context.Background()

	// Healthy admission on modelA: lease, then release. The session stays cached.
	lease, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("healthy acquire: %v", err)
	}
	heldInstance := lease.SessionInstanceID
	if heldInstance == "" {
		t.Fatal("healthy acquire yielded an empty session instance id")
	}
	p.LeaseRelease(lease)
	createsAfterAdmit := mock.SessionCreatesSnapshot()

	entry := (*p.roster.Load())[0]

	// Trip the vendor 24h freebucks-window refusal for modelA (prod
	// 2026-09-16: retry_after=71766s with windowHours 24 + resetAt and the
	// freebucksShortfall marker that makes the kind).
	reset := time.Now().Add(20 * time.Hour).UTC().Truncate(time.Second)
	p.CooldownTokenRateLimit(0, &upstream.RateLimitError{
		Status:             "rate_limited",
		Model:              modelA,
		RetryAfter:         71766 * time.Second,
		ResetAt:            reset,
		Period:             "pacific_day",
		WindowHours:        24,
		FreebucksShortfall: true,
	})

	endsBefore := mock.SessionEndsSnapshot()

	// A different model must NOT bypass an account-wide window cooldown.
	_, err = p.Acquire(ctx, modelB)
	if err == nil {
		t.Fatal("cross-model acquire during a freebucks-window cooldown succeeded (bypass admitted a doomed switch)")
	}
	if !errors.Is(err, upstream.ErrRateLimited) {
		t.Fatalf("parked acquire: want ErrRateLimited, got %v", err)
	}

	// The surviving session is untouched: no EndSession, no replacement create.
	if got := mock.SessionEndsSnapshot(); got != endsBefore {
		t.Errorf("SessionEnds = %d, want %d (refresh reached EndSession on the surviving instance)", got, endsBefore)
	}
	if got := mock.SessionCreatesSnapshot(); got != createsAfterAdmit {
		t.Errorf("SessionCreates = %d, want %d (no doomed re-admission while parked)", got, createsAfterAdmit)
	}
	snap := entry.session.Snapshot()
	if !snap.Usable() {
		t.Fatalf("session dropped by the parked cross-model attempt (status %q, instance %q)", snap.Status, snap.InstanceID)
	}
	if snap.InstanceID != heldInstance {
		t.Errorf("session instance = %q, want the surviving %q", snap.InstanceID, heldInstance)
	}

	// The window cooldown memory itself is intact (kind + full window).
	rle := entry.runs.RateLimitError()
	if rle == nil {
		t.Fatal("no remembered rate-limit error after the parked attempt (token did not stay parked)")
	} else {
		if got := rle.WindowKind(); got != upstream.WindowKindFreebucks {
			t.Errorf("remembered kind = %q, want %q", got, upstream.WindowKindFreebucks)
		}
		if want := 71766 * time.Second; rle.RetryAfter != want {
			t.Errorf("remembered retry_after = %v, want %v (window truncated)", rle.RetryAfter, want)
		}
	}
	if until := entry.runs.CooldownUntil(); time.Until(until) < 19*time.Hour {
		t.Errorf("cooldown window = %v, want ~19.9h parked (memory not preserved)", time.Until(until))
	}
}

// TestPlainQuotaCooldownStillServesOtherModel pins the protected prior
// behavior (issues #155/#178): a plain per-model quota refusal — same shape
// as the window one but WITHOUT the freebucksShortfall marker — still lets
// the token serve its other models. The kind-aware fix must change the window
// path only; this control stays green before and after.
func TestPlainQuotaCooldownStillServesOtherModel(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	ctx := context.Background()

	lease, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("healthy acquire: %v", err)
	}
	p.LeaseRelease(lease)

	// Plain per-model quota cap on modelA: pacific_day period + counter
	// at/over the limit, no window marker (WindowKind() == "").
	(*p.roster.Load())[0].runs.CooldownRateLimit(&upstream.RateLimitError{
		Status:      "rate_limited",
		Model:       modelA,
		RetryAfter:  30 * time.Minute,
		Period:      "pacific_day",
		Limit:       4,
		RecentCount: 4,
	})

	leaseB, err := p.Acquire(ctx, modelB)
	if err != nil {
		t.Fatalf("cross-model acquire during a plain per-model quota cooldown: %v (bypass regressed)", err)
	}
	if leaseB.SessionInstanceID == "" {
		t.Error("cross-model lease has an empty session instance id")
	}
	p.LeaseRelease(leaseB)
}

// TestCanServeOtherModelKindMatrix pins the bypass predicate edges: only a
// quota-shaped refusal for a DIFFERENT model, without the account-wide
// window marker, keeps the token available for the requested model.
func TestCanServeOtherModelKindMatrix(t *testing.T) {
	quotaFor := func(model string) *upstream.RateLimitError {
		return &upstream.RateLimitError{
			Status:      "rate_limited",
			Model:       model,
			RetryAfter:  30 * time.Minute,
			Period:      "pacific_day",
			Limit:       4,
			RecentCount: 4,
		}
	}
	cases := []struct {
		name  string
		rle   *upstream.RateLimitError
		model string
		want  bool
	}{
		{"nil refusal serves nothing", nil, modelB, false},
		{"plain quota cap on another model bypasses", quotaFor(modelA), modelB, true},
		{"plain quota cap on the same model parks", quotaFor(modelA), modelA, false},
		{"untagged refusal parks (no model to isolate)", &upstream.RateLimitError{Status: "rate_limited", RetryAfter: time.Minute}, modelB, false},
		{"transient retry-after parks", &upstream.RateLimitError{Status: "rate_limited", Model: modelA, RetryAfter: time.Minute}, modelB, false},
		{"window ceiling parks every model", func() *upstream.RateLimitError {
			r := quotaFor(modelA)
			r.WindowHours = 24
			r.FreebucksShortfall = true
			return r
		}(), modelB, false},
		{"window ceiling parks its own model too", func() *upstream.RateLimitError {
			r := quotaFor(modelA)
			r.WindowHours = 24
			r.FreebucksShortfall = true
			return r
		}(), modelA, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := canServeOtherModel(tc.rle, tc.model); got != tc.want {
				t.Errorf("canServeOtherModel(%v, %q) = %v, want %v", tc.rle, tc.model, got, tc.want)
			}
		})
	}
}
