package pool

import (
	"context"
	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
	"testing"
	"time"
)

// smartProbeTestPool is newTestPool with the smart-probe master switch on:
// hand-built Configs zero-value it (production Load defaults it ON), and
// the scheduler tests drive the predicate/fire seams directly.
func smartProbeTestPool(t *testing.T, mocks ...*testutil.MockUpstream) *Pool {
	t.Helper()
	return newTestPoolCfg(t, func(c *config.Config) {
		c.SmartProbeEnabled = true
	}, mocks...)
}

// TestSmartProbeIdleAccountsNeverProbed pins the trigger-only contract: a
// pool with no activity and no quota memory dispatches nothing — no dirty
// marks, no reset instants, zero upstream traffic.
func TestSmartProbeIdleAccountsNeverProbed(t *testing.T) {
	mock0 := testutil.NewMock()
	t.Cleanup(mock0.Close)
	mock1 := testutil.NewMock()
	t.Cleanup(mock1.Close)
	p := smartProbeTestPool(t, mock0, mock1)
	now := time.Now()

	if due := p.smartProbeDue(now); len(due) != 0 {
		t.Fatalf("smartProbeDue on a quiet pool = %v, want none (no full sweeps)", due)
	}
	toks := p.roster.Load()
	for i, tok := range *toks {
		if ok, reason := p.smartProbeDueToken(tok, now); ok || reason != "idle" {
			t.Errorf("token %d due = %v (%q), want false/idle", i, ok, reason)
		}
	}
	p.smartProbeTickAt(context.Background(), now)
	if got := mock0.SessionProbesSnapshot(); got != 0 {
		t.Errorf("account #1 probes = %d, want 0 (idle accounts see zero traffic)", got)
	}
	if got := mock1.SessionProbesSnapshot(); got != 0 {
		t.Errorf("account #2 probes = %d, want 0 (idle accounts see zero traffic)", got)
	}
	if got := mock0.SessionCreatesSnapshot(); got != 0 {
		t.Errorf("account #1 session creates = %d, want 0", got)
	}
}

// TestSmartProbeDirtyAccountFiresOnceZeroCost pins the activity trigger:
// a lease grant marks the account dirty, the next pass fires exactly one
// session-less probe (no new session), and the account goes quiet again.
func TestSmartProbeDirtyAccountFiresOnceZeroCost(t *testing.T) {
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	p := smartProbeTestPool(t, mock)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	lease, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	p.LeaseRelease(lease)

	// The lease grant marked the account dirty; past the fresh gate the
	// mark is due (the +61s also covers an admission that stamps
	// freshness — either way the mark fires exactly once).
	tok := (*p.roster.Load())[0]
	if !tok.probeDirty.Load() {
		t.Fatal("lease grant did not mark the account dirty")
	}
	due := p.smartProbeDue(time.Now().Add(61 * time.Second))
	if len(due) != 1 || due[0] != 0 {
		t.Fatalf("smartProbeDue after lease grant = %v, want [0] (dirty)", due)
	}

	p.smartProbeFireOne(ctx, 0)
	if got := mock.SessionProbesSnapshot(); got != 1 {
		t.Errorf("probes after fire = %d, want 1 (the fire ran)", got)
	}
	if got := mock.SessionCreatesSnapshot(); got != 1 {
		t.Errorf("session creates after fire = %d, want still 1 (probe is zero-cost, never admission)", got)
	}
	if due := p.smartProbeDue(time.Now()); len(due) != 0 {
		t.Errorf("smartProbeDue after fire = %v, want none (single fire, then quiet)", due)
	}
}

// TestSmartProbeResetInstantProbesParkedAccount pins the reset trigger: an
// account with quota memory whose reset instant passed is due even with no
// fresh activity — this is what keeps a parked account's quota truthful.
func TestSmartProbeResetInstantProbesParkedAccount(t *testing.T) {
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	mock.RateLimitsByModel = map[string]any{
		modelA: map[string]any{
			"model":       modelA,
			"limit":       5,
			"recentCount": 4,
			"period":      "pacific_day",
			"resetAt":     "2026-08-16T07:00:00.000Z",
		},
	}
	p := smartProbeTestPool(t, mock)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	lease, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	p.LeaseRelease(lease)

	// Isolate the reset trigger from the lease mark: the parked account has
	// quota memory and a past reset, nothing else.
	tok := (*p.roster.Load())[0]
	tok.probeDirty.Store(false)
	now := time.Now().Add(61 * time.Second)
	ok, reason := p.smartProbeDueToken(tok, now)
	if !ok || reason != "reset" {
		t.Fatalf("parked due = %v (%q), want true/reset (past reset instant)", ok, reason)
	}
	if due := p.smartProbeDue(now); len(due) != 1 || due[0] != 0 {
		t.Fatalf("smartProbeDue = %v, want [0]", due)
	}

	p.smartProbeFireOne(ctx, 0)
	if got := mock.SessionProbesSnapshot(); got != 1 {
		t.Errorf("probes after reset fire = %d, want 1", got)
	}
	if got := mock.SessionCreatesSnapshot(); got != 1 {
		t.Errorf("session creates after reset fire = %d, want still 1 (zero-cost)", got)
	}
}

// TestSmartProbe429RefusalMarksInterest pins the refusal hook: a 429
// recorded through CooldownTokenRateLimit marks the account dirty, and the
// remembered reset instant is what the scheduler fires on once the
// cooldown window lifts.
func TestSmartProbe429RefusalMarksInterest(t *testing.T) {
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	p := smartProbeTestPool(t, mock)

	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	p.CooldownTokenRateLimit(0, &upstream.RateLimitError{
		Status:     "rate_limited",
		RetryAfter: time.Second,
		ResetAt:    reset,
	})
	tok := (*p.roster.Load())[0]
	if !tok.probeDirty.Load() {
		t.Fatal("429 refusal did not mark the account dirty")
	}
	// The cooldown window holds the fire back; past it, the dirty mark
	// (with the remembered reset behind it) fires.
	if ok, reason := p.smartProbeDueToken(tok, time.Now()); ok || reason != "cooling" {
		t.Fatalf("due during cooldown = %v (%q), want false/cooling", ok, reason)
	}
	if ok, _ := p.smartProbeDueToken(tok, time.Now().Add(2*time.Second)); !ok {
		t.Fatal("due past the cooldown = false, want true (429 interest + remembered reset)")
	}
}

// TestSmartProbeRetryDelayDoubling pins the backoff math: the first probe
// 429 doubles the base interval, each consecutive one doubles again, and
// the configured cap (default 30m) always wins.
func TestSmartProbeRetryDelayDoubling(t *testing.T) {
	cap := 30 * time.Minute
	for step, want := range map[int64]time.Duration{
		1: 10 * time.Minute,
		2: 20 * time.Minute,
		3: 30 * time.Minute,
		4: 30 * time.Minute,
	} {
		if got := smartProbeRetryDelay(step, cap); got != want {
			t.Errorf("retryDelay(step %d) = %s, want %s", step, got, want)
		}
	}
	if got := smartProbeRetryDelay(2, 12*time.Minute); got != 12*time.Minute {
		t.Errorf("retryDelay(step 2, cap 12m) = %s, want 12m (cap wins)", got)
	}
	if got := smartProbeRetryDelay(1, 0); got != 10*time.Minute {
		t.Errorf("retryDelay(step 1, zero cap) = %s, want 10m (default 30m cap)", got)
	}
}

// TestSmartProbeProbe429BackoffThroughFire pins the doubling through the
// real fire path: consecutive probe-429 outcomes step the schedule
// 10m → 20m with zero sessions created throughout.
func TestSmartProbeProbe429BackoffThroughFire(t *testing.T) {
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	p := smartProbeTestPool(t, mock)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mock.SetRateLimit(true)
	t.Cleanup(func() { mock.SetRateLimit(false) })

	p.smartProbeFireOne(ctx, 0)
	tok := (*p.roster.Load())[0]
	if got := tok.probeBackoffStep.Load(); got != 1 {
		t.Fatalf("backoff step after first probe-429 = %d, want 1", got)
	}
	if d := time.Until(time.Unix(0, tok.probeNextAt.Load())); d < 9*time.Minute || d > 10*time.Minute {
		t.Errorf("retry delay after first probe-429 = %s, want ~10m", d)
	}

	p.smartProbeFireOne(ctx, 0)
	if got := tok.probeBackoffStep.Load(); got != 2 {
		t.Fatalf("backoff step after second probe-429 = %d, want 2", got)
	}
	if d := time.Until(time.Unix(0, tok.probeNextAt.Load())); d < 19*time.Minute || d > 20*time.Minute {
		t.Errorf("retry delay after second probe-429 = %s, want ~20m", d)
	}
	// The mock 429s before its probe counter (every route refuses), so
	// the stepped schedule — not the counter — proves both fires ran
	// their outcome path.
	if got := mock.SessionProbesSnapshot(); got != 0 {
		t.Errorf("probes = %d, want 0 (refused before counting)", got)
	}
	if got := mock.SessionCreatesSnapshot(); got != 0 {
		t.Errorf("session creates = %d, want 0 (429 probes create nothing)", got)
	}
}

// TestSmartProbeSkipsUnhealthyAccounts pins the guard order: locked,
// quarantined/banned, cooling, and country-blocked accounts are never
// probed even with interest marked.
func TestSmartProbeSkipsUnhealthyAccounts(t *testing.T) {
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	mock.Ban = true
	p := smartProbeTestPool(t, mock)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := p.Acquire(ctx, modelA); err == nil {
		t.Fatal("acquire on a banned account succeeded, want refusal")
	}
	tok := (*p.roster.Load())[0]
	// Interest marked, guard must still win: the ban memory from the
	// failed admission outranks the dirty mark. The terminal ban also
	// quarantines the account, and the quarantine gate precedes the
	// live-ban gate by order, so the reason is "quarantined".
	p.markProbeDirty(tok)
	ok, reason := p.smartProbeDueToken(tok, time.Now())
	if ok || reason != "quarantined" {
		t.Errorf("banned due = %v (%q), want false/quarantined", ok, reason)
	}
	p.smartProbeTickAt(ctx, time.Now())
	if got := mock.SessionProbesSnapshot(); got != 0 {
		t.Errorf("probes on banned account = %d, want 0", got)
	}

	// A locked account reports locked, never due.
	mock.Ban = false
	if err := p.LockToken(0); err != nil {
		t.Fatalf("LockToken: %v", err)
	}
	t.Cleanup(func() { _ = p.UnlockLockToken(0) })
	if ok, reason := p.smartProbeDueToken(tok, time.Now()); ok || reason != "locked" {
		t.Errorf("locked due = %v (%q), want false/locked", ok, reason)
	}
}

// TestSmartProbeDisabledConfigStaysQuiet pins the master switch: with the
// knob off, the tick dispatches nothing even for a dirty account.
func TestSmartProbeDisabledConfigStaysQuiet(t *testing.T) {
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	p := newTestPool(t, mock) // hand-built Config: switch zero-values off
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	lease, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	p.LeaseRelease(lease)
	p.smartProbeTickAt(ctx, time.Now().Add(61*time.Second))
	if got := mock.SessionProbesSnapshot(); got != 0 {
		t.Errorf("probes with SMART_PROBE_ENABLED off = %d, want 0", got)
	}
}
