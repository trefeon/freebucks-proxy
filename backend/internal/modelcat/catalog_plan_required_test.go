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
// EVERY surface (Gemini 3.8 Flash), which must also never be Served.
// MiMo 2.6 Pro was opened to all full-access accounts on 2026-09-25.
func TestCatalogPlanRequiredEverySurfaceRows(t *testing.T) {
	want := []string{"google/gemini-3.8-flash"}
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
