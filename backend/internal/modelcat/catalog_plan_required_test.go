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
// EVERY surface (Gemini 3.8 Flash and MiMo 2.6 Pro), which must also never be
// Served, while GPT-6 Luna is statically plan-locked for a viewer the server
// sent no verdict for yet carries the US exemption, so it is in
// SubscriptionProModelIDs but NOT in this set.
func TestCatalogPlanRequiredEverySurfaceRows(t *testing.T) {
	want := []string{"google/gemini-3.8-flash", "mimo/mimo-v2.6-pro"}
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
	if !IsSubscriptionPro("openai/gpt-6-luna") {
		t.Error("gpt-6-luna absent from SubscriptionProModelIDs, want the US-exempt row in the static fallback list")
	}
	if byID("openai/gpt-6-luna").PlanRequired {
		t.Error("gpt-6-luna PlanRequired, want only the every-surface rows flagged")
	}
}
