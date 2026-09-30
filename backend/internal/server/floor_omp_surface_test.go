package server_test

import (
	"encoding/json"
	"fmt"
	"freebuff-proxy/backend/internal/testutil"
	"net/http"
	"strings"
	"testing"
)

// ompDispatchableToolNames is OMP's complete callable surface as the client
// dispatches it: BUILTIN_TOOL_NAMES + HIDDEN_TOOL_NAMES
// (pi-coding-agent/src/tools/builtin-names.ts, omp 18.4.x) plus the
// `mcp__<server>_<tool>` external namespace. A floor-only relay must never
// hand the client a name outside this set, and must never mangle a name
// inside it.
var ompDispatchableToolNames = []string{
	// builtins
	"read", "bash", "edit", "ast_grep", "ast_edit", "ask", "debug", "ida",
	"eval", "github", "glob", "grep", "find", "lsp", "checkpoint", "rewind",
	"context_notes", "new_context", "security_scan", "task", "wait", "todo",
	"web_search", "write", "memory_edit", "retain", "recall", "reflect",
	"learn", "manage_skill",
	// hidden
	"yield", "goal", "think",
	// external / MCP
	"mcp__resend_send", "mcp__postgres_query",
	// subagent + coordination capabilities the model reaches through the
	// prompt vocabulary (task/wait/hub-style) — no official CLI equivalent.
	"task",
}

// Every OMP callable name survives the floor-only response leg byte-identically:
// OMP dispatches on the name it declared or documented, so the relay must not
// rename, drop or virtualize it. The two suppressed pseudo-tools (end_turn,
// decide) are the sole exceptions and are covered elsewhere.
func TestFloorOMPToolSurfaceRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	seen := map[string]bool{}
	names := make([]string, 0, len(ompDispatchableToolNames))
	for _, n := range ompDispatchableToolNames {
		if seen[n] {
			continue
		}
		seen[n] = true
		names = append(names, n)
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
				tfMockTurn(w,
					`{"index":0,"id":"call_x","type":"function","function":{"name":"`+name+`","arguments":"{}"}}`,
					"tool_calls")
			}
			ts, _ := newTestServer(t, nil, mock)
			body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"go"}],"stream":false,"tools":` + ompFloorTools() + `}`
			resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
			}
			var comp map[string]any
			if err := json.Unmarshal(data, &comp); err != nil {
				t.Fatalf("response not JSON: %v", err)
			}
			msg, _ := comp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
			tcs, _ := msg["tool_calls"].([]any)
			if len(tcs) == 0 {
				t.Fatalf("OMP call %q was suppressed; body: %s", name, truncate(string(data), 300))
			}
			got, _ := tcs[0].(map[string]any)["function"].(map[string]any)["name"].(string)
			if got != name {
				t.Errorf("client name = %q, want %q (OMP dispatches on its own name)", got, name)
			}
			if strings.HasPrefix(got, "mcp__") != strings.HasPrefix(name, "mcp__") {
				t.Errorf("client name %q changed MCP namespace of %q", got, name)
			}
		})
	}
}

// The four floor tools the model can call but OMP cannot dispatch never reach
// the client as a tool call: each is either rendered as assistant text or
// absorbed, and none appears by name in the response.
func TestFloorUnroutableNamesNeverRelayed(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	cases := []struct {
		wire      string
		args      string
		wantParts []string
	}{
		{"suggest_followups", `{"followups":[{"prompt":"Ship the fix"}]}`, []string{"Suggested next steps", "Ship the fix"}},
		{"render_ui", `{"widget":{"type":"button","text":"Open report","link":"https://example.test/r"}}`, []string{"Open report", "https://example.test/r"}},
		{"gravity_index", `{"action":"search","query":"kv store"}`, []string{"Gravity Index", "kv store"}},
		{"report_project_profile", `{"status":"unchanged"}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.wire, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
				tfMockTurn(w, fmt.Sprintf(`{"index":0,"id":"call_u","type":"function","function":{"name":%q,"arguments":%q}}`, tc.wire, tc.args), "tool_calls")
			}
			ts, _ := newTestServer(t, nil, mock)
			body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"go"}],"stream":false,"tools":` + ompFloorTools() + `}`
			resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
			}
			out := string(data)
			if strings.Contains(out, `"`+tc.wire+`"`) {
				t.Errorf("undispatchable %s call reached the client: %s", tc.wire, truncate(out, 300))
			}
			var comp map[string]any
			if err := json.Unmarshal(data, &comp); err != nil {
				t.Fatalf("response not JSON: %v", err)
			}
			choice := comp["choices"].([]any)[0].(map[string]any)
			msg := choice["message"].(map[string]any)
			if tcs, ok := msg["tool_calls"].([]any); ok && len(tcs) > 0 {
				t.Fatalf("tool_calls = %v, want none", tcs)
			}
			content, _ := msg["content"].(string)
			for _, want := range tc.wantParts {
				if !strings.Contains(content, want) {
					t.Errorf("content %q missing %q", content, want)
				}
			}
			if choice["finish_reason"] != "stop" {
				t.Errorf("finish_reason = %v, want stop", choice["finish_reason"])
			}
		})
	}
}
