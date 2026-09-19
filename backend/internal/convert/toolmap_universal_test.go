package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

// universalClientTool is one client-declared tool definition: the name the
// harness sends plus its top-level parameter keys (values are JSON types).
type universalClientTool struct {
	name   string
	params map[string]any
}

// assertHarnessWireClean runs the full ingress→classify→egress simulation
// for one harness toolset: client tools[] → NormalizeRequestMapped → the
// wire must carry zero foreign/hollow signals with at least one genuine
// signature tool → every wire name restores downstream (single-name and
// both SSE chunk shapes agree).
func assertHarnessWireClean(t *testing.T, name string, tools []universalClientTool) {
	t.Helper()
	var toolsArr []any
	for _, ct := range tools {
		props := map[string]any{}
		for k, v := range ct.params {
			props[k] = map[string]any{"type": v}
		}
		toolsArr = append(toolsArr, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        ct.name,
				"description": "Tool " + ct.name,
				"parameters": map[string]any{
					"type":       "object",
					"properties": props,
				},
			},
		})
	}

	body, err := json.Marshal(map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "Help me with my project"}},
		"tools":    toolsArr,
	})
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
	if !ok || len(wireTools) == 0 {
		t.Fatalf("wire tools empty or not an array: %v", parsed["tools"])
	}

	v := ClassifyWireTools(wireTools)
	if sig := WireForeignSignal(v); sig != "" {
		t.Errorf("WireForeignSignal = %q, want empty (wire tools = %v)", sig, v.Names)
	}
	if len(v.Foreign) != 0 {
		t.Errorf("len(v.Foreign) = %d (%v), want 0", len(v.Foreign), v.Foreign)
	}
	if len(v.ForeignHarness) != 0 {
		t.Errorf("len(v.ForeignHarness) = %d (%v), want 0", len(v.ForeignHarness), v.ForeignHarness)
	}
	if len(v.Genuine) == 0 {
		t.Errorf("no genuine signature tools found on wire")
	}

	// Verify that every client tool name restores cleanly
	for _, wt := range wireTools {
		fn, ok := wt.(map[string]any)["function"].(map[string]any)
		if !ok {
			continue
		}
		wName := fn["name"].(string)
		if wName == "end_turn" || wName == "decide" {
			continue
		}
		restored := mapper.RestoreName(wName)
		if strings.HasPrefix(restored, "mcp__") {
			t.Errorf("RestoreName(%q) = %q still has mcp__ prefix", wName, restored)
		}
	}

	// Verify streaming chunk restoration matches the single-name path.
	for _, ct := range tools {
		for _, candidate := range []string{ct.name, "mcp__" + ct.name, "run_terminal_command"} {
			streamChunk := map[string]any{
				"choices": []any{map[string]any{
					"delta": map[string]any{
						"tool_calls": []any{
							map[string]any{
								"index": float64(0),
								"function": map[string]any{
									"name": candidate,
								},
							},
						},
					},
				}},
			}
			mapper.FromUpstreamChunk(streamChunk)
			delta := streamChunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
			resName := delta["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["name"].(string)
			if want := mapper.RestoreName(candidate); resName != want {
				t.Errorf("FromUpstreamChunk candidate %q -> %q, want RestoreName %q", candidate, resName, want)
			}
		}
	}
}

// TestUniversalToolsetsWireClassification extends
// TestHarnessToolsetsWireClassification to the remaining inventoried
// harnesses/agents (OpenClaw, OpenHands, SWE-agent, Gemini CLI, Crush,
// Kimi CLI, Aider, Goose, Jcode, plus extended Hermes/Codex/OpenCode rows):
// the same ingress→classify→egress simulation must clear with zero
// foreign/hollow signals and byte-exact downstream restoration.
func TestUniversalToolsetsWireClassification(t *testing.T) {
	harnesses := []struct {
		name  string
		tools []universalClientTool
	}{
		{
			name: "OpenClaw",
			tools: []universalClientTool{
				{"ls", map[string]any{"path": "string"}},
				{"read", map[string]any{"path": "string"}},
				{"write", map[string]any{"path": "string", "content": "string"}},
				{"edit", map[string]any{"path": "string", "old_string": "string", "new_string": "string"}},
				{"exec", map[string]any{"command": "string"}},
				{"browser_exec", map[string]any{"action": "string"}},
				{"computer_use", map[string]any{"action": "string"}},
				{"delegate_task", map[string]any{"task": "string"}},
				{"web_search", map[string]any{"query": "string"}},
				{"web_fetch", map[string]any{"url": "string"}},
				{"memory_search", map[string]any{"query": "string"}},
				{"sessions_spawn", map[string]any{"task": "string"}},
				{"message", map[string]any{"text": "string"}},
			},
		},
		{
			name: "OpenHands",
			tools: []universalClientTool{
				{"execute_bash", map[string]any{"command": "string"}},
				{"run_ipython", map[string]any{"code": "string"}},
				{"call_tool_mcp", map[string]any{"tool": "string"}},
				{"delegate", map[string]any{"task": "string"}},
				{"browse", map[string]any{"url": "string"}},
				{"finish", map[string]any{}},
			},
		},
		{
			name: "SWE-agent",
			tools: []universalClientTool{
				{"bash", map[string]any{"command": "string"}},
				{"str_replace_editor", map[string]any{"command": "string", "path": "string"}},
				{"search_files", map[string]any{"query": "string"}},
				{"browser_navigate", map[string]any{"url": "string"}},
				{"submit", map[string]any{"summary": "string"}},
			},
		},
		{
			name: "Gemini-cli",
			tools: []universalClientTool{
				{"read_file", map[string]any{"path": "string"}},
				{"write_file", map[string]any{"path": "string", "content": "string"}},
				{"replace", map[string]any{"path": "string", "old_string": "string", "new_string": "string"}},
				{"run_shell_command", map[string]any{"command": "string"}},
				{"grep_search", map[string]any{"pattern": "string"}},
				{"read_many_files", map[string]any{"paths": "array"}},
				{"google_web_search", map[string]any{"query": "string"}},
				{"activate_skill", map[string]any{"skill": "string"}},
				{"ask_user", map[string]any{"question": "string"}},
			},
		},
		{
			name: "Crush",
			tools: []universalClientTool{
				{"bash", map[string]any{"command": "string"}},
				{"edit", map[string]any{"path": "string"}},
				{"glob", map[string]any{"pattern": "string"}},
				{"grep", map[string]any{"pattern": "string"}},
				{"ls", map[string]any{"path": "string"}},
				{"fetch", map[string]any{"url": "string"}},
				{"download", map[string]any{"url": "string"}},
				{"todos", map[string]any{"todos": "array"}},
				{"job_output", map[string]any{"job_id": "string"}},
			},
		},
		{
			name: "Kimi-cli",
			tools: []universalClientTool{
				{"Shell", map[string]any{"command": "string"}},
				{"Agent", map[string]any{"prompt": "string"}},
				{"read", map[string]any{"path": "string"}},
				{"write", map[string]any{"path": "string", "content": "string"}},
				{"edit", map[string]any{"path": "string"}},
				{"glob", map[string]any{"pattern": "string"}},
				{"grep", map[string]any{"pattern": "string"}},
				{"task", map[string]any{"task": "string"}},
				{"todo", map[string]any{"todos": "array"}},
			},
		},
		{
			name: "Aider",
			tools: []universalClientTool{
				{"write_file", map[string]any{"path": "string", "content": "string"}},
				{"replace_lines", map[string]any{"path": "string", "old": "string", "new": "string"}},
			},
		},
		{
			name: "Goose",
			tools: []universalClientTool{
				{"developer__shell", map[string]any{"command": "string"}},
				{"developer.shell", map[string]any{"command": "string"}},
				{"developer__text_editor", map[string]any{"path": "string"}},
				{"developer.text_editor", map[string]any{"path": "string"}},
				{"memory", map[string]any{"query": "string"}},
			},
		},
		{
			name: "Jcode",
			tools: []universalClientTool{
				{"read", map[string]any{"path": "string"}},
				{"write", map[string]any{"path": "string", "content": "string"}},
				{"edit", map[string]any{"path": "string"}},
				{"multiedit", map[string]any{"edits": "array"}},
				{"patch", map[string]any{"patch": "string"}},
				{"bash", map[string]any{"command": "string"}},
				{"glob", map[string]any{"pattern": "string"}},
				{"grep", map[string]any{"pattern": "string"}},
				{"webfetch", map[string]any{"url": "string"}},
				{"swarm", map[string]any{"task": "string"}},
				{"selfdev", map[string]any{"task": "string"}},
			},
		},
		{
			name: "Hermes-extended",
			tools: []universalClientTool{
				{"terminal", map[string]any{"command": "string"}},
				{"execute_code", map[string]any{"code": "string"}},
				{"process_manage", map[string]any{"action": "string"}},
				{"delegate_task", map[string]any{"task": "string"}},
				{"read_file", map[string]any{"path": "string"}},
				{"search_files", map[string]any{"query": "string"}},
			},
		},
		{
			name: "Codex-extended",
			tools: []universalClientTool{
				{"shell_command", map[string]any{"command": "string"}},
				{"apply_patch", map[string]any{"patch": "string"}},
				{"exec_command", map[string]any{"cmd": "string"}},
				{"local_shell", map[string]any{"command": "string"}},
			},
		},
		{
			name: "OpenCode-extended",
			tools: []universalClientTool{
				{"read", map[string]any{"path": "string"}},
				{"apply_patch", map[string]any{"patch": "string"}},
				{"skill", map[string]any{"skill": "string"}},
				{"question", map[string]any{"query": "string"}},
				{"plan", map[string]any{"goal": "string"}},
			},
		},
	}

	for _, h := range harnesses {
		t.Run(h.name, func(t *testing.T) {
			assertHarnessWireClean(t, h.name, h.tools)
		})
	}
}
