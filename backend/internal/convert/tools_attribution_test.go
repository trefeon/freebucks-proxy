package convert

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Served canonical defs carry no commit-attribution footer: the stock
// run_terminal_command description instructs the model to end every commit
// with the vendor's Generated-with/Co-Authored-By trailer. The vendor's own
// suppressed variant is the reference output.
func TestCanonicalDefsStripCommitAttribution(t *testing.T) {
	defs, err := canonicalToolDefs()
	if err != nil {
		t.Fatal(err)
	}
	var rtc map[string]any
	for _, d := range defs {
		m, _ := d.(map[string]any)
		fn, _ := m["function"].(map[string]any)
		if fn["name"] == "run_terminal_command" {
			rtc = fn
		}
		raw, _ := json.Marshal(d)
		for _, marker := range []string{"noreply@codebuff.com", "Generated with Codebuff", "🤖"} {
			if strings.Contains(string(raw), marker) {
				t.Errorf("served def contains %q (name=%v)", marker, fn["name"])
			}
		}
	}
	if rtc == nil {
		t.Fatal("run_terminal_command missing from canonical defs")
	}
	desc := rtc["description"].(string)
	if !strings.Contains(desc, "Do NOT add any trailer") {
		t.Error("stripped description lacks the vendor plain step")
	}
	if !strings.Contains(desc, "**Important details**") {
		t.Error("stripped description lost the tail section")
	}
	if !strings.Contains(desc, `git commit -m \"Your commit message here.\"`) {
		t.Error("stripped description lacks the bare example commit")
	}
	// Surgical precision: the other 15 defs are byte-identical to the fixture.
	raw, err := os.ReadFile(filepath.Join("testdata", "cli-tools.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture []any
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	byName := map[string]map[string]any{}
	for _, d := range fixture {
		m, _ := d.(map[string]any)
		fn, _ := m["function"].(map[string]any)
		byName[fn["name"].(string)] = fn
	}
	for _, d := range defs {
		m, _ := d.(map[string]any)
		fn, _ := m["function"].(map[string]any)
		name := fn["name"].(string)
		if name == "run_terminal_command" {
			continue
		}
		want, _ := json.Marshal(byName[name])
		got, _ := json.Marshal(fn)
		if string(got) != string(want) {
			t.Errorf("def %q changed, want byte-identical to fixture", name)
		}
	}
}

// Unknown shapes pass through untouched: serve stock, never corrupt.
func TestNoAttributionDescriptionPassthrough(t *testing.T) {
	if got := noAttributionDescription("plain text, no anchors"); got != "plain text, no anchors" {
		t.Errorf("passthrough changed input: %q", got)
	}
	defs, err := canonicalToolDefs()
	if err != nil {
		t.Fatal(err)
	}
	var desc string
	for _, d := range defs {
		m, _ := d.(map[string]any)
		fn, _ := m["function"].(map[string]any)
		if fn["name"] == "run_terminal_command" {
			desc = fn["description"].(string)
		}
	}
	if second := noAttributionDescription(desc); second != desc {
		t.Error("strip is not idempotent")
	}
}
