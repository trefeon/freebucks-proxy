package pool

import (
	"testing"
	"time"

	"freebucks-proxy/backend/internal/session"
	"freebucks-proxy/backend/internal/upstream"
)

func compactMergeCurrent() session.SessionSnapshot {
	return session.SessionSnapshot{
		Status:     "active",
		InstanceID: "inst-compact-1",
		Model:      "model-a",
		QuotaByModel: map[string]session.QuotaSnapshot{
			"model-a": {Model: "model-a", Limit: 10, RecentCount: 3, ResetAt: time.Now().Add(time.Hour)},
		},
		SubscriptionTierID: "plan_pro",
		Freebucks:          &upstream.FreebucksInfo{Balance: 42, Prices: map[string]float64{"model-a": 2}},
	}
}

// A quota-less compact poll must not blank the quota table: the admission
// values carry across so the dashboard keeps rendering live meter data.
func TestMergeCompactCarriesQuotaFreebucksTier(t *testing.T) {
	current := compactMergeCurrent()
	next := session.SessionSnapshot{Status: "active", InstanceID: "inst-compact-1", Model: "model-a"}

	merged, keep := mergeCompactSessionSnapshot(current, next)
	if !keep {
		t.Fatal("keepCompact = false for the same active slot, want true")
	}
	if len(merged.QuotaByModel) != 1 || merged.QuotaByModel["model-a"].RecentCount != 3 {
		t.Errorf("QuotaByModel = %v, want the carried admission map", merged.QuotaByModel)
	}
	if merged.Freebucks == nil || merged.Freebucks.Balance != 42 {
		t.Errorf("Freebucks = %+v, want the carried balance 42", merged.Freebucks)
	}
	if merged.SubscriptionTierID != "plan_pro" {
		t.Errorf("SubscriptionTierID = %q, want plan_pro carried", merged.SubscriptionTierID)
	}
}

// Fresh compact values win over the carried ones (vendor `next ?? current`).
func TestMergeCompactFreshValuesWin(t *testing.T) {
	current := compactMergeCurrent()
	next := session.SessionSnapshot{
		Status:             "active",
		InstanceID:         "inst-compact-1",
		Model:              "model-a",
		Freebucks:          &upstream.FreebucksInfo{Balance: 7},
		SubscriptionTierID: "plan_pro_max",
		QuotaByModel: map[string]session.QuotaSnapshot{
			"model-a": {Model: "model-a", Limit: 10, RecentCount: 9},
		},
	}

	merged, keep := mergeCompactSessionSnapshot(current, next)
	if !keep {
		t.Fatal("keepCompact = false for the same active slot, want true")
	}
	if merged.Freebucks.Balance != 7 {
		t.Errorf("Freebucks.Balance = %v, want fresh 7", merged.Freebucks.Balance)
	}
	if merged.SubscriptionTierID != "plan_pro_max" {
		t.Errorf("SubscriptionTierID = %q, want fresh plan_pro_max", merged.SubscriptionTierID)
	}
	if merged.QuotaByModel["model-a"].RecentCount != 9 {
		t.Errorf("RecentCount = %v, want fresh 9", merged.QuotaByModel["model-a"].RecentCount)
	}
}

// A different slot (or a non-active response) must not inherit another
// session's meter: the caller fetches one full response instead.
func TestMergeCompactMismatchDropsCompact(t *testing.T) {
	current := compactMergeCurrent()
	cases := map[string]session.SessionSnapshot{
		"other instance": {Status: "active", InstanceID: "inst-other", Model: "model-a"},
		"other model":    {Status: "active", InstanceID: "inst-compact-1", Model: "model-b"},
		"ended":          {Status: "ended", InstanceID: "inst-compact-1", Model: "model-a"},
		"stale current":  {Status: "queued", InstanceID: "inst-compact-1", Model: "model-a"},
	}
	for name, next := range cases {
		if _, keep := mergeCompactSessionSnapshot(current, next); keep {
			t.Errorf("%s: keepCompact = true, want false (fetch one full response)", name)
		}
	}
	// Mismatched current status also drops, even when next looks live.
	stale := current
	stale.Status = "ended"
	if _, keep := mergeCompactSessionSnapshot(stale, session.SessionSnapshot{Status: "active", InstanceID: "inst-compact-1", Model: "model-a"}); keep {
		t.Error("ended current: keepCompact = true, want false")
	}
}
