package server_test

import (
	"encoding/json"
	"freebuff-proxy/backend/internal/testutil"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// Argument payloads as raw JSON (backtick strings, no escapes); embedded
// into SSE chunks with strconv.Quote.
const (
	flArgsFull  = `{"command":"ls","cwd":"/tmp","timeout_seconds":30}`
	flArgsFrag1 = `{"command":"ls"`
	flArgsFrag2 = `,"cwd":"/tmp","timeout_seconds":30}`
)

// Floor-only end to end (OMP family): the wire carries the 16 canonical
// floor defs + pins, and tool calls come back with OMP names AND OMP-shaped
// args — the model fills CLI schemas, the client dispatches OMP schemas.
func ompFloorTools() string {
	return `[{"type":"function","function":{"name":"bash","description":"Run","parameters":{"type":"object","properties":{"command":{"type":"string"},"i":{"type":"string"}},"required":["command","i"]}}},` +
		`{"type":"function","function":{"name":"read","description":"Read","parameters":{"type":"object","properties":{"path":{"type":"string"},"i":{"type":"string"}},"required":["path","i"]}}},` +
		`{"type":"function","function":{"name":"eval","description":"Eval","parameters":{"type":"object","properties":{"language":{"type":"string"},"code":{"type":"string"}}}}}]`
}

func assertFloorOnlyWire(t *testing.T, mock *testutil.MockUpstream) {
	t.Helper()
	recorded := mock.LastChatBody()
	if strings.Contains(recorded, "mcp__") {
		t.Errorf("floor-only wire carries virtualized rider: %s", truncate(recorded, 300))
	}
	if !strings.Contains(recorded, `"process_type"`) {
		t.Error("wire missing canonical run_terminal_command schema")
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(recorded), &body); err != nil {
		t.Fatalf("upstream body not JSON: %v", err)
	}
	tools, _ := body["tools"].([]any)
	// 16 canonical + end_turn: upstream topUp drops the convert-layer
	// decide pin (foreign/banned signature), so the live wire is 17.
	if len(tools) != 17 {
		t.Errorf("wire tools = %d, want 17 (16 floor + end_turn)", len(tools))
	}
}

func TestFloorOnlyNonStreamReshape(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	// The upstream leg is always forced-streaming: serve SSE even for a
	// stream:false client; the relay assembles the completion object.
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-fl1", 1,
			`"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_fl_1","type":"function","function":{"name":"run_terminal_command","arguments":`+strconv.Quote(flArgsFull)+`}`+`}`+`]`+`}`+`,"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-fl1", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":60,"completion_tokens":20,"total_tokens":80}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServer(t, nil, mock)
	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"list files"}],"stream":false,"tools":` + ompFloorTools() + `}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
	}
	assertFloorOnlyWire(t, mock)
	var comp map[string]any
	if err := json.Unmarshal(data, &comp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	tcs := comp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)
	fn := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "bash" {
		t.Errorf("tool name = %v, want bash", fn["name"])
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(fn["arguments"].(string)), &args); err != nil {
		t.Fatalf("args not JSON: %v", err)
	}
	if args["command"] != "ls" || args["cwd"] != "/tmp" || args["timeout"] != float64(30) {
		t.Errorf("args = %v, want OMP bash shape {command,cwd,timeout}", args)
	}
	if _, ok := args["timeout_seconds"]; ok {
		t.Errorf("args carry CLI key timeout_seconds: %v", args)
	}
}

func TestFloorOnlyStreamReshape(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-fl2", 1,
			`"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_fl_2","type":"function","function":{"name":"run_terminal_command","arguments":`+strconv.Quote(flArgsFrag1)+`}`+`}`+`]`+`}`+`,"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-fl2", 1,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":`+strconv.Quote(flArgsFrag2)+`}}]},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-fl2", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":60,"completion_tokens":20,"total_tokens":80}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServer(t, nil, mock)
	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"list files"}],"stream":true,"tools":` + ompFloorTools() + `}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
	}
	assertFloorOnlyWire(t, mock)
	frames, done := collectOpenAIFrames(t, string(data))
	if !done {
		t.Error("stream missing [DONE]")
	}
	if name := toolCallName(frames, 0); name != "bash" {
		t.Errorf("tool call name = %q, want bash", name)
	}
	if args := joinToolArgs(frames, 0); args != `{"command":"ls","cwd":"/tmp","timeout":30}` {
		t.Errorf("assembled args = %q, want OMP bash shape", args)
	}
	for _, f := range frames {
		if s, _ := json.Marshal(f); strings.Contains(string(s), "timeout_seconds") {
			t.Errorf("CLI-shaped args leaked into stream frame: %s", truncate(string(s), 200))
		}
	}
}
