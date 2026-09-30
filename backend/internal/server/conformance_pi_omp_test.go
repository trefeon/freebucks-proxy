package server_test

import (
	"encoding/json"
	"freebuff-proxy/backend/internal/testutil"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Hermetic "real user usage" conformance tests for the pi / Oh My Pi (OMP)
// coding agents (reference/harnesses/pi, reference/harnesses/oh-my-pi)
// against all three proxy surfaces.
//
// pi's core coding-agent tool registry
// (packages/coding-agent/src/core/tools/{bash,edit,edit-diff,find,grep,ls,
// powershell,read,write}.ts) emits the CLASSIC tool names (bash, edit, read,
// ...), and OMP's BUILTIN_TOOL_NAMES (packages/coding-agent/src/tools/
// builtin-names.ts) adds todo/web_search plus first-class loop tools with NO
// official codebuff equivalent (ask, task, hub, eval, lsp, browser, computer,
// github, ast_grep, ast_edit, checkpoint, rewind, security_scan, memory_edit,
// learn, manage_skill, debug, inspect_image).
//
// Contract under test (issue #140 + foreign_toolset gate):
//   1. Every classic pi/OMP tool with an official signature equivalent is
//      renamed upstream and restored to the EXACT client name downstream on
//      ALL surfaces (chat, responses, anthropic).
//   2. Tools with no equivalent pass through VERBATIM both ways, and the
//      injected end_turn keeps the request first-party regardless.
//   3. A tool_choice pinned to a mapped name is re-pointed at the official
//      name upstream.

// tool renders one OpenAI function tool entry for a request body.
func tool(name, params string) string {
	return `{"type":"function","function":{"name":"` + name + `","description":"` + name + ` tool","parameters":` + params + `}}`
}

// frameSetHasToolCall reports whether any collected SSE frame carries a
// tool_calls segment whose function name equals want.
func frameSetHasToolCall(frames []map[string]any, want string) bool {
	for _, f := range frames {
		choices, _ := f["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
		tcs, _ := delta["tool_calls"].([]any)
		for _, tc := range tcs {
			fn, _ := tc.(map[string]any)["function"].(map[string]any)
			if n, _ := fn["name"].(string); n == want {
				return true
			}
		}
	}
	return false
}

// TestConformancePiChatToolRenameRestore replays pi's OpenAI-completions
// surface (pi config api:"openai-completions"): the classic tool set in one
// turn. The upstream wire must carry the official signature names + the
// injected end_turn; the model's calls arrive back under the CLIENT names.
func TestConformancePiChatToolRenameRestore(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: conformance lane excluded; run `go test ./backend/...` for the full tier")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Upstream model calls three tools by their OFFICIAL names.
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-pi", 1,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_p0","type":"function","function":{"name":"read_files","arguments":"{\"paths\":[\"a.go\"]}"}}]},"index":0}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-pi", 1,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_p1","type":"function","function":{"name":"str_replace","arguments":"{\"path\":\"a.go\",\"old_string\":\"x\",\"new_string\":\"y\"}"}}]},"index":0}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-pi", 1,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_p2","type":"function","function":{"name":"code_search","arguments":"{\"pattern\":\"TODO\",\"path\":\".\"}"}}]},"index":0}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-pi", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":30,"completion_tokens":20,"total_tokens":50}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}

	ts, _ := newTestServer(t, []string{"pi-key"}, mock)
	// pi core tools, minus powershell (bash-flavored turn).
	piTools := `"tools":[` +
		tool("read", `{"type":"object","properties":{"paths":{"type":"array","items":{"type":"string"}}},"required":["paths"]}`) + `,` +
		tool("bash", `{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`) + `,` +
		tool("edit", `{"type":"object","properties":{"path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"}},"required":["path","old_string","new_string"]}`) + `,` +
		tool("write", `{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`) + `,` +
		tool("grep", `{"type":"object","properties":{"pattern":{"type":"string"}},"required":["pattern"]}`) + `,` +
		tool("ls", `{"type":"object","properties":{"path":{"type":"string"}},"required":[]}`) + `,` +
		tool("find", `{"type":"object","properties":{"pattern":{"type":"string"}},"required":["pattern"]}`) + `,` +
		tool("edit-diff", `{"type":"object","properties":{"file_path":{"type":"string"},"diff":{"type":"string"}},"required":["file_path","diff"]}`) + `]`

	reqBody := `{"model":"` + modelA + `","messages":[{"role":"user","content":"fix it"}],"stream":true,"stream_options":{"include_usage":true},` + piTools + `}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(reqBody), map[string]string{"Authorization": "Bearer pi-key"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}

	// Upstream wire carries official names + the first-party end_turn pin.
	recorded := mock.RecordedChatBodies[0]
	for _, want := range []string{
		`"name":"read_files"`, `"name":"run_terminal_command"`, `"name":"str_replace"`,
		`"name":"write_file"`, `"name":"code_search"`, `"name":"list_directory"`,
		`"name":"glob"`, `"name":"apply_patch"`, `"end_turn"`,
	} {
		if !strings.Contains(recorded, want) {
			t.Errorf("upstream body missing %s: %s", want, recorded)
		}
	}
	for _, gone := range []string{`"name":"read"`, `"name":"bash"`, `"name":"edit"`, `"name":"grep"`, `"name":"find"`, `"name":"find_files"`} {
		if strings.Contains(recorded, gone) {
			t.Errorf("upstream body still has client tool name %s: %s", gone, recorded)
		}
	}

	// Downstream: the model's official-named calls arrive under pi names.
	frames, done := collectOpenAIFrames(t, string(data))
	if !done {
		t.Error("stream missing [DONE]")
	}
	for _, want := range []string{"read", "edit", "grep"} {
		if !frameSetHasToolCall(frames, want) {
			t.Errorf("client stream missing restored tool call %q", want)
		}
	}
}

// TestConformancePiPowershellWindowsTurn replays the Windows-flavored pi turn
// (pi registers powershell.ts on win32; bash.ts on unix — a session never
// sends both, which is what keeps the shared run_terminal_command target
// unambiguous). Assert powershell renames upstream and restores downstream.
func TestConformancePiPowershellWindowsTurn(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: conformance lane excluded; run `go test ./backend/...` for the full tier")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-ps", 1,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_ps1","type":"function","function":{"name":"run_terminal_command","arguments":"{\"command\":\"dir\"}"}}]},"index":0}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-ps", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServer(t, []string{"pi-key"}, mock)
	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"dir"}],"stream":true,` +
		`"tools":[` + tool("powershell", `{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`) + `]}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), map[string]string{"Authorization": "Bearer pi-key"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	if !mock.BodyContains(`"name":"run_terminal_command"`) {
		t.Errorf("upstream body missing renamed powershell: %s", mock.RecordedChatBodies[0])
	}
	frames, _ := collectOpenAIFrames(t, string(data))
	if !frameSetHasToolCall(frames, "powershell") {
		t.Error("client stream missing restored powershell tool call")
	}
}

// piReshapeTools is a pi session carrying its full built-in set PLUS an
// extension subagent tool (`spawn_agent`, the shape pi extension/local
// harnesses register for delegation). The extension name leaves pi's bare
// vocabulary, so the family must be recognized from the core edit schema
// fingerprint (hasPiEditFingerprint) — and the extension itself must keep
// riding the wire virtualized (mcp__spawn_agent) with its calls restored.
func piReshapeTools() string {
	return `"tools":[` +
		tool("read", `{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"number"},"limit":{"type":"number"}},"required":["path"]}`) + `,` +
		tool("bash", `{"type":"object","properties":{"command":{"type":"string"},"timeout":{"type":"number"}},"required":["command"]}`) + `,` +
		tool("edit", `{"type":"object","properties":{"path":{"type":"string"},"edits":{"type":"array","items":{"type":"object","properties":{"oldText":{"type":"string"},"newText":{"type":"string"}},"required":["oldText","newText"]}}},"required":["path","edits"]}`) + `,` +
		tool("write", `{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`) + `,` +
		tool("grep", `{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"}},"required":["pattern"]}`) + `,` +
		tool("find", `{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"}},"required":["pattern"]}`) + `,` +
		tool("ls", `{"type":"object","properties":{"path":{"type":"string"}},"required":[]}`) + `,` +
		tool("spawn_agent", `{"type":"object","properties":{"task":{"type":"string"}},"required":["task"]}`) + `]`
}

// TestConformancePiArgReshapeE2E is the pi core loop end to end: the model
// fills the canonical CLI shapes it was shown, and every call lands in the
// client under pi's own name AND pi's own argument shape — multi-path read
// fans out per path, multi-replacement edit becomes ONE batch edit, bash
// carries timeout (not timeout_seconds), and the extension subagent tool
// comes back restored so delegation keeps working.
func TestConformancePiArgReshapeE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: conformance lane excluded; run `go test ./backend/...` for the full tier")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-pir", 1,
			`"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[`+
				`{"index":0,"id":"call_r1","type":"function","function":{"name":"read_files","arguments":"{\"paths\":[\"a.go\",\"b.go\"]}"}},`+
				`{"index":1,"id":"call_r2","type":"function","function":{"name":"str_replace","arguments":"{\"path\":\"f.go\",\"replacements\":[{\"oldString\":\"a\",\"newString\":\"b\"},{\"oldString\":\"c\",\"newString\":\"d\"}]}"}},`+
				`{"index":2,"id":"call_r3","type":"function","function":{"name":"run_terminal_command","arguments":"{\"command\":\"go test\",\"timeout_seconds\":120}"}},`+
				`{"index":3,"id":"call_r4","type":"function","function":{"name":"mcp__spawn_agent","arguments":"{\"task\":\"audit\"}"}}`+
				`]},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-pir", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":30,"completion_tokens":20,"total_tokens":50}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServer(t, []string{"pi-key"}, mock)
	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"do it"}],"stream":false,` + piReshapeTools() + `}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), map[string]string{"Authorization": "Bearer pi-key"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}

	// The wire carries mapped official names (pi is NOT floored: the
	// extension def rides virtualized) and canonical definitions.
	recorded := mock.RecordedChatBodies[0]
	for _, want := range []string{`"name":"read_files"`, `"name":"str_replace"`, `"name":"run_terminal_command"`, `"name":"glob"`, `"name":"list_directory"`, `"name":"mcp__spawn_agent"`} {
		if !strings.Contains(recorded, want) {
			t.Errorf("upstream body missing %s: %s", want, truncate(recorded, 300))
		}
	}
	for _, gone := range []string{`"name":"read"`, `"name":"bash"`, `"name":"find"`, `"name":"spawn_agent"`} {
		if strings.Contains(recorded, gone) {
			t.Errorf("upstream body still has client tool name %s: %s", gone, truncate(recorded, 300))
		}
	}

	var comp map[string]any
	if err := json.Unmarshal(data, &comp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	tcs, _ := comp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)
	type wantCall struct {
		name, id, args string
	}
	wants := []wantCall{
		{"read", "call_r1", `{"path":"a.go"}`},
		{"read", "call_r1-fanout-1", `{"path":"b.go"}`},
		{"edit", "call_r2", `{"path":"f.go","edits":[{"oldText":"a","newText":"b"},{"oldText":"c","newText":"d"}]}`},
		{"bash", "call_r3", `{"command":"go test","timeout":120}`},
		{"spawn_agent", "call_r4", `{"task":"audit"}`},
	}
	if len(tcs) != len(wants) {
		t.Fatalf("tool_calls = %d, want %d: %s", len(tcs), len(wants), truncate(string(data), 500))
	}
	for i, w := range wants {
		tc, _ := tcs[i].(map[string]any)
		fn, _ := tc["function"].(map[string]any)
		if fn["name"] != w.name {
			t.Errorf("call[%d] name = %v, want %s", i, fn["name"], w.name)
		}
		if tc["id"] != w.id {
			t.Errorf("call[%d] id = %v, want %s", i, tc["id"], w.id)
		}
		var gotM, wantM map[string]any
		_ = json.Unmarshal([]byte(fn["arguments"].(string)), &gotM)
		_ = json.Unmarshal([]byte(w.args), &wantM)
		gotB, _ := json.Marshal(gotM)
		wantB, _ := json.Marshal(wantM)
		if string(gotB) != string(wantB) {
			t.Errorf("call[%d] args = %s, want %s", i, gotB, wantB)
		}
	}
}

// TestConformancePiStreamArgReshape replays the streaming leg: fragmented
// CLI args are withheld and the terminal chunk injects pi-shaped wholes —
// two read calls for two paths, no CLI-shaped byte ever reaches the client.
func TestConformancePiStreamArgReshape(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: conformance lane excluded; run `go test ./backend/...` for the full tier")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-pis", 1,
			`"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_s1","type":"function","function":{"name":"read_files","arguments":"{\"paths\":[\"a.go\""}}]},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-pis", 1,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":",\"b.go\"]}"}}]},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-pis", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":30,"completion_tokens":20,"total_tokens":50}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServer(t, []string{"pi-key"}, mock)
	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"read two"}],"stream":true,` + piReshapeTools() + `}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), map[string]string{"Authorization": "Bearer pi-key"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	frames, done := collectOpenAIFrames(t, string(data))
	if !done {
		t.Error("stream missing [DONE]")
	}
	if name := toolCallName(frames, 0); name != "read" {
		t.Errorf("index 0 name = %q, want read", name)
	}
	if name := toolCallName(frames, 1); name != "read" {
		t.Errorf("index 1 name = %q, want read (fan-out extra)", name)
	}
	if args := joinToolArgs(frames, 0); args != `{"path":"a.go"}` {
		t.Errorf("index 0 args = %q, want pi read shape", args)
	}
	if args := joinToolArgs(frames, 1); args != `{"path":"b.go"}` {
		t.Errorf("index 1 args = %q, want pi read shape", args)
	}
	for _, f := range frames {
		if s, _ := json.Marshal(f); strings.Contains(string(s), `"paths"`) {
			t.Errorf("CLI-shaped args leaked into stream frame: %s", truncate(string(s), 200))
		}
	}
}

// piFlatTools renders pi's delegation-bearing toolset in the FLAT shape the
// Anthropic Messages and Responses surfaces use (input_schema / parameters
// inline, no nested function object). It carries the batch-edit fingerprint
// that identifies the pi family plus an extension subagent tool, so both the
// family detection and the extension virtualization are exercised on those
// surfaces.
func piFlatTools() string {
	mk := func(name, schema string) string {
		return `{"name":"` + name + `","description":"` + name + ` tool","input_schema":` + schema + `}`
	}
	return `[` +
		mk("read", `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`) + `,` +
		mk("edit", `{"type":"object","properties":{"path":{"type":"string"},"edits":{"type":"array","items":{"type":"object","properties":{"oldText":{"type":"string"},"newText":{"type":"string"}}}}},"required":["path","edits"]}`) + `,` +
		mk("spawn_agent", `{"type":"object","properties":{"task":{"type":"string"}},"required":["task"]}`) + `]`
}

func piResponsesTools() string {
	mk := func(name, schema string) string {
		return `{"type":"function","name":"` + name + `","description":"` + name + ` tool","parameters":` + schema + `}`
	}
	return `[` +
		mk("read", `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`) + `,` +
		mk("edit", `{"type":"object","properties":{"path":{"type":"string"},"edits":{"type":"array","items":{"type":"object","properties":{"oldText":{"type":"string"},"newText":{"type":"string"}}}}},"required":["path","edits"]}`) + `,` +
		mk("spawn_agent", `{"type":"object","properties":{"task":{"type":"string"}},"required":["task"]}`) + `]`
}

// TestConformancePiDelegationAcrossSurfaces pins delegation on the Anthropic
// and Responses surfaces for a pi session whose toolset includes an extension
// subagent tool. Delegation in pi is client-side (the extension runs the
// subagent), so the call only has to come back under pi's own name with pi's
// own args — a virtualized mcp__ name or a dropped entry would make the
// extension answer "Tool spawn_agent not found". The multi-path read also
// fans out on both surfaces (shared ReshapeCompletionCalls helper).
func TestConformancePiDelegationAcrossSurfaces(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: conformance lane excluded; run `go test ./backend/...` for the full tier")
	}
	mockTurn := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-pdel", 1,
			`"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[`+
				`{"index":0,"id":"call_d1","type":"function","function":{"name":"read_files","arguments":"{\"paths\":[\"a.go\",\"b.go\"]}"}},`+
				`{"index":1,"id":"call_d2","type":"function","function":{"name":"mcp__spawn_agent","arguments":"{\"task\":\"audit\"}"}}`+
				`]},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-pdel", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":10,"total_tokens":30}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}

	t.Run("anthropic", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.ChatHandler = mockTurn
		ts, _ := newTestServer(t, []string{"pi-key"}, mock)
		body := `{"model":"` + modelA + `","max_tokens":256,"messages":[{"role":"user","content":"delegate"}],"tools":` + piFlatTools() + `}`
		resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body),
			map[string]string{"Content-Type": "application/json", "x-api-key": "pi-key", "anthropic-version": "2023-06-01"})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
		}
		recorded := mock.LastChatBody()
		// Family recognized from the flat-shape batch edit; extension rides
		// virtualized (no gate-visible foreign name).
		if !strings.Contains(recorded, `"name":"mcp__spawn_agent"`) {
			t.Errorf("upstream body missing virtualized extension tool: %s", truncate(recorded, 300))
		}
		if strings.Contains(recorded, `"name":"spawn_agent"`) {
			t.Errorf("upstream body carries the bare extension name: %s", truncate(recorded, 300))
		}
		var msg map[string]any
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatalf("response not JSON: %v", err)
		}
		blocks, _ := msg["content"].([]any)
		var reads, spawns int
		for _, b := range blocks {
			bm, _ := b.(map[string]any)
			if bm["type"] != "tool_use" {
				continue
			}
			switch bm["name"] {
			case "read":
				reads++
				input, _ := bm["input"].(map[string]any)
				if _, batched := input["paths"]; batched {
					t.Errorf("read block kept CLI {paths} shape: %v", input)
				}
			case "spawn_agent":
				spawns++
				input, _ := bm["input"].(map[string]any)
				if input["task"] != "audit" {
					t.Errorf("spawn_agent input = %v, want {task:audit}", input)
				}
			}
		}
		if reads != 2 {
			t.Errorf("read tool_use blocks = %d, want 2 (per-path fan-out): %s", reads, truncate(string(data), 400))
		}
		if spawns != 1 {
			t.Errorf("spawn_agent tool_use blocks = %d, want 1 (delegation must dispatch)", spawns)
		}
	})

	t.Run("responses", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.ChatHandler = mockTurn
		ts, _ := newTestServer(t, []string{"pi-key"}, mock)
		body := `{"model":"` + modelA + `","input":[{"role":"user","content":[{"type":"input_text","text":"delegate"}]}],"tools":` + piResponsesTools() + `}`
		resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), map[string]string{"Authorization": "Bearer pi-key"})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
		}
		recorded := mock.LastChatBody()
		if !strings.Contains(recorded, `"name":"mcp__spawn_agent"`) {
			t.Errorf("upstream body missing virtualized extension tool: %s", truncate(recorded, 300))
		}
		var obj map[string]any
		if err := json.Unmarshal(data, &obj); err != nil {
			t.Fatalf("response not JSON: %v", err)
		}
		output, _ := obj["output"].([]any)
		var reads, spawns int
		for _, o := range output {
			om, _ := o.(map[string]any)
			if om["type"] != "function_call" {
				continue
			}
			switch om["name"] {
			case "read":
				reads++
				var args map[string]any
				_ = json.Unmarshal([]byte(om["arguments"].(string)), &args)
				if _, batched := args["paths"]; batched {
					t.Errorf("read call kept CLI {paths} shape: %v", args)
				}
			case "spawn_agent":
				spawns++
			}
		}
		if reads != 2 {
			t.Errorf("read function_calls = %d, want 2 (per-path fan-out): %s", reads, truncate(string(data), 400))
		}
		if spawns != 1 {
			t.Errorf("spawn_agent function_calls = %d, want 1 (delegation must dispatch)", spawns)
		}
	})
}

// TestConformanceOmpUnmappedLoopToolsVirtualized replays an OMP turn carrying
// the first-class loop tools that have NO official signature equivalent
// (ask/task/hub/eval/lsp/browser/computer/github/ast_grep/ast_edit/
// checkpoint/rewind/security_scan/memory_edit/learn/manage_skill/debug/
// inspect_image) plus the mapped todo and the official web_search. The
// family set goes floor-only (tools_floor.go): the wire carries the 16
// canonical floor defs + pins with zero riders (live 2026-09-30 the gate
// 503s on any foreign definition riding alongside the floor), while the
// mapper still restores dropped tools' names on the response path.
func TestConformanceOmpUnmappedLoopToolsVirtualized(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: conformance lane excluded; run `go test ./backend/...` for the full tier")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Model calls the unmapped ask tool under its virtual wire name.
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-omp", 1,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_ask1","type":"function","function":{"name":"mcp__ask","arguments":"{\"question\":\"ok?\"}"}}]},"index":0}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-omp", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServer(t, []string{"omp-key"}, mock)

	var tools []string
	for _, name := range []string{"ask", "task", "hub", "eval", "lsp", "browser", "computer", "github", "ast_grep", "ast_edit", "checkpoint", "rewind", "security_scan", "memory_edit", "learn", "manage_skill", "debug", "inspect_image", "todo", "web_search"} {
		tools = append(tools, tool(name, `{"type":"object","properties":{},"required":[]}`))
	}
	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"hi"}],"stream":true,"tools":[` + strings.Join(tools, ",") + `]}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), map[string]string{"Authorization": "Bearer omp-key"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	recorded := mock.RecordedChatBodies[0]
	// Floor-only (tools_floor.go): the OMP-family set (eval/learn/
	// manage_skill present) replaces every client def with the 16
	// canonical floor defs + pins — no virtualized riders, no verbatim
	// foreign names.
	for _, gone := range []string{`"name":"mcp__`, `"name":"ask"`, `"name":"task"`, `"name":"eval"`, `"name":"learn"`} {
		if strings.Contains(recorded, gone) {
			t.Errorf("floor-only wire carries %s: %s", gone, truncate(recorded, 300))
		}
	}
	for _, want := range []string{`"name":"run_terminal_command"`, `"name":"read_files"`, `"name":"write_todos"`, `"name":"web_search"`, "end_turn"} {
		if !strings.Contains(recorded, want) {
			t.Errorf("upstream body missing %s: %s", want, truncate(recorded, 300))
		}
	}
	frames, done := collectOpenAIFrames(t, string(data))
	if !done {
		t.Error("stream missing [DONE]")
	}
	if !frameSetHasToolCall(frames, "ask") {
		t.Error("client stream missing restored ask call")
	}
}

// TestConformancePiAnthropicToolRenameRestore replays pi's Anthropic Messages
// surface (api:"anthropic-messages", x-api-key auth): flat tools[].name
// entries are renamed on the translated upstream chat wire, and the tool_use
// content block the client parses opens with the CLIENT name.
func TestConformancePiAnthropicToolRenameRestore(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: conformance lane excluded; run `go test ./backend/...` for the full tier")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-anth", 1,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a1","type":"function","function":{"name":"run_terminal_command","arguments":"{\"command\":\"ls\"}"}}]},"index":0}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-anth", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":15,"completion_tokens":8,"total_tokens":23}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServer(t, []string{"pi-key"}, mock)
	headers := map[string]string{
		"Content-Type":      "application/json",
		"x-api-key":         "pi-key",
		"anthropic-version": "2023-06-01",
	}
	body := `{"model":"` + modelA + `","max_tokens":4096,"messages":[{"role":"user","content":"ls"}],` +
		`"tools":[{"name":"bash","description":"Run a command","input_schema":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}],` +
		`"stream":true}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	if !mock.BodyContains(`"name":"run_terminal_command"`) || !mock.BodyContains(`"end_turn"`) {
		t.Error("upstream chat body missing renamed tool / end_turn pin")
	}
	events := collectAnthropicEvents(t, string(data))
	idx, name, id := continueToolUseBlock(events)
	if name != "bash" || id != "call_a1" {
		t.Errorf("tool_use name/id = %q/%q, want bash/call_a1 (client dispatch name must survive)", name, id)
	}
	if args := replayInputFragments(events, idx); args != `{"command":"ls"}` {
		t.Errorf("assembled tool_use input = %q", args)
	}
	stop, _ := replayMessageDelta(events)
	if stop != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", stop)
	}
	if last := events[len(events)-1]; last["type"] != "message_stop" {
		t.Errorf("last event = %v, want message_stop", last["type"])
	}
}

// TestConformanceResponsesSurfaceToolRename replays a flat-tools Responses
// request (the pi openai-responses api shape): the flat function name is
// renamed on the upstream chat wire and the function_call item streamed back
// carries the CLIENT name.
func TestConformanceResponsesSurfaceToolRename(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: conformance lane excluded; run `go test ./backend/...` for the full tier")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-resp", 1,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_r1","type":"function","function":{"name":"run_terminal_command","arguments":"{\"command\":\"pwd\"}"}}]},"index":0}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-resp", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":9,"completion_tokens":4,"total_tokens":13}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServer(t, []string{"pi-key"}, mock)
	body := `{"model":"` + modelA + `","input":[{"role":"user","content":[{"type":"input_text","text":"pwd"}]}],"stream":true,` +
		`"tools":[{"type":"function","name":"bash","description":"Run a command","strict":false,"parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}],"tool_choice":"auto"}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), map[string]string{"Authorization": "Bearer pi-key"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	if !mock.BodyContains(`"name":"run_terminal_command"`) || !mock.BodyContains(`"end_turn"`) {
		t.Error("upstream body missing renamed tool / end_turn pin")
	}
	if !strings.Contains(string(data), `"name":"bash"`) {
		t.Errorf("responses stream missing client tool name bash: %s", truncate(string(data), 400))
	}
}

// TestConformanceToolChoicePinnedRename replays a chat request whose
// tool_choice pins a mapped client tool: the choice must be re-pointed at
// the official name upstream.
func TestConformanceToolChoicePinnedRename(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: conformance lane excluded; run `go test ./backend/...` for the full tier")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}
	ts, _ := newTestServer(t, []string{"pi-key"}, mock)
	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"run pwd"}],` +
		`"tools":[` + tool("bash", `{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`) + `],` +
		`"tool_choice":{"type":"function","function":{"name":"bash"}}}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), map[string]string{"Authorization": "Bearer pi-key"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	recorded := mock.RecordedChatBodies[0]
	if !strings.Contains(recorded, `"tool_choice":{"function":{"name":"run_terminal_command"}`) {
		t.Errorf("upstream tool_choice not re-pointed at official name: %s", recorded)
	}
}
