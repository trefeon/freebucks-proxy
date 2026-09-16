package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestEmittedTokensShapeCoversLiveAndCooldownFields pins the acceptance of
// the openapi-regen lane: the builders gained live_turns/queued_waiters/
// oldest_waiter_ms (#572) and cooldown_kind/cooldown_resets_at/
// cooldown_window_hours (#581), so the emitted spec must carry them on the
// /admin/api/tokens items. Regression guard for the emitter's old habit of
// emitting tokenDetail (unexported embedded structs) as an empty object.
func TestEmittedTokensShapeCoversLiveAndCooldownFields(t *testing.T) {
	out := filepath.Join(t.TempDir(), "openapi.json")
	if err := run("../../internal/dashboard/admin_manifest.json", out, "test"); err != nil {
		t.Fatalf("run: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read emitted spec: %v", err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Items struct {
						Properties map[string]any `json:"properties"`
						Required   []string       `json:"required"`
					} `json:"items"`
				} `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse emitted spec: %v", err)
	}
	items := doc.Components.Schemas["tokensData"].Properties["tokens"].Items
	if len(items.Properties) == 0 {
		t.Fatal("tokensData.tokens.items has no properties: embedded structs not flattened")
	}
	for _, k := range []string{"live_turns", "queued_waiters", "oldest_waiter_ms", "cooldown_kind", "cooldown_resets_at", "cooldown_window_hours"} {
		if _, ok := items.Properties[k]; !ok {
			t.Errorf("tokensData.tokens.items missing %q", k)
		}
	}
	req := map[string]bool{}
	for _, k := range items.Required {
		req[k] = true
	}
	for _, k := range []string{"live_turns", "queued_waiters", "oldest_waiter_ms"} {
		if !req[k] {
			t.Errorf("tokensData.tokens.items should require %q (no omitempty)", k)
		}
	}
	for _, k := range []string{"cooldown_kind", "cooldown_resets_at", "cooldown_window_hours"} {
		if req[k] {
			t.Errorf("tokensData.tokens.items should not require %q (omitempty)", k)
		}
	}
}
