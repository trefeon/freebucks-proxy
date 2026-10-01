package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// Served default defs carry no commit-attribution footer (mirror of
// convert's canonical-defs pin; the helper is duplicated per the archtest
// matrix — keep the two in sync).
func TestDefaultToolsStripCommitAttribution(t *testing.T) {
	tools := defaultCliTools()
	var desc string
	for _, d := range tools {
		m, _ := d.(map[string]any)
		fn, _ := m["function"].(map[string]any)
		raw, _ := json.Marshal(d)
		for _, marker := range []string{"noreply@codebuff.com", "Generated with Codebuff", "🤖"} {
			if strings.Contains(string(raw), marker) {
				t.Errorf("served def contains %q (name=%v)", marker, fn["name"])
			}
		}
		if fn["name"] == "run_terminal_command" {
			desc = fn["description"].(string)
		}
	}
	if !strings.Contains(desc, "Do NOT add any trailer") {
		t.Error("stripped description lacks the vendor plain step")
	}
	if second := noAttributionDescription(desc); second != desc {
		t.Error("strip is not idempotent")
	}
}
