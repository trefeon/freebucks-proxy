package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestToolMapperRequestRename pins the request-side rename (#140): mapped
// client names become official signature names on the wire; schemas pass
// through untouched; unmapped and already-official names are unchanged.
func TestToolMapperRequestRename(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[
		{"type":"function","function":{"name":"read_file","description":"Read a file","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}},
		{"type":"function","function":{"name":"bash","description":"Run a command","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}},
		{"type":"function","function":{"name":"my_custom_tool","parameters":{"type":"object","properties":{}}}},
		{"type":"function","function":{"name":"write_file","description":"Official already","parameters":{"type":"object","properties":{}}}}
	],"tool_choice":{"type":"function","function":{"name":"read_file"}}}`)

	out, mapper, err := NormalizeRequestMapped(body, "")
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatal(err)
	}

	names := map[string]map[string]any{}
	tools := payload["tools"].([]any)
	for _, tl := range tools {
		fn := tl.(map[string]any)["function"].(map[string]any)
		names[fn["name"].(string)] = fn
	}

	if _, ok := names["read_files"]; !ok {
		t.Errorf("read_file not renamed to read_files: tools = %v", payload["tools"])
	}
	if _, ok := names["run_terminal_command"]; !ok {
		t.Errorf("bash not renamed to run_terminal_command")
	}
	if _, ok := names["my_custom_tool"]; !ok {
		t.Error("unmapped custom tool was altered")
	}
	if _, ok := names["read_file"]; ok {
		t.Error("original client name still present after rename")
	}

	// Schema passes through untouched (the model fills args per this shape).
	rf := names["read_files"]
	params := rf["parameters"].(map[string]any)
	reqd := params["required"].([]any)
	if len(reqd) != 1 || reqd[0] != "path" {
		t.Errorf("client schema mutated: required = %v", reqd)
	}

	// tool_choice follows the rename.
	tc := payload["tool_choice"].(map[string]any)
	tcfn := tc["function"].(map[string]any)
	if tcfn["name"] != "read_files" {
		t.Errorf("tool_choice name = %v, want read_files", tcfn["name"])
	}

	// The mapper restores both directions.
	if got := mapper.RestoreName("read_files"); got != "read_file" {
		t.Errorf("RestoreName(read_files) = %q, want read_file", got)
	}
	if got := mapper.RestoreName("run_terminal_command"); got != "bash" {
		t.Errorf("RestoreName(run_terminal_command) = %q, want bash", got)
	}
	if got := mapper.RestoreName("end_turn"); got != "end_turn" {
		t.Errorf("RestoreName(end_turn) = %q, want identity", got)
	}
}

// TestToolMapperResponseRestore pins the response-side restore on both chunk
// shapes: streaming delta.tool_calls and non-streaming message.tool_calls.
func TestToolMapperResponseRestore(t *testing.T) {
	mapper := NewToolMapper([]byte(`{"tools":[
		{"type":"function","function":{"name":"bash","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}},
		{"type":"function","function":{"name":"write_to_file","parameters":{"type":"object","properties":{"path":{"type":"string"},"instructions":{"type":"string"},"content":{"type":"string"}}}}}
	]}`))
	if mapper.Len() != 2 {
		t.Fatalf("mapper entries = %d, want 2", mapper.Len())
	}

	streamChunk := map[string]any{
		"choices": []any{map[string]any{
			"delta": map[string]any{
				"tool_calls": []any{
					map[string]any{"index": float64(0), "function": map[string]any{"name": "run_terminal_command", "arguments": "{}"}},
				},
			},
		}},
	}
	if !mapper.FromUpstreamChunk(streamChunk) {
		t.Fatal("stream chunk unchanged by restore")
	}
	delta := streamChunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	fn := delta["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "bash" {
		t.Errorf("stream restored name = %v, want bash", fn["name"])
	}

	jsonChunk := map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{
				"tool_calls": []any{
					map[string]any{"id": "c1", "function": map[string]any{"name": "write_file"}},
				},
			},
		}},
	}
	if !mapper.FromUpstreamChunk(jsonChunk) {
		t.Fatal("json chunk unchanged by restore")
	}
	msg := jsonChunk["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	jfn := msg["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if jfn["name"] != "write_to_file" {
		t.Errorf("json restored name = %v, want write_to_file", jfn["name"])
	}

	// Identity mapper never reports change.
	empty := ToolMapper{}
	if empty.FromUpstreamChunk(streamChunk) {
		t.Error("identity mapper changed a chunk")
	}
}

// TestToolMapperMcpVirtualizedRestore pins the collision-free MCP restore for
// a virtualized harness name: Claude-Code "Task" becomes "mcp__Task" on the
// request leg and must restore to the exact client name downstream, on both
// chunk shapes and the single-name path.
func TestToolMapperMcpVirtualizedRestore(t *testing.T) {
	mapper := NewToolMapper([]byte(`{"tools":[
		{"type":"function","function":{"name":"Task","parameters":{"type":"object"}}}
	]}`))
	if mapper.Len() != 1 {
		t.Fatalf("mapper entries = %d, want 1 (Task virtualized)", mapper.Len())
	}
	if got := mapper.RestoreName("mcp__Task"); got != "Task" {
		t.Errorf("RestoreName(mcp__Task) = %q, want Task", got)
	}
	chunk := map[string]any{"choices": []any{
		map[string]any{"delta": map[string]any{"tool_calls": []any{
			map[string]any{"function": map[string]any{"name": "mcp__Task"}},
		}}},
		map[string]any{"message": map[string]any{"tool_calls": []any{
			map[string]any{"function": map[string]any{"name": "mcp__Task"}},
		}}},
	}}
	if !mapper.FromUpstreamChunk(chunk) {
		t.Fatal("chunk with virtualized name unchanged by restore")
	}
	for i, sel := range []string{"delta", "message"} {
		section := chunk["choices"].([]any)[i].(map[string]any)[sel].(map[string]any)
		fn := section["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
		if fn["name"] != "Task" {
			t.Errorf("%s restored name = %v, want Task", sel, fn["name"])
		}
	}
}

// TestToolMapperMcpNativePassthrough pins that a client-native mcp__* name
// with NO map entry (real MCP tool, passed through verbatim on the request
// leg) passes through byte-identical downstream — never blind-stripped.
func TestToolMapperMcpNativePassthrough(t *testing.T) {
	mapper := NewToolMapper([]byte(`{"tools":[
		{"type":"function","function":{"name":"mcp__my_tool","parameters":{"type":"object"}}}
	]}`))
	if mapper.Len() != 0 {
		t.Fatalf("mapper entries = %d, want 0 (native MCP name maps nothing)", mapper.Len())
	}
	if got := mapper.RestoreName("mcp__my_tool"); got != "mcp__my_tool" {
		t.Errorf("RestoreName(mcp__my_tool) = %q, want byte-identical passthrough", got)
	}
	chunk := map[string]any{"choices": []any{
		map[string]any{"delta": map[string]any{"tool_calls": []any{
			map[string]any{"function": map[string]any{"name": "mcp__my_tool"}},
		}}},
	}}
	if mapper.FromUpstreamChunk(chunk) {
		t.Error("native MCP name chunk reported change")
	}
	delta := chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	fn := delta["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "mcp__my_tool" {
		t.Errorf("native MCP name = %v, want mcp__my_tool untouched", fn["name"])
	}
}

// TestToolMapperSignatureCoverage guards the layer's whole point: every
// mapping target must be a signature tool upstream recognizes, so renamed
// requests classify first-party under detectForeignFreebuffClient.
func TestToolMapperSignatureCoverage(t *testing.T) {
	for client, official := range clientToOfficial {
		if official == "" {
			continue
		}
		if !officialTools[official] {
			t.Errorf("%q maps to %q which is not in officialTools", client, official)
		}
		if strings.ToLower(client) != client {
			t.Errorf("mapping key %q is not lowercase; lookups always lowercase", client)
		}
	}
}

// TestToolMapperCounts pins the console MSG/TOOL segments: NewToolMapper
// retains the client message and tool counts from its single body parse
// (Responses input[] counts as messages). Zero value and garbage bodies
// report 0 so the console omits the segments.
func TestToolMapperCounts(t *testing.T) {
	chat := NewToolMapper([]byte(`{"model":"m","messages":[{"role":"u"},{"role":"a"},{"role":"u"}],"tools":[{"function":{"name":"a"}},{"function":{"name":"b"}}]}`))
	if chat.MsgCount() != 3 {
		t.Errorf("chat msgs = %d, want 3", chat.MsgCount())
	}
	if chat.ToolCount() != 2 {
		t.Errorf("chat tools = %d, want 2", chat.ToolCount())
	}
	resp := NewToolMapper([]byte(`{"model":"m","input":[{"role":"u"},{"role":"u"}],"tools":[]}`))
	if resp.MsgCount() != 2 {
		t.Errorf("responses msgs = %d, want 2 (input[])", resp.MsgCount())
	}
	if resp.ToolCount() != 0 {
		t.Errorf("responses tools = %d, want 0", resp.ToolCount())
	}
	var zero ToolMapper
	if zero.MsgCount() != 0 || zero.ToolCount() != 0 {
		t.Error("zero mapper counts nonzero")
	}
	bad := NewToolMapper([]byte(`not json`))
	if bad.MsgCount() != 0 || bad.ToolCount() != 0 {
		t.Error("garbage body counts nonzero")
	}
}

// TestAllHarnessToolsBidirectionalMapping verifies that every tool from the
// client harnesses in reference/harnesses renames to an official signature tool for upstream
// and restores cleanly back to the client's original casing/name downstream.
func TestAllHarnessToolsBidirectionalMapping(t *testing.T) {
	testCases := []struct {
		harness      string
		clientTool   string
		wantOfficial string
	}{
		// Qwen-Code
		{"Qwen-Code", "run_shell_command", "run_terminal_command"},
		{"Qwen-Code", "grep_search", "code_search"},
		{"Qwen-Code", "todo_write", "write_todos"},
		{"Qwen-Code", "web_fetch", "read_url"},
		// Goose
		{"Goose", "developer__shell", "run_terminal_command"},
		{"Goose", "developer__bash", "run_terminal_command"},
		{"Goose", "developer__text_editor", "str_replace"},
		{"Goose", "developer__read", "read_files"},
		{"Goose", "developer__write", "write_file"},
		{"Goose", "developer__edit", "str_replace"},
		{"Goose", "computer__execute", "run_terminal_command"},
		// Continue (camelCase client tools)
		{"Continue", "readFile", "read_files"},
		{"Continue", "editFile", "str_replace"},
		{"Continue", "createNewFile", "write_file"},
		{"Continue", "runTerminalCommand", "run_terminal_command"},
		{"Continue", "grepSearch", "code_search"},
		{"Continue", "globSearch", "glob"},
		{"Continue", "fetchUrlContent", "read_url"},
		{"Continue", "searchWeb", "web_search"},
		{"Continue", "viewSubdirectory", "list_directory"},
		{"Continue", "singleFindAndReplace", "str_replace"},
		// Roo-Code / Cline
		{"Roo-Code", "apply_diff", "apply_patch"},
		{"Roo-Code", "edit_file", "str_replace"},
		{"Roo-Code", "search_replace", "str_replace"},
		{"Roo-Code", "search_and_replace", "str_replace"},
		{"Roo-Code", "codebase_search", "code_search"},
		{"Roo-Code", "update_todo_list", "write_todos"},
		{"Roo-Code", "read_command_output", "run_terminal_command"},
		{"Cline", "editor", "str_replace"},
		{"Cline", "fetch_web", "read_url"},
		{"Cline", "search", "code_search"},
		// Aider
		{"Aider", "replace_lines", "str_replace"},
		// Codex
		{"Codex", "exec_command", "run_terminal_command"},
		{"Codex", "exec", "run_terminal_command"},
		// Pi / OMP
		{"Pi", "powershell", "run_terminal_command"},
		{"Pi", "find", "find_files"},
		{"Pi", "edit-diff", "apply_patch"},
		// Kilocode / OpenCode
		{"Kilocode", "execute_bash", "run_terminal_command"},
		{"Kilocode", "fuzzy_search", "code_search"},
		{"Kilocode", "list_dir", "list_directory"},
		{"Kilocode", "websearch", "web_search"},
		{"Kilocode", "webfetch", "read_url"},
		// Gemini-CLI
		{"Gemini-CLI", "read_many_files", "read_files"},
		{"Gemini-CLI", "replace", "str_replace"},
		{"Gemini-CLI", "google_web_search", "web_search"},
		{"Gemini-CLI", "activate_skill", "skill"},
		{"Gemini-CLI", "search_file_content", "code_search"},
		// Hermes
		{"Hermes", "terminal", "run_terminal_command"},
		{"Hermes", "web_extract", "read_url"},
		{"Hermes", "patch", "str_replace"},
		{"Hermes", "todo_list", "write_todos"},
		{"Hermes", "skills_list", "skill"},
		{"Hermes", "skill_view", "skill"},
		{"Hermes", "skill_manage", "skill"},
		// OpenHands agent-server
		{"OpenHands", "terminal", "run_terminal_command"},
		{"OpenHands", "invoke_skill", "skill"},
		// Crush-additions
		{"Crush", "fetch", "read_url"},
		{"Crush", "multiedit", "str_replace"},
		{"Crush", "sourcegraph", "code_search"},
		// Kimi-additions
		{"Kimi-CLI", "StrReplaceFile", "str_replace"},
		// Codewhale
		{"Codewhale", "exec_shell", "run_terminal_command"},
		{"Codewhale", "fetch_url", "read_url"},
		{"Codewhale", "web.fetch", "read_url"},
		{"Codewhale", "grep_files", "code_search"},
		{"Codewhale", "file_search", "glob"},
		// jcode
		{"Jcode", "shell_exec", "run_terminal_command"},
		{"Jcode", "agentgrep", "code_search"},
		{"Jcode", "file_grep", "code_search"},
		{"Jcode", "multiedit", "str_replace"},
		{"Jcode", "patch", "str_replace"},
		{"Jcode", "todoread", "write_todos"},
		{"Jcode", "todo_read", "write_todos"},
		// Reasonix
		{"Reasonix", "multi_edit", "str_replace"},
		{"Reasonix", "complete_step", "write_todos"},
		// Claude Code (PascalCase names; mapping is case-insensitive and
		// restore must return the EXACT client casing)
		{"Claude-Code", "Bash", "run_terminal_command"},
		{"Claude-Code", "Read", "read_files"},
		{"Claude-Code", "Edit", "str_replace"},
		{"Claude-Code", "Write", "write_file"},
		{"Claude-Code", "Grep", "code_search"},
		{"Claude-Code", "LS", "list_directory"},
		{"Claude-Code", "TodoWrite", "write_todos"},
		{"Claude-Code", "WebFetch", "read_url"},
		{"Claude-Code", "WebSearch", "web_search"},
	}

	for _, tc := range testCases {
		t.Run(tc.harness+"/"+tc.clientTool, func(t *testing.T) {
			props := map[string]any{}
			if canonicalKeys, ok := canonicalToolParameterKeys[tc.wantOfficial]; ok {
				for k := range canonicalKeys {
					props[k] = map[string]any{"type": "string"}
				}
			}
			body, _ := json.Marshal(map[string]any{
				"model":    "m",
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
				"tools": []any{
					map[string]any{
						"type": "function",
						"function": map[string]any{
							"name":       tc.clientTool,
							"parameters": map[string]any{"type": "object", "properties": props},
						},
					},
				},
			})

			norm, mapper, err := NormalizeRequestMapped(body, "")
			if err != nil {
				t.Fatalf("NormalizeRequestMapped failed: %v", err)
			}

			var parsed map[string]any
			if err := json.Unmarshal(norm, &parsed); err != nil {
				t.Fatalf("unmarshal normalized failed: %v", err)
			}

			tools := parsed["tools"].([]any)
			fn := tools[0].(map[string]any)["function"].(map[string]any)
			gotOfficial := fn["name"].(string)
			if gotOfficial != tc.wantOfficial {
				t.Errorf("normalized tool name = %q, want %q", gotOfficial, tc.wantOfficial)
			}

			// Downstream restoration must restore the client's exact name
			restored := mapper.RestoreName(gotOfficial)
			if restored != tc.clientTool {
				t.Errorf("RestoreName(%q) = %q, want %q", gotOfficial, restored, tc.clientTool)
			}
		})
	}
}

// TestHarnessToolsetsWireClassification verifies that the real-world toolsets
// of the major AI coding agents (Claude Code, Hermes, OMP, OpenCode, Pi, Codex,
// and Cursor) produce wire tools that classify cleanly under upstream detection:
//
//   - WireForeignSignal(v) == "" (no foreign signals)
//   - len(v.Foreign) == 0 (zero foreign harness names on the wire)
//   - len(v.Hollow) == 0 (zero hollow signature tools on the wire)
//   - Downstream restoration restores every client tool name byte-for-byte.
func TestHarnessToolsetsWireClassification(t *testing.T) {
	type clientToolDef struct {
		name   string
		params map[string]any
	}

	harnesses := []struct {
		name  string
		tools []clientToolDef
	}{
		{
			name: "Claude Code",
			tools: []clientToolDef{
				{"Bash", map[string]any{"command": "string"}},
				{"Read", map[string]any{"file_path": "string"}},
				{"Edit", map[string]any{"file_path": "string", "old_string": "string", "new_string": "string"}},
				{"Write", map[string]any{"file_path": "string", "content": "string"}},
				{"Glob", map[string]any{"pattern": "string", "path": "string"}},
				{"Grep", map[string]any{"pattern": "string", "path": "string"}},
				{"Agent", map[string]any{"prompt": "string"}},
				{"AskUserQuestion", map[string]any{"question": "string"}},
				{"Task", map[string]any{"description": "string"}},
				{"Skill", map[string]any{"skill": "string"}},
				{"KillShell", map[string]any{"shell_id": "string"}},
				{"BashOutput", map[string]any{"shell_id": "string"}},
				{"SlashCommand", map[string]any{"command": "string"}},
				{"EnterPlanMode", map[string]any{}},
				{"ExitPlanMode", map[string]any{}},
				{"TodoWrite", map[string]any{"todos": "array"}},
				{"WebFetch", map[string]any{"url": "string"}},
				{"WebSearch", map[string]any{"query": "string"}},
			},
		},
		{
			name: "Hermes",
			tools: []clientToolDef{
				{"terminal", map[string]any{"command": "string"}},
				{"web_extract", map[string]any{"url": "string"}},
				{"patch", map[string]any{"path": "string", "patch": "string"}},
				{"todo_list", map[string]any{"action": "string"}},
				{"skills_list", map[string]any{}},
				{"skill_view", map[string]any{"name": "string"}},
				{"skill_manage", map[string]any{"action": "string"}},
				{"clarify", map[string]any{"question": "string"}},
			},
		},
		{
			name: "OMP",
			tools: []clientToolDef{
				{"bash", map[string]any{"command": "string"}},
				{"read", map[string]any{"path": "string"}},
				{"edit", map[string]any{"path": "string", "input": "string"}},
				{"write", map[string]any{"path": "string", "content": "string"}},
				{"grep", map[string]any{"pattern": "string"}},
				{"glob", map[string]any{"pattern": "string"}},
				{"task", map[string]any{"task": "string"}},
				{"eval", map[string]any{"code": "string"}},
				{"hub", map[string]any{"op": "string"}},
				{"todo", map[string]any{"action": "string"}},
			},
		},
		{
			name: "OpenCode",
			tools: []clientToolDef{
				{"execute_bash", map[string]any{"command": "string"}},
				{"fuzzy_search", map[string]any{"query": "string"}},
				{"list_dir", map[string]any{"path": "string"}},
				{"websearch", map[string]any{"query": "string"}},
				{"webfetch", map[string]any{"url": "string"}},
				{"todowrite", map[string]any{"todos": "array"}},
				{"todoread", map[string]any{}},
			},
		},
		{
			name: "Pi",
			tools: []clientToolDef{
				{"powershell", map[string]any{"command": "string"}},
				{"read", map[string]any{"path": "string"}},
				{"edit", map[string]any{"path": "string"}},
				{"write", map[string]any{"path": "string"}},
				{"find", map[string]any{"pattern": "string"}},
				{"bash", map[string]any{"command": "string"}},
			},
		},
		{
			name: "Codex",
			tools: []clientToolDef{
				{"exec_command", map[string]any{"cmd": "string"}},
				{"write_stdin", map[string]any{"data": "string"}},
				{"request_user_input", map[string]any{"prompt": "string"}},
				{"container_exec", map[string]any{"command": "string"}},
				{"shell", map[string]any{"command": "string"}},
			},
		},
		{
			name: "Cursor",
			tools: []clientToolDef{
				{"Shell", map[string]any{"command": "string"}},
				{"StrReplace", map[string]any{"path": "string", "old": "string", "new": "string"}},
				{"AskQuestion", map[string]any{"text": "string"}},
				{"ReadLints", map[string]any{"paths": "array"}},
				{"Delete", map[string]any{"path": "string"}},
			},
		},
	}

	for _, h := range harnesses {
		t.Run(h.name, func(t *testing.T) {
			var toolsArr []any
			for _, ct := range h.tools {
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
			// Map-first restore: only names this request leg mapped or
			// virtualized shed the mcp__ prefix downstream. The synthetic
			// "mcp__"+ct.name candidates below (for tools that mapped to an
			// official name or passed through untouched) are names the upstream
			// leg never emits for this mapper, so they pass through verbatim —
			// the same native-MCP passthrough pinned by
			// TestToolMapperMcpNativePassthrough. What this loop pins is that
			// the chunk path and RestoreName agree on every candidate shape.
			for _, ct := range h.tools {
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
		})
	}
}
