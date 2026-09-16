package pool

import (
	"context"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
)

// fireGateClock returns a fake clock at offset before the next Pacific
// midnight, pinning the pre-flight / fire-gate / end-buffer zones
// deterministically regardless of wall time.
func fireGateClock(offset time.Duration) time.Time {
	_, end := maturityWindowFor(time.Now())
	return end.Add(-offset)
}

// seedFireReady enrolls one token with a fresh streak, a long-past slot,
// and the default unmetered touch model: the pass is one tick from firing.
func seedFireReady(t *testing.T, p *Pool, token int, streak int, now time.Time) {
	t.Helper()
	seedStreak(p, token, streak, false, now)
	if err := p.SetMaturity(token, true, 7, "", ""); err != nil {
		t.Fatal(err)
	}
	setMaturitySlot(p, token, now.Add(-time.Hour), laDay(now))
}

// Pre-flight classifies but never fires: a fire-ready token ledgers
// skip:preflight with zero upstream admissions, while a gated token still
// gets its real skip code (classification runs, firing doesn't).
func TestMaturityPreflightNeverFires(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newMaturityPool(t, mock)
	now := fireGateClock(10 * time.Minute)
	seedFireReady(t, p, 0, 2, now)

	p.maturityTickAt(context.Background(), now)

	if got := mock.SessionCreatesSnapshot(); got != 0 {
		t.Errorf("SessionCreates = %d, want 0 (pre-flight never fires)", got)
	}
	if got := mock.SessionProbesSnapshot(); got != 0 {
		t.Errorf("SessionProbes = %d, want 0 (pre-flight never probes)", got)
	}
	if _, result := maturityResult(p, 0); result != "skip:preflight" {
		t.Errorf("result = %q, want skip:preflight", result)
	}

	// Classification still runs pre-flight: a locked token ledgers its
	// real gate code, not preflight.
	mock2 := testutil.NewMock()
	defer mock2.Close()
	p2 := newMaturityPool(t, mock2)
	seedFireReady(t, p2, 0, 2, now)
	toks := p2.roster.Load()
	(*toks)[0].locked.Store(true)

	p2.maturityTickAt(context.Background(), now)

	if got := mock2.SessionCreatesSnapshot(); got != 0 {
		t.Errorf("locked SessionCreates = %d, want 0", got)
	}
	if _, result := maturityResult(p2, 0); result != "skip:locked" {
		t.Errorf("locked result = %q, want skip:locked", result)
	}
}

// The gate boundaries are exact: T-6m holds (pre-flight), T-5m fires
// (gate start inclusive), T-1m holds (gate end exclusive), and post-reset
// leaves the ledger untouched.
func TestMaturityFireGateBoundaries(t *testing.T) {
	t.Run("T-6m holds", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		p := newMaturityPool(t, mock)
		now := fireGateClock(6 * time.Minute)
		seedFireReady(t, p, 0, 2, now)

		p.maturityTickAt(context.Background(), now)

		if got := mock.SessionCreatesSnapshot(); got != 0 {
			t.Errorf("SessionCreates = %d, want 0 (outside the gate)", got)
		}
		if _, result := maturityResult(p, 0); result != "skip:preflight" {
			t.Errorf("result = %q, want skip:preflight", result)
		}
	})

	t.Run("T-5m fires", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		p := newMaturityPool(t, mock)
		now := fireGateClock(5 * time.Minute)
		seedFireReady(t, p, 0, 2, now)

		p.maturityTickAt(context.Background(), now)

		if got := mock.SessionCreatesSnapshot(); got != 1 {
			t.Errorf("SessionCreates = %d, want 1 (gate start inclusive)", got)
		}
		if action, result := maturityResult(p, 0); action != "admit" || result != "ok" {
			t.Errorf("last touch = %q/%q, want admit/ok", action, result)
		}
	})

	t.Run("T-1m holds", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		p := newMaturityPool(t, mock)
		now := fireGateClock(time.Minute)
		seedFireReady(t, p, 0, 2, now)

		p.maturityTickAt(context.Background(), now)

		if got := mock.SessionCreatesSnapshot(); got != 0 {
			t.Errorf("SessionCreates = %d, want 0 (gate end exclusive)", got)
		}
		if _, result := maturityResult(p, 0); result != "skip:window-exhausted" {
			t.Errorf("result = %q, want skip:window-exhausted", result)
		}
	})

	t.Run("post-reset untouched", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		p := newMaturityPool(t, mock)
		_, end := maturityWindowFor(time.Now())
		now := end.Add(time.Minute)
		seedFireReady(t, p, 0, 2, now)

		p.maturityTickAt(context.Background(), now)

		if got := mock.SessionCreatesSnapshot(); got != 0 {
			t.Errorf("SessionCreates = %d, want 0 (past the reset)", got)
		}
		if action, result := maturityResult(p, 0); action != "" || result != "" {
			t.Errorf("ledger = %q/%q, want untouched past the reset", action, result)
		}
	})
}

// A token eligible all night but still unfired when the gate closes keeps
// an honest ledger: pre-flight records the hold, the post-gate pass
// records the miss — never a silent drop, never a late fire.
func TestMaturityWindowExhaustedTransition(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newMaturityPool(t, mock)
	pre := fireGateClock(10 * time.Minute)
	seedFireReady(t, p, 0, 2, pre)

	p.maturityTickAt(context.Background(), pre)
	if _, result := maturityResult(p, 0); result != "skip:preflight" {
		t.Fatalf("pre-flight result = %q, want skip:preflight", result)
	}

	p.maturityTickAt(context.Background(), fireGateClock(30*time.Second))

	if got := mock.SessionCreatesSnapshot(); got != 0 {
		t.Errorf("SessionCreates = %d, want 0 (gate closed, never late-fires)", got)
	}
	if _, result := maturityResult(p, 0); result != "skip:window-exhausted" {
		t.Errorf("result = %q, want skip:window-exhausted", result)
	}
}

// The sweep fires oldest-streak-first: ascending cached streak, so the
// lowest-streak accounts claim firing room before higher-streak ones.
func TestMaturityOldestStreakFirst(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	mock2 := testutil.NewMock()
	defer mock2.Close()
	p := newTestPoolCfg(t, func(cfg *config.Config) {
		cfg.MaturityEnabled = true
		cfg.MaturityTouchModel = modelB
		cfg.MaturityTargetDays = 7
	}, mock0, mock1, mock2)
	now := fireGateClock(3 * time.Minute)
	seedFireReady(t, p, 0, 9, now)
	seedFireReady(t, p, 1, 3, now)
	seedFireReady(t, p, 2, 6, now)
	sink := &recordingSink{}
	p.SetHistorySink(sink)

	p.maturityTickAt(context.Background(), now)

	for i, m := range []*testutil.MockUpstream{mock0, mock1, mock2} {
		if got := m.SessionCreatesSnapshot(); got != 1 {
			t.Errorf("token %d SessionCreates = %d, want 1 (all eligible fire)", i, got)
		}
	}
	var fired []int
	sink.mu.Lock()
	for _, e := range sink.got {
		if e.Kind == "touch" && strings.HasPrefix(e.Detail, "admit ok") {
			fired = append(fired, e.TokenIdx)
		}
	}
	sink.mu.Unlock()
	want := []int{1, 2, 0}
	if len(fired) != len(want) {
		t.Fatalf("fire order = %v, want %v", fired, want)
	}
	for i := range want {
		if fired[i] != want[i] {
			t.Errorf("fire order = %v, want %v (oldest-streak-first)", fired, want)
			break
		}
	}
}
