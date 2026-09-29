package server_test

// Conformance replay for pi (earendil-works/pi) against the OpenAI
// chat/completions surface. pi's openai-completions route is a multi-protocol
// client surface: `new OpenAI({ baseURL }).chat.completions.create(...)` →
// POST {baseUrl}/v1/chat/completions (reference/harnesses/pi/WIRE-NOTES.md
// §4, api/openai-completions.ts:778-783), and the request carries a
// system-role message, native `tools` + `tool_choice`, and the stream
// delivers native tool_calls deltas (index + incremental arguments) with a
// terminal finish_reason "tool_calls" then [DONE] (§5, §4). The follow-up
// tool-result turn must preserve the role:tool message (its tool_call_id)
// when translated upstream — a proxy that drops the tool history breaks
// pi's tool-result turns (§10, api/openai-completions.ts:837-844).

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
)

// piToolCallIDAndType returns the id and type of the first tool_calls
// fragment bearing the given index, or "" for either when the stream never
// carried it.
func piToolCallIDAndType(frames []map[string]any, index int) (string, string) {
	for _, f := range frames {
		d := openAIDelta(f)
		if d == nil {
			continue
		}
		tcs, _ := d["tool_calls"].([]any)
		for _, tcRaw := range tcs {
			tc, _ := tcRaw.(map[string]any)
			if tc == nil {
				continue
			}
			if idx, _ := tc["index"].(float64); int(idx) != index {
				continue
			}
			if id, _ := tc["id"].(string); id != "" {
				typ, _ := tc["type"].(string)
				return id, typ
			}
		}
	}
	return "", ""
}

// TestConformancePiChatToolLoop replays pi's openai-completions tool loop:
// user → native tool call → tool_result → final answer. Asserts the stream
// delivers the indexed tool_calls delta (id/type/function.name +
// incremental arguments), terminates with finish_reason "tool_calls" and
// [DONE], and that the follow-up tool-result turn reaches the upstream with
// role:tool + tool_call_id preserved (pi requires the tool history or the
// tool-result turn breaks).
func TestConformancePiChatToolLoop(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: conformance lane excluded; run `go test ./backend/...` for the full tier")
	}
	mock := testutil.NewMock()
	defer mock.Close()

	var calls atomic.Int32
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if calls.Add(1) == 1 {
			// Turn 1: upstream answers a native tool call (index + 2 args
			// fragments), no end_turn pseudo-tool (pi's raw wire has none).
			_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-pi1", 500,
				`"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]`)))
			_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-pi1", 500,
				`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_pi_1","type":"function","function":{"name":"run_shell","arguments":"{\"cmd\":\""}}]},"finish_reason":null}]`)))
			_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-pi1", 500,
				`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ls\"}"}}]},"finish_reason":null}]`)))
			_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-pi1", 500,
				`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]`)))
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		// Turn 2: final answer after the tool_result.
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-pi2", 500,
			`"choices":[{"index":0,"delta":{"content":"Done. file1.txt"}},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-pi2", 500,
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}

	ts, _ := newTestServerCfg(t, []string{"pi-key"}, nil, mock)
	headers := map[string]string{"Authorization": "Bearer pi-key"}

	// --- Turn 1: system + user, native tools, tool_choice auto, stream ---
	turn1Body := `{
		"model": "` + modelA + `",
		"messages": [
			{"role": "system", "content": "You are pi, a coding agent."},
			{"role": "user", "content": "list files in /tmp"}
		],
		"stream": true,
		"tools": [{"type": "function", "function": {"name": "run_shell", "description": "Run a shell command and return its output", "parameters": {"type": "object", "properties": {"cmd": {"type": "string"}}, "required": ["cmd"]}}}],
		"tool_choice": "auto"
	}`
	resp1, data1 := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(turn1Body), headers)
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("turn 1 status = %d, want 200: %s", resp1.StatusCode, truncate(string(data1), 300))
	}
	if ct := resp1.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("turn 1 Content-Type = %q, want text/event-stream", ct)
	}
	body1 := string(data1)
	if !strings.HasSuffix(body1, "data: [DONE]\n\n") {
		t.Errorf("turn 1 stream must end with [DONE]: %q", truncate(body1, 300))
	}
	frames, done := collectOpenAIFrames(t, body1)
	if !done {
		t.Error("turn 1 stream missing the [DONE] terminator")
	}

	// Native tool call: indexed delta with id/type/name and incremental
	// arguments that assemble into complete JSON.
	id, typ := piToolCallIDAndType(frames, 0)
	if id != "call_pi_1" {
		t.Errorf("tool_calls delta id = %q, want call_pi_1", id)
	}
	if typ != "function" {
		t.Errorf("tool_calls delta type = %q, want function", typ)
	}
	if name := toolCallName(frames, 0); name != "run_shell" {
		t.Errorf("tool_calls function.name = %q, want run_shell", name)
	}
	args0 := joinToolArgs(frames, 0)
	assertToolArgsComplete(t, args0)
	if args0 != `{"cmd":"ls"}` {
		t.Errorf("joined tool arguments = %q, want %q", args0, `{"cmd":"ls"}`)
	}

	// Terminal finish_reason must be "tool_calls" (pi assembles the turn from it).
	fr, ok := findTerminalFinish(frames)
	if !ok {
		t.Error("turn 1 delivered no terminal finish_reason")
	} else if fr != "tool_calls" {
		t.Errorf("turn 1 terminal finish_reason = %q, want tool_calls", fr)
	}

	// Request translation, recorded upstream: system message + tools + tool_choice.
	if n := len(mock.RecordedChatBodiesSnapshot()); n != 1 {
		t.Errorf("upstream chat calls after turn 1 = %d, want 1", n)
	}
	for _, want := range []string{`"role":"system"`, `"run_shell"`, `"tool_choice":"auto"`} {
		if !mock.BodyContains(want) {
			t.Errorf("turn 1 upstream body missing %s", want)
		}
	}

	// --- Turn 2: feed the tool_result back, expect a clean 200 ---
	turn2Body := `{
		"model": "` + modelA + `",
		"messages": [
			{"role": "system", "content": "You are pi, a coding agent."},
			{"role": "user", "content": "list files in /tmp"},
			{"role": "assistant", "content": null, "tool_calls": [{"id": "call_pi_1", "type": "function", "function": {"name": "run_shell", "arguments": "{\"cmd\":\"ls\"}"}}]},
			{"role": "tool", "tool_call_id": "call_pi_1", "content": "file1.txt\ndir1\n"}
		],
		"stream": true,
		"tools": [{"type": "function", "function": {"name": "run_shell", "description": "Run a shell command and return its output", "parameters": {"type": "object", "properties": {"cmd": {"type": "string"}}, "required": ["cmd"]}}}],
		"tool_choice": "auto"
	}`
	resp2, data2 := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(turn2Body), headers)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("turn 2 status = %d, want 200: %s", resp2.StatusCode, truncate(string(data2), 300))
	}

	// The tool_result must reach the upstream as a role:tool message with the
	// echoed tool_call_id (pi's tool-result turn depends on the history).
	last := mock.LastChatBody()
	if !strings.Contains(last, `"role":"tool"`) {
		t.Errorf("upstream body missing role:tool message: %s", truncate(last, 400))
	}
	if !strings.Contains(last, `"tool_call_id":"call_pi_1"`) {
		t.Errorf("upstream body missing tool_call_id echoed from the tool_use id: %s", truncate(last, 400))
	}
	if !strings.Contains(last, "file1.txt") {
		t.Errorf("upstream body missing the tool result content: %s", truncate(last, 400))
	}
	if bodies := mock.RecordedChatBodiesSnapshot(); len(bodies) != 2 {
		t.Errorf("upstream chat request count = %d, want 2 (one per turn)", len(bodies))
	}
}
