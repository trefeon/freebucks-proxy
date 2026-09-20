package pool

import (
	"context"
	"errors"
	"freebucks-proxy/backend/internal/testutil"
	"freebucks-proxy/backend/internal/upstream"
	"io"
	"net/http"
	"testing"
)

// TestProbeTokenDetailedIdleMeterPopulatesSnapshot pins the pre-session
// populate path end to end: a 200 none-with-balance probe persists
// Freebucks/quota/referral/standing into the session snapshot without ever
// holding a slot, and reports ok with a nil error.
func TestProbeTokenDetailedIdleMeterPopulatesSnapshot(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var gotInstance string
	var posts int
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/freebuff/session" {
			gotInstance = r.Header.Get("x-freebuff-instance-id")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"none","accessTier":"free",`+
			`"rateLimitsByModel":{"deepseek/deepseek-v4-flash":{"model":"deepseek/deepseek-v4-flash","limit":6,"recentCount":2,"period":"pacific_day","resetAt":"2026-09-01T07:00:00Z"}},`+
			`"referral":{"code":"ABC123","qualifiedCount":1},`+
			`"standing":{"level":"trusted","label":"Trusted","score":80},`+
			`"freebucks":{"balance":17.5,"daily":{"limit":20,"spent":5,"remaining":15,"resetAt":"2026-09-01T07:00:00Z"},"prices":{"deepseek/deepseek-v4-flash":2}}}`)
	}

	p := newTestPool(t, mock)

	outcome, st, err := p.ProbeTokenDetailed(context.Background(), 0)
	if err != nil {
		t.Fatalf("idle-with-balance probe returned error: %v", err)
	}
	if st == nil || st.Status != "none" {
		t.Fatalf("probe state = %+v, want non-nil none state", st)
	}
	if outcome.Status != "ok" {
		t.Errorf("outcome.Status = %q, want ok", outcome.Status)
	}
	snap := p.Snapshot()[0]
	if snap.Freebucks == nil || snap.Freebucks.Balance != 17.5 {
		t.Errorf("Snapshot().Freebucks = %+v, want balance 17.5", snap.Freebucks)
	}
	if snap.Referral == nil || snap.Referral.Code != "ABC123" {
		t.Errorf("Snapshot().Referral = %+v, want code ABC123", snap.Referral)
	}
	if snap.Standing == nil || snap.Standing.Level != "trusted" {
		t.Errorf("Snapshot().Standing = %+v, want level trusted", snap.Standing)
	}
	if q, ok := snap.QuotaByModel["deepseek/deepseek-v4-flash"]; !ok || q.Limit != 6 {
		t.Errorf("Snapshot().QuotaByModel = %+v, want flash quota limit 6", snap.QuotaByModel)
	}
	if gotInstance != "" {
		t.Errorf("x-freebuff-instance-id = %q, want absent (zero-cost)", gotInstance)
	}
	if posts != 0 {
		t.Errorf("admission POSTs = %d, want 0", posts)
	}
}

// TestProbeTokenIdlePropagatesStateWithSentinel pins the bridge re-emit:
// an idle token reports ErrNoActiveSession ALONGSIDE its meter state, so
// dashboard callers branch unchanged and still read the balance.
func TestProbeTokenIdlePropagatesStateWithSentinel(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"none","freebucks":{"balance":17.5,"daily":{"limit":20,"spent":5,"remaining":15,"resetAt":"2026-09-01T07:00:00Z"},"prices":{"deepseek/deepseek-v4-flash":2}}}`)
	}

	p := newTestPool(t, mock)

	st, err := p.ProbeToken(context.Background(), 0)
	if !errors.Is(err, upstream.ErrNoActiveSession) {
		t.Fatalf("err = %v, want ErrNoActiveSession", err)
	}
	if st == nil || st.Status != "none" {
		t.Fatalf("state = %+v, want none state alongside the sentinel", st)
	}
	if st.Freebucks == nil || st.Freebucks.Balance != 17.5 {
		t.Errorf("state.Freebucks = %+v, want balance 17.5", st.Freebucks)
	}
}

// TestProbeTokenDetailedExposesTierAndOffers pins the plan-tier / offer
// passthrough end to end: a pre-join probe's subscription.tierId,
// limitedOfferReason, and limitedModelOffers reach pool.TokenSnapshot, and
// the offer slice is a detached copy of pooled live state.
func TestProbeTokenDetailedExposesTierAndOffers(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"none","subscription":{"tierId":"plan_pro"},`+
			`"limitedOfferReason":"closed",`+
			`"limitedModelOffers":[{"model":"anthropic/claude-fable-5.1","remaining":7,"total":10,"userRemaining":1,"userResetAt":null}]}`)
	}

	p := newTestPool(t, mock)

	if _, _, err := p.ProbeTokenDetailed(context.Background(), 0); err != nil {
		t.Fatalf("probe returned error: %v", err)
	}
	snap := p.Snapshot()[0]
	if snap.SubscriptionTierID != "plan_pro" {
		t.Errorf("Snapshot().SubscriptionTierID = %q, want plan_pro", snap.SubscriptionTierID)
	}
	if snap.LimitedOfferReason != "closed" {
		t.Errorf("Snapshot().LimitedOfferReason = %q, want closed", snap.LimitedOfferReason)
	}
	if len(snap.LimitedModelOffers) != 1 || snap.LimitedModelOffers[0].Model != upstream.FreebuffFable51ModelID {
		t.Fatalf("Snapshot().LimitedModelOffers = %+v, want one fable offer", snap.LimitedModelOffers)
	}
	if snap.LimitedModelOffers[0].UserResetAt != nil {
		t.Errorf("UserResetAt = %q, want nil (null stays null)", *snap.LimitedModelOffers[0].UserResetAt)
	}
	// The snapshot hands out a copy: mutating it must not leak into the
	// pooled session state (freebucks_clone_test.go copy-bug class).
	snap.LimitedModelOffers[0].Model = "mutated/other"
	if got := p.Snapshot()[0].LimitedModelOffers[0].Model; got != upstream.FreebuffFable51ModelID {
		t.Errorf("offer slice aliases pooled state: mutation leaked (%q)", got)
	}
}
