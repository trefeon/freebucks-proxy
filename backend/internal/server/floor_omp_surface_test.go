package server_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
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

// OMP's delegation surface must survive the floor-only relay on every
// surface, because subagent spawning is CLIENT-SIDE: OMP's task tool
// (pi-coding-agent/src/task/index.ts:507, "Spawn subagents to complete
// delegated tasks", params agent/name/task/context/tasks[]/batch) runs the
// subagent locally, and `hub` coordinates the resulting jobs. The proxy's
// only job is to hand the call back under OMP's own name with OMP's own
// args — a rename, a virtualized mcp__name or a dropped entry is what turns
// delegation into "Tool task not found". The model reaches them through
// OMP's own # Tool Inventory system prompt (the proxy prepends its canonical
// opening, never replaces the client prompt), so no client def needs to ride
// the gate-mandated floor.
const (
	ompTaskArgs  = `{"agent":"researcher","task":"audit the auth flow","context":"focus on session refresh"}`
	ompTaskFrag1 = `{"agent":"researcher","task":"audit the auth `
	ompTaskFrag2 = `flow","context":"focus on session refresh"}`
	ompHubArgs   = `{"op":"wait","job_id":"job-7"}`
)

// Streaming chat: a fragmented task call and a whole hub call come back
// verbatim (name + args), the turn stays a tool_calls turn, and the request
// leg carried zero foreign riders.
func TestFloorOmpDelegationStreamsVerbatim(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Name-only first fragment (live OMP shape), args on continuations.
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-del", 1,
			`"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_t1","type":"function","function":{"name":"task"}}]},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-del", 1,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":`+strconv.Quote(ompTaskFrag1)+`}}]},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-del", 1,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":`+strconv.Quote(ompTaskFrag2)+`}},`+
				`{"index":1,"id":"call_h1","type":"function","function":{"name":"hub","arguments":`+strconv.Quote(ompHubArgs)+`}}]},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-del", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":40,"completion_tokens":20,"total_tokens":60}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServer(t, nil, mock)
	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"delegate it"}],"stream":true,"tools":` + ompFloorTools() + `}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
	}
	assertFloorOnlyWire(t, mock)
	frames, done := collectOpenAIFrames(t, string(data))
	if !done {
		t.Error("stream missing [DONE]")
	}
	if name := toolCallName(frames, 0); name != "task" {
		t.Errorf("index 0 name = %q, want task (delegation must dispatch)", name)
	}
	if args := joinToolArgs(frames, 0); args != ompTaskArgs {
		t.Errorf("index 0 args = %q, want verbatim OMP task args %q", args, ompTaskArgs)
	}
	if name := toolCallName(frames, 1); name != "hub" {
		t.Errorf("index 1 name = %q, want hub", name)
	}
	if args := joinToolArgs(frames, 1); args != ompHubArgs {
		t.Errorf("index 1 args = %q, want verbatim OMP hub args %q", args, ompHubArgs)
	}
	// A delegation turn must stay a tool turn, not be downgraded to text.
	last := frames[len(frames)-1]
	if fr, _ := last["choices"].([]any)[0].(map[string]any)["finish_reason"].(string); fr != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", fr)
	}
	if strings.Contains(string(data), "mcp__") {
		t.Errorf("delegation call was virtualized downstream: %s", truncate(string(data), 300))
	}
	if strings.Contains(string(data), "not found") {
		t.Errorf("response advertises an undispatched tool: %s", truncate(string(data), 300))
	}
}

// Non-streaming Anthropic + Responses surfaces: the same task call arrives
// with its own name and its own args on both, so a pi/OMP client that speaks
// either surface delegates identically.
func TestFloorOmpDelegationAcrossSurfaces(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	taskCall := `{"index":0,"id":"call_t1","type":"function","function":{"name":"task","arguments":` + strconv.Quote(ompTaskArgs) + `}}`

	t.Run("anthropic", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
			tfMockTurn(w, taskCall, "tool_calls")
		}
		ts, _ := newTestServer(t, nil, mock)
		body := `{"model":"` + modelA + `","max_tokens":256,"messages":[{"role":"user","content":"delegate"}],"tools":` + anthropicFloorTools() + `}`
		resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body),
			map[string]string{"Content-Type": "application/json", "anthropic-version": "2023-06-01"})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
		}
		assertFloorOnlyWire(t, mock)
		var msg map[string]any
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatalf("response not JSON: %v", err)
		}
		blocks, _ := msg["content"].([]any)
		var use map[string]any
		for _, b := range blocks {
			bm, _ := b.(map[string]any)
			if bm["type"] == "tool_use" {
				use = bm
			}
		}
		if use == nil {
			t.Fatalf("no tool_use block: %s", truncate(string(data), 300))
		}
		if use["name"] != "task" {
			t.Errorf("tool_use name = %v, want task", use["name"])
		}
		input, _ := use["input"].(map[string]any)
		if input["agent"] != "researcher" || input["task"] != "audit the auth flow" {
			t.Errorf("tool_use input = %v, want verbatim OMP task args", input)
		}
		if msg["stop_reason"] != "tool_use" {
			t.Errorf("stop_reason = %v, want tool_use", msg["stop_reason"])
		}
	})

	t.Run("responses", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
			tfMockTurn(w, taskCall, "tool_calls")
		}
		ts, _ := newTestServer(t, nil, mock)
		body := `{"model":"` + modelA + `","input":[{"role":"user","content":[{"type":"input_text","text":"delegate"}]}],"tools":` + responsesFloorTools() + `}`
		resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
		}
		assertFloorOnlyWire(t, mock)
		var obj map[string]any
		if err := json.Unmarshal(data, &obj); err != nil {
			t.Fatalf("response not JSON: %v", err)
		}
		output, _ := obj["output"].([]any)
		var call map[string]any
		for _, o := range output {
			om, _ := o.(map[string]any)
			if om["type"] == "function_call" {
				call = om
			}
		}
		if call == nil {
			t.Fatalf("no function_call item: %s", truncate(string(data), 300))
		}
		if call["name"] != "task" {
			t.Errorf("function_call name = %v, want task", call["name"])
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(call["arguments"].(string)), &args); err != nil {
			t.Fatalf("function_call arguments not JSON: %v", err)
		}
		if args["agent"] != "researcher" || args["context"] != "focus on session refresh" {
			t.Errorf("function_call arguments = %v, want verbatim OMP task args", args)
		}
	})
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
