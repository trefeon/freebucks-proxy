package server_test

// Issue #685 regression: opencode v2 offers CodeMode's `execute` tool beside
// its builtin `bash`. Both resolve to the official run_terminal_command wire
// name, so the request leg virtualizes the later one (`execute`) to
// mcp__execute to keep the upstream tool list name-unique. The response leg
// MUST restore that virtualized name to the client's own name — and it can
// only do so with the mapper the request normalized with: a mapper rebuilt
// from the client body cannot know about the dedupe and leaked mcp__execute
// to a client that never offered it (opencode v2 rejects the unknown name).
//
// Toolset source (reference/agents/opencode):
//   - packages/core/src/tool/builtins.ts: ApplyPatchTool, BashTool, EditTool,
//     GlobTool, GrepTool, QuestionTool, ReadTool, SkillTool, TodoWriteTool,
//     WebFetchTool, WebSearchTool, WriteTool.
//   - packages/codemode/docs/codemode.md: "When visible deferred tools exist,
//     Core reserves and materializes one `execute` tool" (Core integration on
//     dev: packages/core/src/tool/registry.ts + tool/execute.ts). It is
//     materialized LAST, which is why the leaked name observed live was
//     mcp__execute and not mcp__bash.

import (
	"freebucks-proxy/backend/internal/testutil"
	"io"
	"net/http"
	"strings"
	"testing"
)

// ocV2Tool is one opencode v2 tool: its client-facing name and a plausible
// parameter schema (the proxy forwards schemas untouched, so the schema only
// has to be well-formed JSON).
type ocV2Tool struct {
	name   string
	schema string
}

var ocV2Tools = []ocV2Tool{
	{"apply_patch", `{"type":"object","properties":{"patchText":{"type":"string"}},"required":["patchText"]}`},
	{"bash", `{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`},
	{"edit", `{"type":"object","properties":{"path":{"type":"string"},"oldString":{"type":"string"},"newString":{"type":"string"}},"required":["path"]}`},
	{"glob", `{"type":"object","properties":{"pattern":{"type":"string"}},"required":["pattern"]}`},
	{"grep", `{"type":"object","properties":{"pattern":{"type":"string"}},"required":["pattern"]}`},
	{"question", `{"type":"object","properties":{"questions":{"type":"array","items":{"type":"string"}}},"required":["questions"]}`},
	{"read", `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`},
	{"skill", `{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`},
	{"todowrite", `{"type":"object","properties":{"todos":{"type":"array","items":{"type":"string"}}},"required":["todos"]}`},
	{"webfetch", `{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}`},
	{"websearch", `{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`},
	{"write", `{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`},
	{"execute", `{"type":"object","properties":{"code":{"type":"string"}},"required":["code"]}`},
}

func ocV2ChatTools() string { return renderOCV2Tools(true, false) }
func ocV2FlatTools() string { return renderOCV2Tools(false, false) }
func ocV2AnthTools() string { return renderOCV2Tools(false, true) }

// renderOCV2Tools renders the toolset in one surface's declaration shape:
// chat-completions wrapped ({"type":"function","function":{...}}), Responses
// flat ({"type":"function","name":...}) or Anthropic ({"name","input_schema"}).
func renderOCV2Tools(chat, anthropicShape bool) string {
	parts := make([]string, 0, len(ocV2Tools))
	for _, tl := range ocV2Tools {
		switch {
		case chat:
			parts = append(parts, tool(tl.name, tl.schema))
		case anthropicShape:
			parts = append(parts, `{"name":"`+tl.name+`","description":"`+tl.name+` tool","input_schema":`+tl.schema+`}`)
		default:
			parts = append(parts, `{"type":"function","name":"`+tl.name+`","description":"`+tl.name+` tool","strict":false,"parameters":`+tl.schema+`}`)
		}
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// ocV2DedupeUpstream serves the model's call to the VIRTUALIZED name — the
// concrete shape that leaked to the opencode v2 client.
func ocV2DedupeUpstream() func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-dedupe", 1,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_d1","type":"function","function":{"name":"mcp__execute","arguments":"{\"code\":\"return 1\"}"}}]},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-dedupe", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":9,"total_tokens":29}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
}

// assertDedupeWire pins that the request leg really exercised the dedupe:
// bash claims run_terminal_command, execute virtualizes. Without this the
// client-side assertions below could pass for the wrong reason.
func assertDedupeWire(t *testing.T, mock *testutil.MockUpstream) {
	t.Helper()
	if len(mock.RecordedChatBodies) == 0 {
		t.Fatal("no upstream chat body recorded")
	}
	recorded := mock.RecordedChatBodies[0]
	for _, want := range []string{`"name":"run_terminal_command"`, `"name":"mcp__execute"`} {
		if !strings.Contains(recorded, want) {
			t.Errorf("upstream body missing %s (dedupe path not exercised): %s", want, truncate(recorded, 600))
		}
	}
}

// assertNoVirtualizedName fails when a name the request leg invented to keep
// the wire unique reaches the client. The client never offered it, so its
// dispatcher cannot route it.
func assertNoVirtualizedName(t *testing.T, body string) {
	t.Helper()
	if strings.Contains(body, "mcp__execute") {
		t.Errorf("virtualized upstream name mcp__execute leaked to the client: %s", truncate(body, 400))
	}
}

// TestToolDedupeRestoreChatNonStreaming covers the non-streaming relay: the
// upstream call is always forced-streaming, so the same SSE fixture is
// assembled into one completion object for a stream:false client, and the
// restore must survive that assembly.
func TestToolDedupeRestoreChatNonStreaming(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = ocV2DedupeUpstream()
	ts, _ := newTestServer(t, []string{"oc-key"}, mock)

	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"run it"}],"stream":false,"tools":` + ocV2ChatTools() + `}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), map[string]string{"Authorization": "Bearer oc-key"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	assertDedupeWire(t, mock)
	client := string(data)
	assertNoVirtualizedName(t, client)
	if !strings.Contains(client, `"name":"execute"`) {
		t.Errorf("completion missing the client's own tool name execute: %s", truncate(client, 400))
	}
}

// TestToolDedupeRestoreLegacyFunctions drives the legacy `functions` request
// shape (still accepted, converted to `tools` by the normalizer). The request
// mapper only learns those names while ToUpstream rewrites the wire list, so
// this surface is restore-correct only because the relays reuse the request's
// mapper — a rebuild from the raw body parses `tools` (absent here) and
// restores nothing.
func TestToolDedupeRestoreLegacyFunctions(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = ocV2DedupeUpstream()
	ts, _ := newTestServer(t, []string{"oc-key"}, mock)

	fns := []string{
		`{"name":"bash","description":"bash tool","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}`,
		`{"name":"execute","description":"execute tool","parameters":{"type":"object","properties":{"code":{"type":"string"}},"required":["code"]}}`,
	}
	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"run it"}],"stream":true,"functions":[` +
		strings.Join(fns, ",") + `],"function_call":"auto"}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), map[string]string{"Authorization": "Bearer oc-key"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	assertDedupeWire(t, mock)
	client := string(data)
	assertNoVirtualizedName(t, client)
	frames, done := collectOpenAIFrames(t, client)
	if !done {
		t.Error("stream missing [DONE]")
	}
	if !frameSetHasToolCall(frames, "execute") {
		t.Errorf("client stream has no tool call dispatched as execute: %s", truncate(client, 400))
	}
}

// TestToolDedupeRestoreChat drives the OpenAI chat surface: the model's call
// to the virtualized name must arrive as the client's `execute`.
func TestToolDedupeRestoreChat(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = ocV2DedupeUpstream()
	ts, _ := newTestServer(t, []string{"oc-key"}, mock)

	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"run it"}],"stream":true,"tools":` + ocV2ChatTools() + `}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), map[string]string{"Authorization": "Bearer oc-key"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	assertDedupeWire(t, mock)
	client := string(data)
	assertNoVirtualizedName(t, client)
	if !strings.Contains(client, `"name":"execute"`) {
		t.Errorf("client stream missing the client's own tool name execute: %s", truncate(client, 400))
	}
	frames, done := collectOpenAIFrames(t, client)
	if !done {
		t.Error("stream missing [DONE]")
	}
	if !frameSetHasToolCall(frames, "execute") {
		t.Errorf("client stream has no tool call dispatched as execute: %s", truncate(client, 400))
	}
}

// TestToolDedupeRestoreMessages drives the Anthropic surface (opencode's
// anthropic provider path): the restored name must open the tool_use block.
func TestToolDedupeRestoreMessages(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = ocV2DedupeUpstream()
	ts, _ := newTestServer(t, []string{"oc-key"}, mock)

	body := `{"model":"` + modelA + `","max_tokens":4096,"messages":[{"role":"user","content":"run it"}],` +
		`"stream":true,"tools":` + ocV2AnthTools() + `}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), map[string]string{
		"Content-Type":      "application/json",
		"x-api-key":         "oc-key",
		"anthropic-version": "2023-06-01",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	assertDedupeWire(t, mock)
	client := string(data)
	assertNoVirtualizedName(t, client)
	events := collectAnthropicEvents(t, client)
	idx, name, id := continueToolUseBlock(events)
	if name != "execute" || id != "call_d1" {
		t.Errorf("tool_use name/id = %q/%q, want execute/call_d1", name, id)
	}
	if args := replayInputFragments(events, idx); args != `{"code":"return 1"}` {
		t.Errorf("assembled tool_use input = %q, want the client-shaped arguments", args)
	}
}

// TestToolDedupeRestoreResponses drives the Responses surface (opencode's
// default OpenAI provider path): the function_call output item must carry the
// restored name.
func TestToolDedupeRestoreResponses(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = ocV2DedupeUpstream()
	ts, _ := newTestServer(t, []string{"oc-key"}, mock)

	body := `{"model":"` + modelA + `","input":[{"role":"user","content":[{"type":"input_text","text":"run it"}]}],` +
		`"stream":true,"tools":` + ocV2FlatTools() + `,"tool_choice":"auto"}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), map[string]string{"Authorization": "Bearer oc-key"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	assertDedupeWire(t, mock)
	client := string(data)
	assertNoVirtualizedName(t, client)
	events := collectResponsesEvents(t, client)
	item := codexItemOfType(events, "response.output_item.done", "function_call")
	if item == nil {
		t.Fatalf("no function_call output_item.done in the stream: %s", truncate(client, 400))
	}
	if name, _ := item["name"].(string); name != "execute" {
		t.Errorf("function_call item name = %q, want execute", name)
	}
	if args, _ := item["arguments"].(string); args != `{"code":"return 1"}` {
		t.Errorf("function_call item arguments = %q, want the client-shaped arguments", args)
	}
}
