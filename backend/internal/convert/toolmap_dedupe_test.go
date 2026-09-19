package convert

import (
	"encoding/json"
	"testing"
)

// TestToolMapperDedupeDuplicateWireNames pins the Hermes+DeepSeek 502 fix:
// terminal and execute_code both map to run_terminal_command, so the wire
// would carry two identically-named tools and strict upstreams reject it
// ("Tool names must be unique"). First occurrence keeps the official name,
// later ones virtualize to mcp__<original>; the wire stays name-unique and
// both names restore to their client originals downstream.
func TestToolMapperDedupeDuplicateWireNames(t *testing.T) {
	mkTool := func(name string, param string) map[string]any {
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": "Tool " + name,
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{param: map[string]any{"type": "string"}},
				},
			},
		}
	}
	payload := map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{
			mkTool("terminal", "command"),
			mkTool("execute_code", "code"),
			mkTool("read_file", "path"),
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	norm, mapper, err := NormalizeRequestMapped(body, "")
	if err != nil {
		t.Fatalf("NormalizeRequestMapped: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(norm, &parsed); err != nil {
		t.Fatalf("unmarshal normalized: %v", err)
	}
	wireTools, ok := parsed["tools"].([]any)
	if !ok {
		t.Fatalf("wire tools not an array: %v", parsed["tools"])
	}
	seen := map[string]bool{}
	for _, wt := range wireTools {
		fn, ok := wt.(map[string]any)["function"].(map[string]any)
		if !ok {
			continue
		}
		name, _ := fn["name"].(string)
		if name == "end_turn" || name == "decide" {
			continue
		}
		if seen[name] {
			t.Fatalf("duplicate wire name %q in %v", name, wireNamesOf(t, wireTools))
		}
		seen[name] = true
	}
	if got := mapper.RestoreName("run_terminal_command"); got != "terminal" {
		t.Errorf("RestoreName(run_terminal_command) = %q, want terminal (first wins)", got)
	}
	if got := mapper.RestoreName("mcp__execute_code"); got != "execute_code" {
		t.Errorf("RestoreName(mcp__execute_code) = %q, want execute_code (virtualized)", got)
	}
}

// wireNamesOf lists wire tool names for failure messages.
func wireNamesOf(t *testing.T, wireTools []any) []string {
	t.Helper()
	var out []string
	for _, wt := range wireTools {
		if fn, ok := wt.(map[string]any)["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); name != "" {
				out = append(out, name)
			}
		}
	}
	return out
}
