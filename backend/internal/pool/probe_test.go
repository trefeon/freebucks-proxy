package pool

import (
	"context"
	"freebuff-proxy/backend/internal/testutil"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestProbeTokenDetailed_BannedQuarantines(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.Ban = true

	p := newTestPool(t, mock)
	ctx := context.Background()

	outcome, _, err := p.ProbeTokenDetailed(ctx, 0)
	if err == nil {
		t.Fatal("expected ban error, got nil")
	}
	if outcome.Status != "banned" {
		t.Errorf("outcome.Status = %q, want banned", outcome.Status)
	}
	if !outcome.Quarantined {
		t.Error("outcome.Quarantined = false, want true")
	}
	if !p.Snapshot()[0].Quarantined {
		t.Error("pool Snapshot()[0].Quarantined = false, want true")
	}
}

func TestProbeTokenDetailed_UnbanLiftsQuarantine(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.Ban = true

	p := newTestPool(t, mock)
	ctx := context.Background()

	// Initial probe marks banned
	_, _, _ = p.ProbeTokenDetailed(ctx, 0)
	if !p.Snapshot()[0].Quarantined {
		t.Fatal("expected token to be quarantined initially")
	}

	// Upstream unbans the account
	mock.Ban = false

	outcome, _, err := p.ProbeTokenDetailed(ctx, 0)
	if err != nil {
		t.Fatalf("probe after unban returned error: %v", err)
	}
	if outcome.Status != "ok" {
		t.Errorf("outcome.Status = %q, want ok", outcome.Status)
	}
	if outcome.Quarantined {
		t.Error("outcome.Quarantined = true, want false")
	}
	if p.Snapshot()[0].Quarantined {
		t.Error("pool Snapshot()[0].Quarantined = true after unban, want false")
	}
}

func TestProbeTokenDetailed_ZeroFreebucksLocksUntilReset(t *testing.T) {
	resetTime := time.Now().Add(4 * time.Hour).UTC().Truncate(time.Second)
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/freebuff/session" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"status": "active",
				"instanceId": "inst-123",
				"freebucks": {
					"balance": 0,
					"daily": {
						"limit": 20,
						"spent": 20,
						"remaining": 0,
						"resetAt": "` + resetTime.Format(time.RFC3339) + `"
					},
					"prices": {
						"upstage/solar-pro4": 2
					}
				}
			}`))
		}
	}

	p := newTestPool(t, mock)

	outcome, _, err := p.ProbeTokenDetailed(context.Background(), 0)
	if err != nil {
		t.Fatalf("unexpected probe error: %v", err)
	}
	if outcome.Status != "freebucks_exhausted" {
		t.Errorf("outcome.Status = %q, want freebucks_exhausted", outcome.Status)
	}
	if !outcome.Cooling {
		t.Error("outcome.Cooling = false, want true")
	}
	if p.Snapshot()[0].CooldownUntil.IsZero() {
		t.Error("pool Snapshot()[0].CooldownUntil is zero, want cooling down until reset")
	}
}

func TestProbeAllTokens(t *testing.T) {
	mock1 := testutil.NewMock()
	defer mock1.Close()
	mock2 := testutil.NewMock()
	defer mock2.Close()

	p := newTestPool(t, mock1, mock2)

	outcomes, err := p.ProbeAllTokens(context.Background())
	if err != nil {
		t.Fatalf("ProbeAllTokens failed: %v", err)
	}
	if len(outcomes) != 2 {
		t.Fatalf("len(outcomes) = %d, want 2", len(outcomes))
	}
	for i, o := range outcomes {
		if o.Index != i {
			t.Errorf("outcome[%d].Index = %d, want %d", i, o.Index, i)
		}
		if o.Status != "ok" {
			t.Errorf("outcome[%d].Status = %q, want ok", i, o.Status)
		}
	}
}

func TestProbeTokenDetailed_IdleLiftsBanQuarantine(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SetBan(true)

	p := newTestPool(t, mock)
	ctx := context.Background()

	// Initial probe marks banned (nil state, non-nil error).
	_, st, err := p.ProbeTokenDetailed(ctx, 0)
	if err == nil {
		t.Fatal("expected ban error, got nil")
	}
	if st != nil {
		t.Fatalf("banned probe state = %+v, want nil", st)
	}
	if !p.Snapshot()[0].Quarantined {
		t.Fatal("expected token to be quarantined initially")
	}

	// Upstream unbans the account and it goes idle: no active session.
	mock.SetBan(false)
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"session not found"}`))
	}

	outcome, st, err := p.ProbeTokenDetailed(ctx, 0)
	if err != nil {
		t.Fatalf("idle probe after unban returned error: %v", err)
	}
	if st != nil {
		t.Fatalf("idle probe state = %+v, want nil", st)
	}
	if outcome.Status != "ok" {
		t.Errorf("outcome.Status = %q, want ok", outcome.Status)
	}
	if outcome.Quarantined {
		t.Error("outcome.Quarantined = true, want false")
	}
	if p.Snapshot()[0].Quarantined {
		t.Error("pool Snapshot()[0].Quarantined = true after idle probe, want false")
	}
}

func TestProbeTokenDetailed_NonBanQuarantineSurvivesHealthyProbe(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	p := newTestPool(t, mock)

	// Plant a non-ban quarantine directly: a healthy probe must not touch it.
	toks := p.roster.Load()
	if toks == nil || len(*toks) == 0 {
		t.Fatal("expected non-empty roster")
	}
	(*toks)[0].quarantine.Store(&quarantineState{reason: "spend_limited", detail: "test-only"})

	outcome, _, err := p.ProbeTokenDetailed(context.Background(), 0)
	if err != nil {
		t.Fatalf("healthy probe returned error: %v", err)
	}
	if outcome.Status != "ok" {
		t.Errorf("outcome.Status = %q, want ok", outcome.Status)
	}
	if !p.Snapshot()[0].Quarantined {
		t.Error("pool Snapshot()[0].Quarantined = false, want true (non-ban quarantine must survive)")
	}
	if q := (*toks)[0].quarantine.Load(); q == nil || q.reason != "spend_limited" {
		t.Errorf("quarantine reason = %+v, want reason spend_limited preserved", q)
	}
}

func TestProbeTokenDetailed_QuotaExemptZeroBalanceIsOK(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/freebuff/session" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"status": "active",
				"instanceId": "inst-exempt",
				"freebucks": {
					"balance": 0,
					"daily": {
						"limit": 20,
						"spent": 20,
						"remaining": 0,
						"resetAt": "` + time.Now().Add(4*time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339) + `"
					},
					"quotaExempt": true,
					"prices": {
						"upstage/solar-pro4": 2
					}
				}
			}`))
		}
	}

	p := newTestPool(t, mock)

	outcome, _, err := p.ProbeTokenDetailed(context.Background(), 0)
	if err != nil {
		t.Fatalf("unexpected probe error: %v", err)
	}
	if outcome.Status != "ok" {
		t.Errorf("outcome.Status = %q, want ok (quota-exempt zero balance is not exhausted)", outcome.Status)
	}
	if outcome.SpendableFB != 0 {
		t.Errorf("outcome.SpendableFB = %v, want 0", outcome.SpendableFB)
	}
	if outcome.Cooling {
		t.Error("outcome.Cooling = true, want false")
	}
	if !p.Snapshot()[0].CooldownUntil.IsZero() {
		t.Error("pool Snapshot()[0].CooldownUntil is non-zero, want no exhaustion cooldown")
	}
}

func TestProbeAllTokens_PreservesOrderMixedOutcomes(t *testing.T) {
	slowOK := testutil.NewMock()
	defer slowOK.Close()
	slowOK.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		// Finish last so any position-based (rather than index-based)
		// collection would mis-order the results.
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"active","instanceId":"inst-slow"}`))
	}

	banned := testutil.NewMock()
	defer banned.Close()
	banned.SetBan(true)

	exhausted := testutil.NewMock()
	defer exhausted.Close()
	exhausted.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"status": "active",
			"instanceId": "inst-empty",
			"freebucks": {
				"balance": 0,
				"daily": {
					"limit": 20,
					"spent": 20,
					"remaining": 0,
					"resetAt": "` + time.Now().Add(4*time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339) + `"
				},
				"prices": {
					"upstage/solar-pro4": 2
				}
			}
		}`))
	}

	p := newTestPool(t, slowOK, banned, exhausted)

	outcomes, err := p.ProbeAllTokens(context.Background())
	if err != nil {
		t.Fatalf("ProbeAllTokens failed: %v", err)
	}
	if len(outcomes) != 3 {
		t.Fatalf("len(outcomes) = %d, want 3", len(outcomes))
	}
	want := []string{"ok", "banned", "freebucks_exhausted"}
	for i, w := range want {
		if outcomes[i].Index != i {
			t.Errorf("outcome[%d].Index = %d, want %d", i, outcomes[i].Index, i)
		}
		if outcomes[i].Status != w {
			t.Errorf("outcome[%d].Status = %q, want %q", i, outcomes[i].Status, w)
		}
	}
}

func TestProbeTokenDetailed_EffectivelyDeadBelowCheapestPrice(t *testing.T) {
	resetTime := time.Now().Add(4 * time.Hour).UTC().Truncate(time.Second)
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/freebuff/session" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"status": "active",
				"instanceId": "inst-dust",
				"freebucks": {
					"balance": 0.5,
					"daily": {
						"limit": 20,
						"spent": 19.5,
						"remaining": 0.5,
						"resetAt": "` + resetTime.Format(time.RFC3339) + `"
					},
					"prices": {
						"upstage/solar-pro4": 2
					}
				}
			}`))
		}
	}

	p := newTestPool(t, mock)

	outcome, _, err := p.ProbeTokenDetailed(context.Background(), 0)
	if err != nil {
		t.Fatalf("unexpected probe error: %v", err)
	}
	if outcome.Status != "freebucks_exhausted" {
		t.Errorf("outcome.Status = %q, want freebucks_exhausted (0.5 < cheapest price 2)", outcome.Status)
	}
	if !outcome.Cooling {
		t.Error("outcome.Cooling = false, want true")
	}
	if p.Snapshot()[0].CooldownUntil.IsZero() {
		t.Error("pool Snapshot()[0].CooldownUntil is zero, want cooling down until reset")
	}
}

func TestProbeTokenDetailed_MonthlyExhaustedLocksDespiteDailyBalance(t *testing.T) {
	dailyReset := time.Now().Add(4 * time.Hour).UTC().Truncate(time.Second)
	monthReset := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second)
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/freebuff/session" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"status": "active",
				"instanceId": "inst-monthly",
				"freebucks": {
					"balance": 10,
					"daily": {
						"limit": 20,
						"spent": 10,
						"remaining": 10,
						"resetAt": "` + dailyReset.Format(time.RFC3339) + `"
					},
					"monthly": {
						"limitUsd": 50,
						"spentUsd": 50,
						"remainingUsd": 0,
						"resetAt": "` + monthReset.Format(time.RFC3339) + `"
					},
					"prices": {
						"upstage/solar-pro4": 2
					}
				}
			}`))
		}
	}

	p := newTestPool(t, mock)

	outcome, _, err := p.ProbeTokenDetailed(context.Background(), 0)
	if err != nil {
		t.Fatalf("unexpected probe error: %v", err)
	}
	if outcome.Status != "freebucks_exhausted" {
		t.Errorf("outcome.Status = %q, want freebucks_exhausted (monthly spent despite daily balance)", outcome.Status)
	}
	if !strings.Contains(outcome.Detail, "monthly") {
		t.Errorf("outcome.Detail = %q, want monthly-allowance cause", outcome.Detail)
	}
	// Earliest future recovery wins (daily refill first; monthly re-locks after).
	if outcome.ResetAt != dailyReset.Format(time.RFC3339) {
		t.Errorf("outcome.ResetAt = %q, want daily reset %q", outcome.ResetAt, dailyReset.Format(time.RFC3339))
	}
}

func TestProbeTokenDetailed_MonthlyExhaustedDespiteExempt(t *testing.T) {
	monthReset := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second)
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/freebuff/session" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"status": "active",
				"instanceId": "inst-exempt-monthly",
				"freebucks": {
					"balance": 10,
					"daily": {
						"limit": 20,
						"spent": 10,
						"remaining": 10,
						"resetAt": "` + time.Now().Add(-2*time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339) + `"
					},
					"monthly": {
						"limitUsd": 50,
						"spentUsd": 50,
						"remainingUsd": 0,
						"resetAt": "` + monthReset.Format(time.RFC3339) + `"
					},
					"quotaExempt": true,
					"prices": {
						"upstage/solar-pro4": 2
					}
				}
			}`))
		}
	}

	p := newTestPool(t, mock)

	outcome, _, err := p.ProbeTokenDetailed(context.Background(), 0)
	if err != nil {
		t.Fatalf("unexpected probe error: %v", err)
	}
	if outcome.Status != "freebucks_exhausted" {
		t.Errorf("outcome.Status = %q, want freebucks_exhausted (monthly gates even when exempt)", outcome.Status)
	}
	// Past daily reset is skipped as a recovery candidate; monthly reset wins.
	if outcome.ResetAt != monthReset.Format(time.RFC3339) {
		t.Errorf("outcome.ResetAt = %q, want monthly reset %q", outcome.ResetAt, monthReset.Format(time.RFC3339))
	}
}

func TestProbeAllTokens_EmptyRoster(t *testing.T) {
	p := newTestPool(t)

	outcomes, err := p.ProbeAllTokens(context.Background())
	if err != nil {
		t.Fatalf("ProbeAllTokens on empty roster failed: %v", err)
	}
	if outcomes == nil {
		t.Error("outcomes is nil, want non-nil empty slice (JSON encodes [])")
	}
	if len(outcomes) != 0 {
		t.Errorf("len(outcomes) = %d, want 0", len(outcomes))
	}
}
