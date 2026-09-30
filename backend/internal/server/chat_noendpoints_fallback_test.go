package server_test

import (
	"freebucks-proxy/backend/internal/testutil"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// TestChatNoEndpointsFallsBackWithoutTools pins the issue #729 recovery
// (reopened #630 symptom): upstream routing 404s "No endpoints found for
// <model>" on a tools-bearing request while the same body without tools
// succeeds. The proxy must resolve endpoints like a tools-less request —
// retry once with the tools envelope stripped — instead of surfacing
// 502 model_no_endpoints.
//
// The mock replays the live shape: FIRST chat (tools present) 404s with
// the exact OpenRouter phrasing, SECOND chat (tools stripped) serves a
// normal SSE stream. Assertions cover the whole recovery: 200 to the
// client with content (never model_no_endpoints), exactly two upstream
// chat calls, and a tools-free retry body.
func TestChatNoEndpointsFallsBackWithoutTools(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var chatCalls atomic.Int32
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		if chatCalls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"No endpoints found for deepseek/deepseek-v4-flash.","code":404}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-729a", 1,
			`"choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":"stop"}]`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServer(t, nil, mock)

	// The exact issue repro shape: stream + one function tool.
	body := []byte(`{"model":"deepseek/deepseek-v4-flash","stream":true,` +
		`"messages":[{"role":"user","content":"Say hello in one sentence."}],` +
		`"tools":[{"type":"function","function":{"name":"test_tool","description":"A test tool",` +
		`"parameters":{"type":"object","properties":{}}}}]}`)
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no-endpoints must fall back to a tools-less retry): %s", resp.StatusCode, data)
	}
	if strings.Contains(string(data), "model_no_endpoints") {
		t.Errorf("body carries model_no_endpoints on the recovered path: %s", data)
	}
	if !strings.Contains(string(data), "hello") {
		t.Errorf("body missing the fallback completion content: %s", data)
	}
	if got := chatCalls.Load(); got != 2 {
		t.Fatalf("upstream chat calls = %d, want exactly 2 (failed tools attempt + one tools-less retry)", got)
	}
	bodies := mock.RecordedChatBodiesSnapshot()
	if len(bodies) != 2 {
		t.Fatalf("recorded chat bodies = %d, want 2", len(bodies))
	}
	if !strings.Contains(bodies[0], `"name":"test_tool"`) {
		t.Errorf("first chat body missing the client tool (fallback must trigger on the tools shape): %s", bodies[0])
	}
	if strings.Contains(bodies[1], `"tools"`) {
		t.Errorf("retry chat body still carries a tools envelope (must resolve like a tools-less request): %s", bodies[1])
	}
	if strings.Contains(bodies[1], `"tool_choice"`) {
		t.Errorf("retry chat body still carries tool_choice: %s", bodies[1])
	}
}

// TestChatNoEndpointsWithoutToolsStillSurfaces pins the fallback bound: a
// 404 with NO tools in the request must surface as 502 model_no_endpoints
// with exactly one upstream call — retrying the identical shape would
// re-trip the same fence, so no retry storm.
func TestChatNoEndpointsWithoutToolsStillSurfaces(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var chatCalls atomic.Int32
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		chatCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"message":"No endpoints found for deepseek/deepseek-v4-flash.","code":404}}`)
	}
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (tools-less 404 has no fallback shape): %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "model_no_endpoints") {
		t.Errorf("body missing model_no_endpoints: %s", data)
	}
	if got := chatCalls.Load(); got != 1 {
		t.Errorf("upstream chat calls = %d, want exactly 1 (no retry of an identical shape)", got)
	}
}
