package upstream

import (
	"context"
	"freebucks-proxy/backend/internal/testutil"
	"net/http"
	"slices"
	"testing"
	"time"
)

// TestParseFreebucks pins issue #232 with the issue #321 wire shape: the
// session response's "freebucks" block is parsed into SessionState.Freebucks
// (balance, daily window with resetAt, wallet, spend ceiling, planId,
// prices); absent block stays nil.
func TestParseFreebucks(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"active","instanceId":"inst-fb","model":"openai/gpt-5.6-luna","expiresAt":"2030-01-01T00:00:00Z","freebucks":{"balance":17.5,"daily":{"limit":20,"spent":5,"remaining":15,"resetAt":"2026-09-01T07:00:00Z"},"wallet":{"balance":2.5,"monthlyBonus":10,"nextBonusAt":"2026-10-01T07:00:00Z"},"spend":{"limitUsd":15,"resetAt":"2026-09-01T07:00:00Z"},"monthly":{"limitUsd":50,"spentUsd":10,"remainingUsd":40,"resetAt":"2026-10-01T07:00:00Z"},"planId":"starter","prices":{"openai/gpt-5.6-luna":2}}}`))
	}

	client, err := NewForAuth(testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.ProbeAccount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Freebucks == nil {
		t.Fatal("Freebucks = nil, want parsed block")
		return
	}
	fb := st.Freebucks
	if fb.Balance != 17.5 {
		t.Errorf("Balance = %v, want 17.5", fb.Balance)
	}
	if fb.Daily.Limit != 20 || fb.Daily.Remaining != 15 {
		t.Errorf("Daily = %+v, want limit 20 remaining 15", fb.Daily)
	}
	if fb.Daily.ResetAt.IsZero() {
		t.Error("Daily.ResetAt zero, want parsed time")
	}
	if fb.Wallet.Balance != 2.5 {
		t.Errorf("Wallet.Balance = %v, want 2.5", fb.Wallet.Balance)
	}
	if fb.Wallet.MonthlyBonus != 10 {
		t.Errorf("Wallet.MonthlyBonus = %v, want 10", fb.Wallet.MonthlyBonus)
	}
	if fb.Wallet.NextBonusAt.IsZero() {
		t.Error("Wallet.NextBonusAt zero, want parsed time")
	}
	if fb.Spend.LimitUsd != 15 {
		t.Errorf("Spend.LimitUsd = %v, want 15", fb.Spend.LimitUsd)
	}
	if fb.Spend.ResetAt.IsZero() {
		t.Error("Spend.ResetAt zero, want parsed time")
	}
	if fb.PlanID != "starter" {
		t.Errorf("PlanID = %q, want starter", fb.PlanID)
	}
	if fb.Prices["openai/gpt-5.6-luna"] != 2 {
		t.Errorf("Prices = %v, want luna 2", fb.Prices)
	}
	if fb.Monthly == nil {
		t.Fatal("Monthly = nil, want parsed allowance")
		return
	}
	if fb.Monthly.LimitUsd != 50 || fb.Monthly.SpentUsd != 10 || fb.Monthly.RemainingUsd != 40 {
		t.Errorf("Monthly = %+v, want limit 50 spent 10 remaining 40", fb.Monthly)
	}
	if fb.Monthly.ResetAt.IsZero() {
		t.Error("Monthly.ResetAt zero, want parsed time")
	}
}

// TestParseFreebucksAbsent: no freebucks block -> nil, other fields intact.
func TestParseFreebucksAbsent(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"active","instanceId":"inst-plain","model":"mimo/mimo-v2.5","expiresAt":"2030-01-01T00:00:00Z"}`))
	}
	client, err := NewForAuth(testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.ProbeAccount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Freebucks != nil {
		t.Errorf("Freebucks = %+v, want nil", st.Freebucks)
	}
	if st.InstanceID != "inst-plain" {
		t.Errorf("InstanceID = %q, want inst-plain", st.InstanceID)
	}
}

// TestParseFreebucksPriceDrift pins issue #350: quotaExempt, priceNotices
// and priceChanges parse; due changes apply at parse time (reprice +
// notice refresh + consume), future ones are kept, and models off the
// meter are never repriced.
func TestParseFreebucksPriceDrift(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"active","instanceId":"inst-fb350","model":"openai/gpt-5.6-luna","expiresAt":"2030-01-01T00:00:00Z","freebucks":{"balance":0,"daily":{"limit":20,"spent":20,"remaining":0,"resetAt":"2026-09-01T07:00:00Z"},"quotaExempt":true,"prices":{"openai/gpt-5.6-luna":2},"priceNotices":{"openai/gpt-5.6-luna":"old copy"},"priceChanges":[{"at":"2020-01-02T00:00:00Z","modelId":"openai/gpt-5.6-luna","price":5,"tagline":"new copy"},{"at":"2020-01-01T00:00:00Z","modelId":"openai/gpt-5.6-luna","price":3,"tagline":"mid copy"},{"at":"2999-01-01T00:00:00Z","modelId":"openai/gpt-5.6-luna","price":9,"tagline":"future copy"},{"at":"2020-01-03T00:00:00Z","modelId":"off/meter","price":1,"tagline":"unpriced"}]}}`))
	}
	client, err := NewForAuth(testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.ProbeAccount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fb := st.Freebucks
	if fb == nil {
		t.Fatal("Freebucks = nil, want parsed block")
		return
	}
	if !fb.QuotaExempt {
		t.Error("QuotaExempt = false, want true")
	}
	// Due changes apply oldest-first: 2 -> 3 -> 5.
	if fb.Prices["openai/gpt-5.6-luna"] != 5 {
		t.Errorf("Prices[luna] = %v, want 5 (last due change wins)", fb.Prices["openai/gpt-5.6-luna"])
	}
	if fb.PriceNotices["openai/gpt-5.6-luna"] != "new copy" {
		t.Errorf("PriceNotices[luna] = %q, want new copy", fb.PriceNotices["openai/gpt-5.6-luna"])
	}
	if _, ok := fb.Prices["off/meter"]; ok {
		t.Error("off-meter model repriced, want untouched")
	}
	if len(fb.PriceChanges) != 1 || fb.PriceChanges[0].Tagline != "future copy" {
		t.Errorf("PriceChanges = %+v, want only the future change", fb.PriceChanges)
	}
}

// TestApplyFreebucksPriceChangesUnit: bad timestamps are kept pending (never
// applied), nil prices map is allocated, empty schedule is a no-op.
func TestApplyFreebucksPriceChangesUnit(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	fb := &FreebucksInfo{
		Prices: map[string]float64{"m": 2},
		PriceChanges: []FreebucksPriceChange{
			{At: "not-a-time", ModelID: "m", Price: 7, Tagline: "bad"},
			{At: "2020-05-05T00:00:00Z", ModelID: "m", Price: 4, Tagline: "due"},
		},
	}
	ApplyFreebucksPriceChanges(fb, now)
	if fb.Prices["m"] != 4 {
		t.Errorf("Prices[m] = %v, want 4", fb.Prices["m"])
	}
	if len(fb.PriceChanges) != 1 || fb.PriceChanges[0].Tagline != "bad" {
		t.Errorf("PriceChanges = %+v, want only the unparsable change kept", fb.PriceChanges)
	}
	var nilFB *FreebucksInfo
	ApplyFreebucksPriceChanges(nilFB, now) // must not panic
	empty := &FreebucksInfo{}
	ApplyFreebucksPriceChanges(empty, now) // must not panic
}

// TestParseFreebucksFirstTabDiscount pins the vendor 6cd8970 first-tab
// discount wire shape: the daily window carries its reset timezone and the
// block carries the account-wide first-tab offer (amount, availability,
// holder). Absent holder stays nil; absent block stays nil.
func TestParseFreebucksFirstTabDiscount(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"active","instanceId":"inst-ftd","model":"openai/gpt-5.6-luna","expiresAt":"2030-01-01T00:00:00Z","freebucks":{"balance":17.5,"daily":{"limit":20,"spent":5,"remaining":15,"resetAt":"2026-09-01T07:00:00Z","resetTimeZone":"America/New_York"},"prices":{"openai/gpt-5.6-luna":2},"firstTabDiscount":{"amount":3,"available":true,"holder":{"instanceId":null,"surface":"single","expiresAt":"2030-01-01T00:00:00Z"}}}}`))
	}

	client, err := NewForAuth(testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.ProbeAccount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Freebucks == nil {
		t.Fatal("Freebucks = nil, want parsed block")
	}
	fb := st.Freebucks
	if fb.Daily.ResetTimeZone != "America/New_York" {
		t.Errorf("Daily.ResetTimeZone = %q, want America/New_York", fb.Daily.ResetTimeZone)
	}
	if fb.FirstTabDiscount == nil {
		t.Fatal("FirstTabDiscount = nil, want parsed offer")
	}
	d := fb.FirstTabDiscount
	if d.Amount != 3 || !d.Available {
		t.Errorf("FirstTabDiscount = %+v, want amount 3 available", d)
	}
	if d.Holder == nil || d.Holder.Surface != "single" {
		t.Errorf("Holder = %+v, want single-surface holder", d.Holder)
	}
	if d.Holder.InstanceID != nil {
		t.Errorf("Holder.InstanceID = %q, want nil (JSON null)", *d.Holder.InstanceID)
	}
}

// TestApplyFreebucksPriceChangesFirstTabDiscount pins the vendor 6cd8970
// price-changes parity: a due repricing lands discount-adjusted (price minus
// the available first-tab amount, floored at zero), mirroring
// discountedSessionPrice in freebuff-price-changes.ts. Without an available
// discount the full scheduled price still applies.
func TestApplyFreebucksPriceChangesFirstTabDiscount(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	newFB := func() *FreebucksInfo {
		return &FreebucksInfo{
			Prices: map[string]float64{"m": 2},
			PriceChanges: []FreebucksPriceChange{
				{At: "2020-05-05T00:00:00Z", ModelID: "m", Price: 5, Tagline: "repriced"},
			},
			FirstTabDiscount: &FreebucksFirstTabDiscount{Amount: 3, Available: true},
		}
	}
	fb := newFB()
	ApplyFreebucksPriceChanges(fb, now)
	if fb.Prices["m"] != 2 {
		t.Errorf("Prices[m] = %v, want 2 (5 scheduled minus 3 discount)", fb.Prices["m"])
	}
	if fb.PriceNotices["m"] != "repriced" {
		t.Errorf("PriceNotices[m] = %q, want repriced", fb.PriceNotices["m"])
	}

	fb = newFB()
	fb.FirstTabDiscount.Available = false
	ApplyFreebucksPriceChanges(fb, now)
	if fb.Prices["m"] != 5 {
		t.Errorf("Prices[m] = %v, want 5 (unavailable discount changes nothing)", fb.Prices["m"])
	}

	fb = newFB()
	fb.FirstTabDiscount.Amount = 99
	ApplyFreebucksPriceChanges(fb, now)
	if fb.Prices["m"] != 0 {
		t.Errorf("Prices[m] = %v, want 0 (discount floors at zero)", fb.Prices["m"])
	}
}

// TestParseFreebucksListPricesOffPeak pins the vendor 3420c99 wire shape:
// listPrices rides beside prices (pre-discount, display only) and offPeak
// carries the server-owned recurring policy per model id. Absent on older
// servers stays nil — never a zero allocation.
func TestParseFreebucksListPricesOffPeak(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"active","instanceId":"inst-op","model":"openai/gpt-5.6-luna","expiresAt":"2030-01-01T00:00:00Z","freebucks":{"balance":17.5,"daily":{"limit":20,"spent":5,"remaining":15,"resetAt":"2026-09-01T07:00:00Z"},"prices":{"openai/gpt-5.6-luna":5},"listPrices":{"openai/gpt-5.6-luna":15},"firstTabDiscount":{"amount":10,"available":true},"offPeak":{"openai/gpt-5.6-luna":{"startHourUtc":0,"endHourUtc":8,"price":5,"regularPrice":15}}}}`))
	}

	client, err := NewForAuth(testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.ProbeAccount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Freebucks == nil {
		t.Fatal("Freebucks = nil, want parsed block")
	}
	fb := st.Freebucks
	if fb.ListPrices["openai/gpt-5.6-luna"] != 15 {
		t.Errorf("ListPrices = %v, want luna 15", fb.ListPrices)
	}
	if fb.Prices["openai/gpt-5.6-luna"] != 5 {
		t.Errorf("Prices = %v, want luna 5 (discounted, not list)", fb.Prices)
	}
	op, ok := fb.OffPeak["openai/gpt-5.6-luna"]
	if !ok {
		t.Fatal("OffPeak missing luna entry, want parsed policy")
	}
	if op.StartHourUtc != 0 || op.EndHourUtc != 8 || op.Price != 5 || op.RegularPrice != 15 {
		t.Errorf("OffPeak[luna] = %+v, want 0-8 price 5 regular 15", op)
	}
}

// TestParseFreebucksPlanRequiredModelIDs pins the vendor c2d2958b wire shape:
// the Freebucks block's per-viewer plan-lock verdict parses into
// FreebucksInfo.PlanRequiredModelIDs. A present verdict is copied verbatim
// (including an EMPTY one, which is the server saying "no row is gated for
// this viewer" and must stay distinguishable from an absent field, whose nil
// means "fall back to the static list").
func TestParseFreebucksPlanRequiredModelIDs(t *testing.T) {
	cases := []struct {
		name    string
		verdict string
		want    []string
		wantNil bool
	}{
		{
			name:    "verdict present",
			verdict: `,"planRequiredModelIds":["mimo/mimo-v2.6-pro","openai/gpt-5.6-luna"]`,
			want:    []string{"mimo/mimo-v2.6-pro", "openai/gpt-5.6-luna"},
		},
		{
			name:    "verdict present and empty",
			verdict: `,"planRequiredModelIds":[]`,
			want:    []string{},
		},
		{
			name:    "field absent",
			verdict: "",
			wantNil: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":"active","instanceId":"inst-plan","model":"openai/gpt-5.6-luna","expiresAt":"2030-01-01T00:00:00Z","freebucks":{"balance":17.5,"daily":{"limit":20,"spent":5,"remaining":15,"resetAt":"2026-09-01T07:00:00Z"},"prices":{"openai/gpt-5.6-luna":5}` + tc.verdict + `}}`))
			}
			client, err := NewForAuth(testConfig(mock.URL(), nil))
			if err != nil {
				t.Fatal(err)
			}
			st, err := client.ProbeAccount(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if st.Freebucks == nil {
				t.Fatal("Freebucks = nil, want parsed block")
			}
			got := st.Freebucks.PlanRequiredModelIDs
			if tc.wantNil {
				if got != nil {
					t.Errorf("PlanRequiredModelIDs = %v, want nil for an absent field", got)
				}
				return
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("PlanRequiredModelIDs = %v, want %v", got, tc.want)
			}
		})
	}
}
