package convert

import (
	"encoding/json"
	"os"
	"strings"
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

// advisorBody renders an ADVISOR-role request (omp 18.4.3): plain schemas
// with no injected intent-`i` (intentTracing:false).
func advisorBody(names ...string) string {
	var b strings.Builder
	b.WriteString(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[`)
	for i, n := range names {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"type":"function","function":{"name":"` + n + `","parameters":{"type":"object","properties":{"input":{"type":"string"}}}}}`)
	}
	b.WriteString("]}")
	return b.String()
}

// ADVISOR role (omp 18.4.3): declares advise+read+grep+glob (+recall/custom
// variants) with intentTracing:false, so ZERO OMP family signals reach the
// proxy except the `advise` name itself — no intent-`i`, 0 of the 2-of-4
// signature names. Detection keys on `advise` alone; the 2-of-4 threshold
// for eval/learn/manage_skill/context_notes is unchanged.
func TestAdvisorFamilyDetection(t *testing.T) {
	cases := []struct {
		name  string
		tools []string
		want  clientFamily
	}{
		{"advisor role set", []string{"advise", "read", "grep", "glob"}, familyOMP},
		{"advisor with recall", []string{"advise", "read", "grep", "glob", "recall"}, familyOMP},
		{"eval+learn advisor variant", []string{"advise", "eval", "learn"}, familyOMP},
		{"advise alone forces OMP", []string{"advise"}, familyOMP},
		{"read+grep without advise reads pi", []string{"read", "grep"}, familyPi},
		{"empty tools unchanged", nil, familyNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			norm, mapper, err := NormalizeRequestMappedOpts([]byte(advisorBody(tc.tools...)), "", DefaultOptions())
			if err != nil {
				t.Fatalf("NormalizeRequestMappedOpts: %v", err)
			}
			if mapper.family != tc.want {
				t.Fatalf("family = %v, want %v", mapper.family, tc.want)
			}
			if tc.want != familyOMP {
				return
			}
			if !mapper.FloorOnly() {
				t.Error("advisor family did not go floor-only (advise must force OMP)")
			}
			// Floor-only wire: exactly the 16 canonical CLI definitions
			// plus the injected end_turn/decide pins (upstream topUp drops
			// decide, so the live gate sees 16+end_turn) — the foreign
			// `advise` definition must not ride.
			var p map[string]any
			if err := json.Unmarshal(norm, &p); err != nil {
				t.Fatalf("wire not JSON: %v", err)
			}
			tools, _ := p["tools"].([]any)
			if len(tools) != 18 {
				t.Errorf("wire tools = %d, want 18 (16 floor + end_turn + decide)", len(tools))
			}
			for _, x := range tools {
				m, _ := x.(map[string]any)
				fn, _ := m["function"].(map[string]any)
				if n, _ := fn["name"].(string); n == "advise" {
					t.Error("wire carries foreign rider advise (trips the gate)")
				}
			}
		})
	}
}

// TestFloorAdvisorShape pins the ADVISOR-role response leg: the model calls
// `advise` from prompt vocabulary (never a wire definition) alongside floor
// calls, and both must reach the client — advise byte-verbatim (no reshape
// rule, no text fallback), floor calls reshaped+restored, the turn staying
// a tool_calls turn.
func TestFloorAdvisorShape(t *testing.T) {
	_, mapper, err := NormalizeRequestMappedOpts([]byte(advisorBody("advise", "read", "grep", "glob")), "", DefaultOptions())
	if err != nil {
		t.Fatalf("NormalizeRequestMappedOpts: %v", err)
	}
	if mapper.family != familyOMP {
		t.Fatalf("family = %v, want familyOMP", mapper.family)
	}
	const adviseArgs = `{"note":"extract this helper","severity":"concern"}`
	comp := completionWith(
		`[{"id":"c1","type":"function","function":{"name":"advise","arguments":"{\"note\":\"extract this helper\",\"severity\":\"concern\"}"}},`+
			`{"id":"c2","type":"function","function":{"name":"read_files","arguments":"{\"paths\":[\"a.go\",\"b.go\"]}"}}]`,
		"tool_calls")
	// Non-streaming relay order: reshape (wire-keyed) before the name
	// restore, text fallbacks last.
	if !mapper.ReshapeCompletionCalls(comp) {
		t.Fatal("ReshapeCompletionCalls reported no change (read_files must reshape)")
	}
	mapper.FromUpstreamChunk(comp)
	if mapper.ApplyTextFallbacks(comp) {
		t.Fatal("ApplyTextFallbacks changed an advisor turn (advise/read are dispatchable)")
	}
	choice := comp["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls (dispatchable calls remain)", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	tcs, _ := msg["tool_calls"].([]any)
	if len(tcs) != 3 {
		t.Fatalf("tool_calls = %d, want 3 (advise + 2 fanned-out reads)", len(tcs))
	}
	adviseFn := tcs[0].(map[string]any)["function"].(map[string]any)
	if adviseFn["name"] != "advise" {
		t.Errorf("call 0 name = %v, want advise", adviseFn["name"])
	}
	if adviseFn["arguments"] != adviseArgs {
		t.Errorf("advise args = %v, want verbatim %v", adviseFn["arguments"], adviseArgs)
	}
	for i, want := range []string{`{"path":"a.go"}`, `{"path":"b.go"}`} {
		fn := tcs[i+1].(map[string]any)["function"].(map[string]any)
		if fn["name"] != "read" {
			t.Errorf("call %d name = %v, want read (restored from read_files)", i+1, fn["name"])
		}
		if fn["arguments"] != want {
			t.Errorf("call %d args = %v, want %v", i+1, fn["arguments"], want)
		}
	}
	// Streaming gate: advise has no reshape rule and no fallback, so the
	// chunk pipeline (server/openai_chunk_pipeline.go) withholds nothing
	// and flushes its fragments verbatim — it can never text-fallback.
	if mapper.HasReshapeRule("advise") {
		t.Error("HasReshapeRule(advise) = true, want false (streaming must flow verbatim)")
	}
	if _, kind := mapper.TextFallback("advise", `{"note":"x"}`); kind != TextFallbackNone {
		t.Errorf("TextFallback(advise) kind = %v, want TextFallbackNone", kind)
	}
}
