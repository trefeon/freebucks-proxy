package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

// Response-leg translation for floor tools OMP cannot dispatch (see
// tools_textfallback.go). The four unroutable floor wire names are suggested
// by the CLI definitions the gate requires on the wire; OMP dispatches only
// its own registry, so their calls must be suppressed and rendered as text
// (or absorbed) instead of relayed as "not found".

func TestTextFallbackClassify(t *testing.T) {
	m := ToolMapper{family: familyOMP}
	cases := []struct {
		name      string
		wire      string
		args      string
		wantKind  TextFallbackKind
		wantParts []string
	}{
		{
			name:     "suggest_followups renders prompts",
			wire:     "suggest_followups",
			args:     `{"followups":[{"prompt":"Add tests for the change","label":"Add tests"},{"prompt":"Ship it"}]}`,
			wantKind: TextFallbackRender,
			wantParts: []string{
				"Suggested next steps",
				"- Add tests for the change",
				"- Ship it",
			},
		},
		{
			name:      "suggest_followups label-only entry still renders",
			wire:      "suggest_followups",
			args:      `{"followups":[{"label":"Only a label"}]}`,
			wantKind:  TextFallbackRender,
			wantParts: []string{"- Only a label"},
		},
		{
			name:      "render_ui plain URL becomes a markdown link",
			wire:      "render_ui",
			args:      `{"widget":{"type":"button","text":"Open preview","link":"https://example.test/p"}}`,
			wantKind:  TextFallbackRender,
			wantParts: []string{"[Open preview](https://example.test/p)"},
		},
		{
			name:      "render_ui gravity_index link reference renders bare label",
			wire:      "render_ui",
			args:      `{"widget":{"type":"button","text":"Get your key","link":{"source":"gravity_index","search_id":"s1","service_slug":"resend"}}}`,
			wantKind:  TextFallbackRender,
			wantParts: []string{"Get your key"},
		},
		{
			name:      "gravity_index states the request it cannot service",
			wire:      "gravity_index",
			args:      `{"action":"search","query":"serverless postgres for Next.js"}`,
			wantKind:  TextFallbackRender,
			wantParts: []string{"Gravity Index", "search", "serverless postgres for Next.js"},
		},
		{
			name:     "report_project_profile is absorbed",
			wire:     "report_project_profile",
			args:     `{"status":"unchanged"}`,
			wantKind: TextFallbackAbsorb,
		},
		{
			name:     "empty render collapses to absorb",
			wire:     "suggest_followups",
			args:     `{"followups":[]}`,
			wantKind: TextFallbackAbsorb,
		},
		{
			name:     "invalid JSON on a renderable name absorbs",
			wire:     "suggest_followups",
			args:     `not json`,
			wantKind: TextFallbackAbsorb,
		},
		{
			name:     "ordinary floor tool is untouched",
			wire:     "code_search",
			args:     `{"pattern":"x"}`,
			wantKind: TextFallbackNone,
		},
		{
			name:     "unknown name is untouched",
			wire:     "some_other_tool",
			args:     `{}`,
			wantKind: TextFallbackNone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, kind := m.TextFallback(tc.wire, tc.args)
			if kind != tc.wantKind {
				t.Fatalf("kind = %v, want %v", kind, tc.wantKind)
			}
			for _, want := range tc.wantParts {
				if !strings.Contains(text, want) {
					t.Errorf("text %q missing %q", text, want)
				}
			}
		})
	}
}

// A non-floor mapper (every non-OMP client) must be completely inert: the
// four names are ordinary CLI tools for those clients and keep riding.
func TestTextFallbackNonFloorMapperInert(t *testing.T) {
	m := ToolMapper{}
	if m.HasTextFallback("suggest_followups") {
		t.Error("HasTextFallback true on a non-floor mapper")
	}
	if _, kind := m.TextFallback("suggest_followups", `{"followups":[{"prompt":"x"}]}`); kind != TextFallbackNone {
		t.Errorf("kind = %v, want TextFallbackNone on a non-floor mapper", kind)
	}
	comp := map[string]any{"choices": []any{map[string]any{
		"message":       map[string]any{"tool_calls": []any{map[string]any{"function": map[string]any{"name": "suggest_followups", "arguments": `{"followups":[{"prompt":"x"}]}`}}}},
		"finish_reason": "tool_calls",
	}}}
	if m.ApplyTextFallbacks(comp) {
		t.Error("ApplyTextFallbacks changed a non-floor completion")
	}
}

func completionWith(calls string, finish string) map[string]any {
	return map[string]any{"choices": []any{map[string]any{
		"message":       map[string]any{"content": "", "tool_calls": mustCalls(calls)},
		"finish_reason": finish,
	}}}
}

func mustCalls(calls string) []any {
	var out []any
	if err := json.Unmarshal([]byte(calls), &out); err != nil {
		panic(err)
	}
	return out
}

// A turn whose only call is an unroutable floor emission ends as a finished
// text turn: the call is gone, the payload is message content, and
// finish_reason reads stop so no client waits on a call it never received.
func TestApplyTextFallbacksRendersAndFlips(t *testing.T) {
	m := ToolMapper{family: familyOMP}
	comp := completionWith(
		`[{"id":"c1","type":"function","function":{"name":"suggest_followups","arguments":"{\"followups\":[{\"prompt\":\"Run the tests\"}]}"}}]`,
		"tool_calls")
	if !m.ApplyTextFallbacks(comp) {
		t.Fatal("ApplyTextFallbacks reported no change")
	}
	choice := comp["choices"].([]any)[0].(map[string]any)
	if _, ok := choice["message"].(map[string]any)["tool_calls"]; ok {
		t.Error("tool_calls survived suppression")
	}
	content, _ := choice["message"].(map[string]any)["content"].(string)
	if !strings.Contains(content, "Run the tests") {
		t.Errorf("content = %q, want the rendered followup", content)
	}
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v, want stop", choice["finish_reason"])
	}
}

// Absorbed calls leave no content and no calls; an unknown-to-fallback call
// in the same message is kept and the turn stays a tool_calls turn.
func TestApplyTextFallbacksKeepsRealCalls(t *testing.T) {
	m := ToolMapper{family: familyOMP}
	comp := completionWith(
		`[{"id":"c1","type":"function","function":{"name":"code_search","arguments":"{\"pattern\":\"x\"}"}},`+
			`{"id":"c2","type":"function","function":{"name":"report_project_profile","arguments":"{\"status\":\"unchanged\"}"}},`+
			`{"id":"c3","type":"function","function":{"name":"render_ui","arguments":"{\"widget\":{\"type\":\"button\",\"text\":\"Open\",\"link\":\"https://example.test\"}}"}}]`,
		"tool_calls")
	if !m.ApplyTextFallbacks(comp) {
		t.Fatal("ApplyTextFallbacks reported no change")
	}
	choice := comp["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	tcs, _ := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls = %d, want 1 (only the ordinary search call)", len(tcs))
	}
	fn := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "code_search" {
		t.Errorf("kept call = %v, want code_search", fn["name"])
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls (a real call remains)", choice["finish_reason"])
	}
	content, _ := msg["content"].(string)
	if !strings.Contains(content, "Open") {
		t.Errorf("content = %q, want the rendered widget", content)
	}
	if !strings.Contains(content, "Gravity") && strings.Contains(content, "report_project") {
		t.Errorf("content = %q, absorbed tool must not render", content)
	}
}

// Text is appended after any content the model already produced, never
// replacing it.
func TestApplyTextFallbacksAppendsContent(t *testing.T) {
	m := ToolMapper{family: familyOMP}
	comp := completionWith(
		`[{"id":"c1","type":"function","function":{"name":"gravity_index","arguments":"{\"action\":\"search\",\"query\":\"kv store\"}"}}]`,
		"tool_calls")
	comp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"] = "Here is the plan."
	m.ApplyTextFallbacks(comp)
	content, _ := comp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string)
	if !strings.HasPrefix(content, "Here is the plan.") {
		t.Errorf("content = %q, want the model text first", content)
	}
	if !strings.Contains(content, "kv store") {
		t.Errorf("content = %q, want the gravity_index note appended", content)
	}
}
