package server_test

// Conformance replay for Gemini-CLI's wire tool names against the OpenAI
// chat/completions surface. Gemini emits its own vocabulary
// (reference/agents/gemini-cli/packages/core/src/tools/definitions/
// base-declarations.ts: read_file, replace, grep_search, run_shell_command,
// google_web_search, web_fetch, activate_skill) plus legacy aliases kept for
// policy/skill back-compat (tool-names.ts TOOL_LEGACY_ALIASES:
// search_file_content -> grep_search), MCP tools qualified as
// mcp_{server}_{tool} (mcp-tool.ts MCP_TOOL_PREFIX), and locally discovered
// commands under discovered_tool_ (tool-names.ts DISCOVERED_TOOL_PREFIX).
// The proxy must rename the mapped names to the official signature
// equivalents upstream, leave MCP/discovered names verbatim, and restore
// client names on the response path.

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"freebucks-proxy/backend/internal/testutil"
)

func TestConformanceGeminiToolNames(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: conformance lane excluded; run `go test ./backend/...` for the full tier")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Upstream model calls two tools by their OFFICIAL names.
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-gem1", 901,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_gem_1","type":"function","function":{"name":"run_terminal_command","arguments":"{\"command\":\"ls\"}"}}]},"index":0}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-gem1", 901,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_gem_2","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"x\"}"}}]},"index":0}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-gem1", 901,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":60,"completion_tokens":20,"total_tokens":80}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServer(t, nil, mock)

	tool := func(name, desc string) string {
		return `{"type":"function","function":{"name":"` + name + `","description":"` + desc + `",` +
			`"parameters":{"type":"object","properties":{}}}}`
	}
	body := `{"model":"` + modelA + `",` +
		`"messages":[{"role":"user","content":"list files"}],` +
		`"stream":true,` +
		`"tools":[` + strings.Join([]string{
		tool("read_file", "Read a file"),
		tool("replace", "Edit a file"),
		tool("grep_search", "Search content"),
		tool("run_shell_command", "Run a command"),
		tool("google_web_search", "Search the web"),
		tool("search_file_content", "Legacy search alias"),
		tool("mcp_github_list_issues", "MCP tool"),
		tool("discovered_tool_mycommand", "Local command"),
	}, ",") + `],` +
		`"tool_choice":"auto"}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
	}

	// Upstream carries official signature names for every mapped client name.
	recorded := mock.LastChatBody()
	for _, want := range []string{
		`"name":"read_files"`, `"name":"str_replace"`, `"name":"code_search"`,
		`"name":"run_terminal_command"`, `"name":"web_search"`,
	} {
		if !strings.Contains(recorded, want) {
			t.Errorf("upstream body missing renamed tool %s: %s", want, truncate(recorded, 600))
		}
	}
	// Mapped client names (including the legacy alias) are gone from name
	// slots; MCP and discovered tools pass through verbatim.
	for _, gone := range []string{
		`"name":"read_file"`, `"name":"replace"`, `"name":"grep_search"`,
		`"name":"run_shell_command"`, `"name":"search_file_content"`,
		`"name":"google_web_search"`,
	} {
		if strings.Contains(recorded, gone) {
			t.Errorf("upstream body still carries client name %s: %s", gone, truncate(recorded, 600))
		}
	}
	for _, want := range []string{`"name":"mcp_github_list_issues"`, `"name":"discovered_tool_mycommand"`} {
		if !strings.Contains(recorded, want) {
			t.Errorf("upstream body missing verbatim tool %s: %s", want, truncate(recorded, 600))
		}
	}

	// Streamed deltas restore the client's own names.
	frames, done := collectOpenAIFrames(t, string(data))
	if !done {
		t.Error("stream missing [DONE]")
	}
	for _, want := range []string{"run_shell_command", "google_web_search"} {
		if !frameSetHasToolCall(frames, want) {
			t.Errorf("stream missing restored tool call %q", want)
		}
	}
	for _, gone := range []string{"run_terminal_command", "web_search"} {
		if frameSetHasToolCall(frames, gone) {
			t.Errorf("stream leaks official name %q", gone)
		}
	}
}
