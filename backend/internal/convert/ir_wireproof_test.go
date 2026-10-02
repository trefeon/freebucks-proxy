package convert

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// Wire-identical proof for the IR remake (Phase 1.1/1.3/1.4 acceptance):
//
//  1. Corpus agreement — every fixture harness through NormalizeRequestMappedIR
//     emits zero ir.divergence diagnostics (the IR Own decisions and the JSON
//     appliers agree on every wire name, both directions).
//  2. Golden wire bytes — representative requests pin exact wire output in
//     testdata/ir_wire_golden.json (regenerate with -update-ir after eyeballing
//     the diff; the committed bytes are the contract).
//  3. Render parity — RenderIRCalls over []IRCall matches the legacy
//     ApplyTextFallbacks → ReshapeCompletionCalls → FromUpstreamChunk relay
//     order field-for-field on a mixed fan-out/fallback completion.

var updateIRGolden = flag.Bool("update-ir", false, "regenerate testdata/ir_wire_golden.json")

const irGoldenPath = "testdata/ir_wire_golden.json"

type irGoldenCase struct {
	Name    string          `json:"name"`
	Request json.RawMessage `json:"request"`
	Wire    json.RawMessage `json:"wire"`
}

func irGoldenRequests() []irGoldenCase {
	ompTools := []any{
		map[string]any{"type": "function", "function": map[string]any{"name": "advise", "parameters": map[string]any{"type": "object"}}},
		map[string]any{"type": "function", "function": map[string]any{"name": "read", "parameters": map[string]any{"type": "object"}}},
		map[string]any{"type": "function", "function": map[string]any{"name": "bash", "parameters": map[string]any{"type": "object"}}},
		map[string]any{"type": "function", "function": map[string]any{"name": "custom_rider", "parameters": map[string]any{"type": "object"}}},
	}
	piTools := []any{
		map[string]any{"type": "function", "function": map[string]any{"name": "read", "parameters": map[string]any{"type": "object"}}},
		map[string]any{"type": "function", "function": map[string]any{"name": "bash", "parameters": map[string]any{"type": "object"}}},
		map[string]any{"type": "function", "function": map[string]any{"name": "edit", "parameters": map[string]any{"type": "object"}}},
		map[string]any{"type": "function", "function": map[string]any{"name": "write", "parameters": map[string]any{"type": "object"}}},
	}
	customTools := []any{
		map[string]any{"type": "function", "function": map[string]any{"name": "test_tool", "parameters": map[string]any{"type": "object"}}},
		map[string]any{"type": "function", "function": map[string]any{"name": "bash", "parameters": map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}}}},
	}
	mk := func(name string, tools []any, extra map[string]any) irGoldenCase {
		body := map[string]any{
			"model":    "luna/luna-pro",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			"tools":    tools,
		}
		for k, v := range extra {
			body[k] = v
		}
		raw, err := json.Marshal(body)
		if err != nil {
			panic(err)
		}
		return irGoldenCase{Name: name, Request: raw}
	}
	return []irGoldenCase{
		mk("omp_floor", ompTools, nil),
		mk("pi_core", piTools, nil),
		mk("custom_mixed", customTools, nil),
		mk("pinned_choice", customTools, map[string]any{
			"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "bash"}},
		}),
		mk("no_tools", nil, nil),
	}
}

// TestIRCorpusAgreement runs every corpus harness through the IR pipeline and
// rejects any ir.divergence diagnostic: the IR Own pass and the JSON appliers
// must name every wire identically. (Wire bytes vs the contract are pinned
// by TestCorpusToolNameMapping; this test pins IR↔applier agreement.)
func TestIRCorpusAgreement(t *testing.T) {
	fix := loadCorpusFixture(t)
	for _, h := range fix.Harnesses {
		tools := make([]any, 0, len(h.Tools))
		for _, ct := range h.Tools {
			tools = append(tools, corpusClientTool(ct.Name))
		}
		body, err := json.Marshal(map[string]any{
			"model":    "deepseek/deepseek-v4-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			"tools":    tools,
		})
		if err != nil {
			t.Fatalf("harness %s: marshal: %v", h.Harness, err)
		}
		_, _, ir, err := NormalizeRequestMappedIR(body, "", DefaultOptions())
		if err != nil {
			t.Fatalf("harness %s: %v", h.Harness, err)
		}
		for _, d := range ir.Diagnostics {
			if d.Code == DiagDivergence {
				t.Errorf("harness %s: divergence %s → %s: %s", h.Harness, d.From, d.To, d.Message)
			}
		}
	}
}

// TestIRGoldenWire pins exact wire bytes per representative request.
func TestIRGoldenWire(t *testing.T) {
	cases := irGoldenRequests()
	if *updateIRGolden {
		out := make([]irGoldenCase, 0, len(cases))
		for _, c := range cases {
			wire, _, _, err := NormalizeRequestMappedIR(c.Request, "", DefaultOptions())
			if err != nil {
				t.Fatalf("case %s: %v", c.Name, err)
			}
			c.Wire = wire
			out = append(out, c)
		}
		raw, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(irGoldenDir(), irGoldenPath), append(raw, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("regenerated %s", irGoldenPath)
		return
	}
	raw, err := os.ReadFile(filepath.Join(irGoldenDir(), irGoldenPath))
	if err != nil {
		t.Fatalf("read golden: %v (run with -update-ir to generate)", err)
	}
	var golden []irGoldenCase
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("parse golden: %v", err)
	}
	want := make(map[string]string, len(golden))
	for _, c := range golden {
		want[c.Name] = string(c.Wire)
	}
	for _, c := range cases {
		wire, _, ir, err := NormalizeRequestMappedIR(c.Request, "", DefaultOptions())
		if err != nil {
			t.Fatalf("case %s: %v", c.Name, err)
		}
		stored, ok := want[c.Name]
		if !ok {
			t.Errorf("case %s missing from golden file", c.Name)
			continue
		}
		// The golden file is MarshalIndent-pinned (indentation is not part
		// of the contract): compact the stored form before comparing bytes.
		var canon any
		if err := json.Unmarshal([]byte(stored), &canon); err != nil {
			t.Fatalf("case %s: golden wire is not JSON: %v", c.Name, err)
		}
		compact, err := json.Marshal(canon)
		if err != nil {
			t.Fatal(err)
		}
		if string(wire) != string(compact) {
			t.Errorf("case %s: wire bytes drifted\n got: %.300s...\nwant: %.300s...", c.Name, wire, compact)
		}
		for _, d := range ir.Diagnostics {
			if d.Code == DiagDivergence {
				t.Errorf("case %s: divergence %s → %s", c.Name, d.From, d.To)
			}
		}
	}
}

func irGoldenDir() string { return "." }

// TestIRFloorGoldenShape pins the floor-only contract at the IR level: the
// rider is dropped with a diagnostic, floor defs carry KindFloor, and the
// wire is exactly the 16 canonical defs plus pins.
func TestIRFloorGoldenShape(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[`
	body += irFuncTool("advise") + `,` + irFuncTool("read") + `,` + irFuncTool("custom_rider") + `]}`
	wire, _, ir, err := NormalizeRequestMappedIR([]byte(body), "", DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if ir.FamilyName() != "omp" {
		t.Fatalf("family = %q, want omp", ir.FamilyName())
	}
	var dropped bool
	for _, d := range ir.Set.Defs {
		if d.ClientName == "custom_rider" && d.Dropped {
			dropped = true
		}
	}
	if !dropped {
		t.Error("custom_rider not marked dropped")
	}
	var floors int
	for _, d := range ir.Set.Defs {
		if d.Kind == KindFloor && !d.Dropped {
			floors++
		}
	}
	if floors != 16 {
		t.Errorf("floor defs = %d, want 16", floors)
	}
	var parsed map[string]any
	if err := json.Unmarshal(wire, &parsed); err != nil {
		t.Fatal(err)
	}
	tools, _ := parsed["tools"].([]any)
	if len(tools) != 18 {
		t.Errorf("wire tools = %d, want 18 (16 floor + end_turn + decide)", len(tools))
	}
	names := payloadWireNames(parsed)
	sort.Strings(names)
	// Every floor name is a canonical CLI def (gate requirement) or a pin.
	allowed := map[string]bool{"end_turn": true, "decide": true}
	if defs, err := canonicalToolDefs(); err == nil {
		for _, d := range defs {
			m, _ := d.(map[string]any)
			fn, _ := m["function"].(map[string]any)
			if n, _ := fn["name"].(string); n != "" {
				allowed[n] = true
			}
		}
	}
	for _, n := range names {
		if !allowed[n] {
			t.Errorf("wire carries non-floor rider %q", n)
		}
	}
}

// TestRenderParityLegacyRelay proves RenderIRCalls matches the relay order
// ApplyTextFallbacks → ReshapeCompletionCalls → FromUpstreamChunk on a mixed
// completion: multi-path fan-out, multi-replacement fan-out, reshaped 1:1,
// rendered fallback, and absorbed fallback.
func TestRenderParityLegacyRelay(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[`
	body += irFuncTool("advise") + `,` + irFuncTool("read") + `,` + irFuncTool("bash") + `,` +
		irFuncTool("edit") + `,` + irFuncTool("write") + `,` + irFuncTool("grep") + `,` + irFuncTool("todo") + `]}`
	_, mapper, _, err := NormalizeRequestMappedIR([]byte(body), "", DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	toolCall := func(id, name, args string) map[string]any {
		return map[string]any{
			"id": id, "type": "function",
			"function": map[string]any{"name": name, "arguments": args},
		}
	}
	tcs := []any{
		toolCall("c1", "read_files", `{"paths":["a.go","b.go"]}`),
		toolCall("c2", "str_replace", `{"path":"f.go","replacements":[{"oldString":"x","newString":"y"},{"oldString":"p","newString":"q"}]}`),
		toolCall("c3", "write_file", `{"path":"n.go","content":"hi","instructions":"drop me"}`),
		toolCall("c4", "suggest_followups", `{"suggestions":["try this"]}`),
		toolCall("c5", "report_project_profile", `{"x":1}`),
	}
	legacy := map[string]any{
		"choices": []any{map[string]any{
			"finish_reason": "tool_calls",
			"message": map[string]any{
				"content":    "",
				"tool_calls": append([]any{}, tcs...),
			},
		}},
	}
	// Legacy relay order: fallbacks (wire names) → reshape (wire names) →
	// restore. Deep-copy tool_calls per stage is unnecessary: stages mutate
	// in the documented order on one object, exactly like the relays.
	mapper.ApplyTextFallbacks(legacy)
	mapper.ReshapeCompletionCalls(legacy)
	mapper.FromUpstreamChunk(legacy)

	// IR path: assemble the same wire calls and render through the threaded
	// mapper. The legacy stages above mutate the shared maps in place
	// (reshape rewrites arguments, restore rewrites names), so the IR path
	// rebuilds pristine wire calls from the original fields instead of
	// re-reading the mutated maps.
	wireCalls := []IRCall{
		{WireName: "read_files", Args: `{"paths":["a.go","b.go"]}`, ID: "c1"},
		{WireName: "str_replace", Args: `{"path":"f.go","replacements":[{"oldString":"x","newString":"y"},{"oldString":"p","newString":"q"}]}`, ID: "c2"},
		{WireName: "write_file", Args: `{"path":"n.go","content":"hi","instructions":"drop me"}`, ID: "c3"},
		{WireName: "suggest_followups", Args: `{"suggestions":["try this"]}`, ID: "c4"},
		{WireName: "report_project_profile", Args: `{"x":1}`, ID: "c5"},
	}
	rendered := RenderIRCalls(mapper, wireCalls)

	// Compare: surviving legacy calls vs non-suppressed rendered calls.
	legacyMsg := legacy["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	var legacyCalls []any
	if kept, ok := legacyMsg["tool_calls"].([]any); ok {
		legacyCalls = kept
	}
	var kept []RenderedCall
	var texts []string
	for _, rc := range rendered {
		if rc.Suppressed {
			texts = append(texts, rc.Text)
			continue
		}
		kept = append(kept, rc)
	}
	if len(kept) != len(legacyCalls) {
		t.Fatalf("kept = %d, legacy surviving = %d", len(kept), len(legacyCalls))
	}
	for i, raw := range legacyCalls {
		tc := raw.(map[string]any)
		fn := tc["function"].(map[string]any)
		if kept[i].ClientName != fn["name"].(string) {
			t.Errorf("call %d: client = %q, legacy = %q", i, kept[i].ClientName, fn["name"])
		}
		if kept[i].Args != fn["arguments"].(string) {
			t.Errorf("call %d: args = %s, legacy = %s", i, kept[i].Args, fn["arguments"])
		}
		if kept[i].ID != tc["id"].(string) {
			t.Errorf("call %d: id = %q, legacy = %q", i, kept[i].ID, tc["id"])
		}
	}
	// Fan-out shape: c1 → 2 reads, c2 → 2 edits.
	var c1, c2 int
	for _, rc := range kept {
		switch rc.ID {
		case "c1", "c1-fanout-1":
			c1++
		case "c2", "c2-fanout-1":
			c2++
		}
	}
	if c1 != 2 || c2 != 2 {
		t.Errorf("fan-out ids: c1×%d c2×%d, want 2/2", c1, c2)
	}
	// Rendered text: legacy appends fallback renders to content verbatim.
	legacyContent, _ := legacyMsg["content"].(string)
	var wantText string
	for _, s := range texts {
		wantText += s
	}
	if legacyContent != wantText {
		t.Errorf("content = %q, rendered texts = %q", legacyContent, wantText)
	}
	// finish_reason flips when nothing remains; here calls remain.
	if fr := legacy["choices"].([]any)[0].(map[string]any)["finish_reason"]; fr != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls (calls remain)", fr)
	}
}
