package convert

import "testing"

// Issue #630 bisection record (mock-level, no live keys, no network).
//
// The report: POST /v1/chat/completions with tools[] on
// deepseek/deepseek-v4-flash answers upstream 404 "No endpoints found
// for deepseek/deepseek-v4-flash.", while the same body without tools
// succeeds. These tests pin what OUR wire emits for each shape and how
// the vendor gate (3420c99 foreign-client-signals.ts) reads it, so the
// trigger attribution stays evidence instead of lore:
//
//   - no tools: normalize emits no tools array at all (the injection
//     early-returns on empty) — the gate's tool leg never runs. Clear.
//   - tools=[test_tool]: wire carries test_tool verbatim plus the
//     injected hollow end_turn. The tool leg reads foreign_toolset
//     (enforced) with the injection logged as hollow — but test_tool
//     ALONE already trips foreign_toolset, so the hollow injection is
//     not the differentiator for the gate verdict.
//   - mapped tools with subset schemas (pi powershell -> run_terminal_
//     command{command}) read GENUINE and clear the tool leg: the escape
//     hatch that keeps renamed harness traffic first-party.
//
// What this does NOT claim: the 404's layer. The enforced gate
// downgrades to the tiny model; the observed refusal names the
// requested model in OpenRouter routing phrasing ("No endpoints found
// for ...", same family as the max_price fence's failed_routing_step
// precedent in the registry), which points at the routing/capability
// layer rather than hollow/unrecognised-tool enforcement. That split
// needs a live dump and is recorded here so a live-keys follow-up can
// settle it; the distinct 404 surfacing lives in upstream/classify.go.

func wireToolsOf(t *testing.T, body map[string]any) []any {
	t.Helper()
	out, err := NormalizeRequest(mustJSON(t, body), "")
	if err != nil {
		t.Fatalf("NormalizeRequest: %v", err)
	}
	got := decode(t, out)
	raw, ok := got["tools"]
	if !ok {
		return nil
	}
	tools, ok := raw.([]any)
	if !ok {
		t.Fatalf("tools = %T, want array", raw)
	}
	return tools
}

func toolNamesOf(tools []any) []string {
	var names []string
	for _, tv := range tools {
		tm, ok := tv.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tm["function"].(map[string]any)
		if !ok {
			continue
		}
		if n, _ := fn["name"].(string); n != "" {
			names = append(names, n)
		}
	}
	return names
}

func TestIssue630NoToolsWireIsBare(t *testing.T) {
	tools := wireToolsOf(t, map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "Say hello in one sentence."}},
	})
	if len(tools) != 0 {
		t.Fatalf("no-tools request emitted %d wire tools, want none", len(tools))
	}
	if s := WireForeignSignal(ClassifyWireTools(tools)); s != "" {
		t.Errorf("signal = %q, want clear", s)
	}
}

func TestIssue630TestToolWireVerdict(t *testing.T) {
	tools := wireToolsOf(t, map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "Say hello in one sentence."}},
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "test_tool",
				"description": "A test tool",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			},
		}},
	})
	names := toolNamesOf(tools)
	if len(names) != 2 || names[0] != "test_tool" || names[1] != "end_turn" {
		t.Fatalf("wire tools = %v, want [test_tool end_turn]", names)
	}
	v := ClassifyWireTools(tools)
	if len(v.Genuine) != 0 {
		t.Errorf("genuine = %v, want none", v.Genuine)
	}
	if len(v.Hollow) != 1 || v.Hollow[0] != "end_turn" {
		t.Errorf("hollow = %v, want [end_turn]", v.Hollow)
	}
	if len(v.Unrecognised) != 1 || v.Unrecognised[0] != "test_tool" {
		t.Errorf("unrecognised = %v, want [test_tool]", v.Unrecognised)
	}
	if s := WireForeignSignal(v); s != ForeignToolset {
		t.Errorf("signal = %q, want foreign_toolset", s)
	}
	if !EnforcedSignals[WireForeignSignal(v)] {
		t.Error("foreign_toolset must be an enforced signal")
	}
	// The injection is not the differentiator: test_tool alone, with no
	// injected definition at all, trips the same enforced signal.
	bare := ClassifyWireTools([]any{map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "test_tool",
			"description": "A test tool",
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}})
	if s := WireForeignSignal(bare); s != ForeignToolset {
		t.Errorf("bare test_tool signal = %q, want foreign_toolset", s)
	}
}

func TestIssue630MappedSubsetClearsToolLeg(t *testing.T) {
	// pi powershell renamed to run_terminal_command keeps its {command}
	// schema, which is a non-empty subset of the canonical keys: genuine
	// under the vendor rule, so the tool leg clears despite the hollow
	// injection riding alongside.
	out, _, err := NormalizeRequestMapped(mustJSON(t, map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "dir"}},
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "powershell",
				"description": "run a command",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"command": map[string]any{"type": "string"}},
					"required":   []any{"command"},
				},
			},
		}},
	}), "")
	if err != nil {
		t.Fatalf("NormalizeRequestMapped: %v", err)
	}
	tools, _ := decode(t, out)["tools"].([]any)
	v := ClassifyWireTools(tools)
	if len(v.Genuine) != 1 || v.Genuine[0] != "run_terminal_command" {
		t.Errorf("genuine = %v, want [run_terminal_command]", v.Genuine)
	}
	if s := WireForeignSignal(v); s != "" {
		t.Errorf("signal = %q, want clear", s)
	}
}
