package registry

import (
	"freebuff-proxy/backend/internal/modelcat"
	"strings"
	"testing"
)

// TestServedModelsRouteToBase3FreeRoots pins the live-capture pattern
// (docs/LIVE-CAPTURE.md): every model the CLI picker can serve STARTs on
// base3-free-<model-slug> — the per-model root, never a generic or base2 id.
// Both capture turns are covered explicitly (base3-free-deepseek-flash,
// base3-free-glm-5-3-flash); the table covers the full served set so a newly
// served model without a base3 root fails here instead of burning a doomed
// admission upstream (free_mode_invalid_agent_model).
func TestServedModelsRouteToBase3FreeRoots(t *testing.T) {
	servedRoots := map[string]string{
		"deepseek/deepseek-v4-flash": "base3-free-deepseek-flash",
		"z-ai/glm-5.3-flash":         "base3-free-glm-5-3-flash",
		"mimo/mimo-v2.5":             "base3-free-mimo",
		"mimo/mimo-v2.6-pro":         "base3-free-mimo-2-6-pro",
		"openai/gpt-6-luna":          "base3-free-luna-6",
		"upstage/solar-pro4":         "base3-free-solar-pro4",
		"upstage/solar-mini4":        "base3-free-solar-mini4",
		"stealth/space-bunny-alpha":  "base3-free-space-bunny-alpha",
	}

	// The table must track the served set exactly: a served model missing
	// from the table (or a table row for a model no longer served) is a
	// coverage gap, not a passing test.
	if len(servedRoots) != len(modelcat.ServedMap()) {
		t.Fatalf("served-roots table has %d rows, served set has %d — update the table with the served change",
			len(servedRoots), len(modelcat.ServedMap()))
	}
	for m := range modelcat.ServedMap() {
		if _, ok := servedRoots[m]; !ok {
			t.Errorf("served model %q has no pinned base3 root — add its base3-free-<slug> row", m)
		}
	}

	r := New(nil, nil)
	r.LoadFallback()
	for model, wantAgent := range servedRoots {
		agent, err := r.AgentForModel(model)
		if err != nil {
			t.Errorf("AgentForModel(%q): %v", model, err)
			continue
		}
		if agent != wantAgent {
			t.Errorf("AgentForModel(%q) = %q, want %q", model, agent, wantAgent)
		}
		if !strings.HasPrefix(agent, "base3-free-") {
			t.Errorf("AgentForModel(%q) = %q, want base3-free-<slug> (capture pattern)", model, agent)
		}
	}
}
