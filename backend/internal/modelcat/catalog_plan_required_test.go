package modelcat

import (
	"slices"
	"testing"
)

// TestCatalogPlanRequiredEverySurfaceRows pins the generated every-surface
// plan-lock flag per row. It is no longer read by the dashboard's models
// table — the display decision now comes from the server's per-viewer verdict
// with IsSubscriptionPro as the fallback — so this test is what keeps the
// generated fact honest: PlanRequired marks the rows upstream refuses on
// EVERY surface (Gemini 3.8 Flash, both Muse Spark rows, GPT-6.1 Sol), which
// must also never be Served.
// MiMo 2.6 Pro was opened to all full-access accounts on 2026-09-25.
// Muse Spark 1.3 returned from PAUSED as a plan-gated row on 2026-09-28,
// 1.2 joined it behind the same paywall, and GPT-6.1 Sol arrived paywalled
// (US-or-paid) on 2026-09-29 — all vendor a2fd480.
func TestCatalogPlanRequiredEverySurfaceRows(t *testing.T) {
	want := []string{"openai/gpt-6.1-sol", "google/gemini-3.8-flash", "meta/muse-spark-1.3-contributor", "meta/muse-spark-1.2-contributor"}
	got := make([]string, 0, len(want))
	for _, info := range Catalog {
		if !info.PlanRequired {
			continue
		}
		if info.Served {
			t.Errorf("%s is PlanRequired and Served, want every-surface rows unserved", info.ID)
		}
		got = append(got, info.ID)
	}
	if !slices.Equal(got, want) {
		t.Errorf("PlanRequired rows = %v, want %v", got, want)
	}
	if byID("openai/gpt-6-luna").PlanRequired {
		t.Error("gpt-6-luna PlanRequired, want only the every-surface rows flagged")
	}
}
