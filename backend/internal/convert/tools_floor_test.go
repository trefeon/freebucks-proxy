package convert

import (
	"encoding/json"
	"os"
	"testing"
)

// RED: an OMP-family toolset (intent-`i` schemas / OMP-signature names) must
// normalize to the floor-only wire: exactly the 16 canonical CLI definitions
// plus the end_turn/decide pins — zero foreign-schema riders (mcp__
// virtualized defs, find_files), because the gate 503s on any foreign
// definition riding alongside the floor (live bisect 2026-09-30: OMP-tools +
// neutral-system 503, official-tools + OMP-system 200).
func TestFloorOnlyWireForOMPToolset(t *testing.T) {
	raw, err := os.ReadFile("D:/tmp/ompcap/001-req.json")
	if err != nil {
		t.Skip("no capture")
	}
	var cap struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(raw, &cap); err != nil {
		t.Fatal(err)
	}
	wire, _, err := NormalizeRequestMappedOpts([]byte(cap.Body), "", DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	if err := json.Unmarshal(wire, &p); err != nil {
		t.Fatal(err)
	}
	tools, _ := p["tools"].([]any)
	names := []string{}
	for _, x := range tools {
		m, _ := x.(map[string]any)
		fn, _ := m["function"].(map[string]any)
		n, _ := fn["name"].(string)
		names = append(names, n)
	}
	t.Logf("wire tools (%d): %v", len(names), names)
	wantFloor := map[string]bool{}
	for _, d := range mustCanonical(t) {
		m, _ := d.(map[string]any)
		fn, _ := m["function"].(map[string]any)
		n, _ := fn["name"].(string)
		wantFloor[n] = true
	}
	for _, n := range names {
		if n == "end_turn" || n == "decide" {
			continue
		}
		if !wantFloor[n] {
			t.Errorf("wire carries non-floor tool %q (foreign rider trips the gate)", n)
		}
	}
	if len(names) != len(wantFloor)+2 {
		t.Errorf("wire tools = %d, want %d floor + end_turn + decide", len(names), len(wantFloor))
	}
}

func mustCanonical(t *testing.T, _ ...any) []any {
	t.Helper()
	defs, err := canonicalToolDefs()
	if err != nil {
		t.Fatal(err)
	}
	return defs
}
