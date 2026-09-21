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

// TestToolMapperWireNameOwnerRoundTrips pins reverse-map OWNERSHIP when one
// client tool's own name IS the official wire name another client tool maps
// onto (Roo-Code declares both write_file — an alias it resolves itself — and
// write_to_file, which maps to write_file). The tool that claimed the wire
// name in ToUpstream's ordered pass must be the one that name restores to:
// a reverse slot pre-registered for the mapped tool must not steal a name the
// native tool actually sent. Either order is legal; both must round-trip.
func TestToolMapperWireNameOwnerRoundTrips(t *testing.T) {
	cases := []struct {
		order   string
		client  []string
		wantOwn map[string]string // wire name -> the client tool that owns it
	}{
		{
			order:   "native name first",
			client:  []string{"write_file", "write_to_file"},
			wantOwn: map[string]string{"write_file": "write_file", "mcp__write_to_file": "write_to_file"},
		},
		{
			order:   "mapped name first",
			client:  []string{"write_to_file", "write_file"},
			wantOwn: map[string]string{"write_file": "write_to_file", "mcp__write_file": "write_file"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.order, func(t *testing.T) {
			tools := make([]any, 0, len(tc.client))
			for _, name := range tc.client {
				tools = append(tools, map[string]any{
					"type": "function",
					"function": map[string]any{
						"name":       name,
						"parameters": map[string]any{"type": "object"},
					},
				})
			}
			body, err := json.Marshal(map[string]any{
				"model":    "m",
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
				"tools":    tools,
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			norm, mapper, err := NormalizeRequestMapped(body, "")
			if err != nil {
				t.Fatalf("NormalizeRequestMapped: %v", err)
			}
			var parsed map[string]any
			if err := json.Unmarshal(norm, &parsed); err != nil {
				t.Fatalf("unmarshal normalized: %v", err)
			}
			wireTools, _ := parsed["tools"].([]any)
			got := map[string]string{}
			for _, item := range wireTools {
				fn, _ := item.(map[string]any)["function"].(map[string]any)
				name, _ := fn["name"].(string)
				if name == "" || name == "end_turn" || name == "decide" {
					continue
				}
				got[name] = mapper.RestoreName(name)
			}
			for wire, owner := range tc.wantOwn {
				if got[wire] != owner {
					t.Errorf("wire %q restores to %q, want %q (client order %v; wire names %v)",
						wire, got[wire], owner, tc.client, wireNamesOf(t, wireTools))
				}
			}
			if len(got) != len(tc.wantOwn) {
				t.Errorf("wire names = %v, want exactly %v", got, tc.wantOwn)
			}
		})
	}
}

// TestToolMapperLegacyFunctionsOwnership covers the legacy `functions` request
// shape: NewToolMapper parses tools[] and sees nothing there, so ToUpstream
// resolves those wire names on the fly. The ordered pass must still hand each
// wire name to the tool that claimed it — no tool may overwrite the reverse
// slot of a name another tool already holds.
func TestToolMapperLegacyFunctionsOwnership(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"functions":[` +
		`{"name":"bash","parameters":{"type":"object"}},` +
		`{"name":"execute","parameters":{"type":"object"}}]}`
	norm, mapper, err := NormalizeRequestMapped([]byte(body), "")
	if err != nil {
		t.Fatalf("NormalizeRequestMapped: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(norm, &parsed); err != nil {
		t.Fatalf("unmarshal normalized: %v", err)
	}
	wireTools, _ := parsed["tools"].([]any)
	got := map[string]string{}
	for _, item := range wireTools {
		fn, _ := item.(map[string]any)["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if name == "" || name == "end_turn" || name == "decide" {
			continue
		}
		got[name] = mapper.RestoreName(name)
	}
	want := map[string]string{"run_terminal_command": "bash", "mcp__execute": "execute"}
	for wire, owner := range want {
		if got[wire] != owner {
			t.Errorf("wire %q restores to %q, want %q (wire names %v)", wire, got[wire], owner, wireNamesOf(t, wireTools))
		}
	}
	if len(got) != len(want) {
		t.Errorf("wire names = %v, want exactly %v", got, want)
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
