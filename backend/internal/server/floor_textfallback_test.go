package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
)

// Unroutable floor emissions end to end: a model call to a floor tool OMP
// cannot dispatch (suggest_followups, render_ui, gravity_index,
// report_project_profile) must never reach the client as a tool call. The
// relay suppresses it and renders the payload as assistant text (or absorbs
// it), so the agent loop never takes a "Tool <name> not found" turn.

func tfMockTurn(w http.ResponseWriter, calls, finish string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-tf", 1,
		`"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[`+calls+`]},"finish_reason":null}]`)))
	_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-tf", 1,
		`"choices":[{"index":0,"delta":{},"finish_reason":"`+finish+`"}],"usage":{"prompt_tokens":50,"completion_tokens":10,"total_tokens":60}`)))
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

// A followup suggestion split across two argument fragments must assemble,
// render as assistant text, and leave the turn with finish_reason stop —
// never a tool_calls turn naming suggest_followups.
func TestFloorTextFallbackStreamFollowups(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-tff", 1,
			`"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_sf","type":"function","function":{"name":"suggest_followups","arguments":`+
				strconv.Quote(`{"followups":[{"prompt":"Add tests for the `)+`}}]},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-tff", 1,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":`+
				strconv.Quote(`change"}]}`)+`}}]},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-tff", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":50,"completion_tokens":10,"total_tokens":60}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServer(t, nil, mock)
	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"done?"}],"stream":true,"tools":` + ompFloorTools() + `}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
	}
	assertFloorOnlyWire(t, mock)
	out := string(data)
	if strings.Contains(out, `"suggest_followups"`) {
		t.Errorf("client received an undispatchable suggest_followups call: %q", truncate(out, 400))
	}
	if !strings.Contains(out, "Suggested next steps") || !strings.Contains(out, "Add tests for the change") {
		t.Errorf("rendered followups missing; body: %q", truncate(out, 600))
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Errorf("terminal finish_reason not flipped to stop: %q", truncate(out, 400))
	}
	if strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Errorf("finish_reason tool_calls survived a call-less turn: %q", truncate(out, 400))
	}
}

// A dispatchable call in the same turn keeps the turn a tool_calls turn; the
// unroutable sibling is still rendered as text and never relayed by name.
func TestFloorTextFallbackStreamKeepsRealCall(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		tfMockTurn(w,
			`{"index":0,"id":"call_b1","type":"function","function":{"name":"run_terminal_command","arguments":"{\"command\":\"ls\"}"}},`+
				`{"index":1,"id":"call_s1","type":"function","function":{"name":"suggest_followups","arguments":"{\"followups\":[{\"prompt\":\"Run unit tests\",\"label\":\"Tests\"}]}"}}`,
			"tool_calls")
	}
	ts, _ := newTestServer(t, nil, mock)
	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"go"}],"stream":true,"tools":` + ompFloorTools() + `}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
	}
	out := string(data)
	if strings.Contains(out, `"suggest_followups"`) {
		t.Errorf("client received an undispatchable suggest_followups call: %q", truncate(out, 400))
	}
	if !strings.Contains(out, `"name":"bash"`) {
		t.Errorf("dispatchable call not restored: %q", truncate(out, 400))
	}
	if !strings.Contains(out, "Suggested next steps") || !strings.Contains(out, "Run unit tests") {
		t.Errorf("suggest_followups note missing; body: %q", truncate(out, 600))
	}
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Errorf("finish_reason tool_calls lost while a real call remains: %q", truncate(out, 400))
	}
}

// Non-streaming chat: the suppressed call leaves no tool_calls entry, the
// payload rides as message content, and finish_reason reads stop.
func TestFloorTextFallbackNonStreamChat(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		tfMockTurn(w,
			`{"index":0,"id":"call_rp","type":"function","function":{"name":"report_project_profile","arguments":"{\"status\":\"unchanged\"}"}},`+
				`{"index":1,"id":"call_ru","type":"function","function":{"name":"render_ui","arguments":"{\"widget\":{\"type\":\"button\",\"text\":\"Open preview\",\"link\":\"https://example.test/p\"}}"}}`,
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
	choice := comp["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if tcs, ok := msg["tool_calls"].([]any); ok && len(tcs) > 0 {
		t.Fatalf("tool_calls = %v, want none (all unroutable)", tcs)
	}
	content, _ := msg["content"].(string)
	if !strings.Contains(content, "Open preview") || !strings.Contains(content, "https://example.test/p") {
		t.Errorf("content = %q, want the rendered widget link", content)
	}
	if strings.Contains(content, "report_project") {
		t.Errorf("absorbed report_project_profile leaked into content: %q", content)
	}
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v, want stop", choice["finish_reason"])
	}
}

// Anthropic non-streaming: the suppressed call must not become a tool_use
// block, and the stop reason must not advertise tool use.
func TestFloorTextFallbackAnthropicNonStream(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		tfMockTurn(w,
			`{"index":0,"id":"call_sf","type":"function","function":{"name":"suggest_followups","arguments":"{\"followups\":[{\"prompt\":\"Ship it\"}]}"}}`,
			"tool_calls")
	}
	ts, _ := newTestServer(t, nil, mock)
	body := `{"model":"` + modelA + `","max_tokens":256,"messages":[{"role":"user","content":"done?"}],"tools":` + anthropicFloorTools() + `}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), map[string]string{"anthropic-version": "2023-06-01"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
	}
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if msg["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v, want end_turn", msg["stop_reason"])
	}
	blocks, _ := msg["content"].([]any)
	for _, raw := range blocks {
		block, _ := raw.(map[string]any)
		if block["type"] == "tool_use" {
			t.Fatalf("tool_use block survived suppression: %v", block["name"])
		}
	}
	if !strings.Contains(string(data), "Ship it") {
		t.Errorf("rendered followup missing from Anthropic response: %q", truncate(string(data), 600))
	}
}
