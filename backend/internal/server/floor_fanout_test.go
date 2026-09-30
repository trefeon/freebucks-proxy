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

// Floor-only fan-out end to end: multi-path reads and multi-replacement
// edits expand to one OMP call per item on every whole-args leg (chat
// non-streaming, chat streaming flush, Anthropic + Responses non-streaming).

const (
	fanReadArgs = `{"paths":["a.go","b.go"]}`
	fanEditArgs = `{"path":"f","replacements":[{"oldString":"a","newString":"b"},{"oldString":"c","newString":"d"}]}`
	fanFrag1    = `{"paths":["a.go"`
	fanFrag2    = `,"b.go"]}`
)

func fanMockTurn(w http.ResponseWriter, calls string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-fan", 1,
		`"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[`+calls+`]},"finish_reason":null}]`)))
	_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-fan", 1,
		`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":60,"completion_tokens":20,"total_tokens":80}`)))
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

func TestFloorOnlyNonStreamFanout(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		fanMockTurn(w,
			`{"index":0,"id":"call_m1","type":"function","function":{"name":"read_files","arguments":`+strconv.Quote(fanReadArgs)+`}},`+
				`{"index":1,"id":"call_m2","type":"function","function":{"name":"str_replace","arguments":`+strconv.Quote(fanEditArgs)+`}}`)
	}
	ts, _ := newTestServer(t, nil, mock)
	// ompFloorTools declares bash/read (floor trigger via intent-i) but no
	// edit: append one so str_replace restores to the client name.
	floorTools := strings.TrimSuffix(ompFloorTools(), "]") +
		`,{"type":"function","function":{"name":"edit","description":"Edit","parameters":{"type":"object","properties":{"path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"}},"required":["path","old_string","new_string"]}}}]`
	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"do it"}],"stream":false,"tools":` + floorTools + `}`
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
	if len(tcs) != 4 {
		t.Fatalf("tool_calls = %d, want 4 (read x2 + edit x2)", len(tcs))
	}
	type want struct {
		name, id, args string
	}
	wants := []want{
		{"read", "call_m1", `{"path":"a.go"}`},
		{"read", "call_m1-fanout-1", `{"path":"b.go"}`},
		{"edit", "call_m2", `{"path":"f","old_string":"a","new_string":"b"}`},
		{"edit", "call_m2-fanout-1", `{"path":"f","old_string":"c","new_string":"d"}`},
	}
	for i, w := range wants {
		tc := tcs[i].(map[string]any)
		fn := tc["function"].(map[string]any)
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

func TestFloorOnlyStreamFanoutFlush(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// The wire args arrive split across two fragments; the terminal
		// chunk must inject one reshaped whole per path.
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-fan2", 1,
			`"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_s1","type":"function","function":{"name":"read_files","arguments":`+strconv.Quote(fanFrag1)+`}}]},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-fan2", 1,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":`+strconv.Quote(fanFrag2)+`}}]},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-fan2", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":60,"completion_tokens":20,"total_tokens":80}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServer(t, nil, mock)
	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"read two"}],"stream":true,"tools":` + ompFloorTools() + `}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
	}
	assertFloorOnlyWire(t, mock)
	frames, done := collectOpenAIFrames(t, string(data))
	if !done {
		t.Error("stream missing [DONE]")
	}
	// First keeps its index, the extra takes a fresh one; each assembles
	// exactly one OMP-shaped call.
	if name := toolCallName(frames, 0); name != "read" {
		t.Errorf("index 0 name = %q, want read", name)
	}
	if name := toolCallName(frames, 1); name != "read" {
		t.Errorf("index 1 name = %q, want read (fan-out extra)", name)
	}
	if args := joinToolArgs(frames, 0); args != `{"path":"a.go"}` {
		t.Errorf("index 0 args = %q, want OMP read shape", args)
	}
	if args := joinToolArgs(frames, 1); args != `{"path":"b.go"}` {
		t.Errorf("index 1 args = %q, want OMP read shape", args)
	}
	for _, f := range frames {
		if s, _ := json.Marshal(f); strings.Contains(string(s), `"paths"`) {
			t.Errorf("CLI-shaped args leaked into stream frame: %s", truncate(string(s), 200))
		}
	}
}

func anthropicFloorTools() string {
	mk := func(name string) string {
		return `{"name":"` + name + `","description":"` + name + ` tool","input_schema":{"type":"object","properties":{}}}`
	}
	return `[` + mk("read") + `,` + mk("eval") + `,` + mk("learn") + `]`
}

func TestFloorOnlyAnthropicJSONFanout(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		fanMockTurn(w,
			`{"index":0,"id":"call_a1","type":"function","function":{"name":"read_files","arguments":`+strconv.Quote(fanReadArgs)+`}}`)
	}
	ts, _ := newTestServer(t, nil, mock)
	body := `{"model":"` + modelA + `","max_tokens":100,"messages":[{"role":"user","content":"read two"}],` +
		`"tools":` + anthropicFloorTools() + `,"stream":false}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body),
		map[string]string{"Content-Type": "application/json", "anthropic-version": "2023-06-01"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	content, _ := msg["content"].([]any)
	var blocks []map[string]any
	for _, b := range content {
		bm, _ := b.(map[string]any)
		if bm["type"] == "tool_use" {
			blocks = append(blocks, bm)
		}
	}
	if len(blocks) != 2 {
		t.Fatalf("tool_use blocks = %d, want 2: %s", len(blocks), truncate(string(data), 400))
	}
	for i, wantPath := range []string{"a.go", "b.go"} {
		if blocks[i]["name"] != "read" {
			t.Errorf("block[%d] name = %v, want read", i, blocks[i]["name"])
		}
		input, _ := blocks[i]["input"].(map[string]any)
		if input["path"] != wantPath || len(input) != 1 {
			t.Errorf("block[%d] input = %v, want {path:%s}", i, input, wantPath)
		}
	}
	if blocks[0]["id"] == blocks[1]["id"] {
		t.Errorf("fan-out blocks share id %v (must be unique for result echo)", blocks[0]["id"])
	}
	if msg["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v, want tool_use", msg["stop_reason"])
	}
}

func responsesFloorTools() string {
	mk := func(name string) string {
		return `{"type":"function","name":"` + name + `","description":"` + name + ` tool","parameters":{"type":"object","properties":{}}}`
	}
	return `[` + mk("read") + `,` + mk("eval") + `,` + mk("learn") + `]`
}

func TestFloorOnlyResponsesJSONFanout(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		fanMockTurn(w,
			`{"index":0,"id":"call_r1","type":"function","function":{"name":"read_files","arguments":`+strconv.Quote(fanReadArgs)+`}}`)
	}
	ts, _ := newTestServer(t, nil, mock)
	body := `{"model":"` + modelA + `","input":[{"role":"user","content":[{"type":"input_text","text":"read two"}]}],` +
		`"tools":` + responsesFloorTools() + `,"stream":false}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	output, _ := obj["output"].([]any)
	var calls []map[string]any
	for _, o := range output {
		om, _ := o.(map[string]any)
		if om["type"] == "function_call" {
			calls = append(calls, om)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("function_call items = %d, want 2: %s", len(calls), truncate(string(data), 400))
	}
	for i, wantPath := range []string{"a.go", "b.go"} {
		if calls[i]["name"] != "read" {
			t.Errorf("call[%d] name = %v, want read", i, calls[i]["name"])
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(calls[i]["arguments"].(string)), &args); err != nil {
			t.Fatalf("call[%d] arguments not JSON: %v", i, err)
		}
		if args["path"] != wantPath || len(args) != 1 {
			t.Errorf("call[%d] arguments = %v, want {path:%s}", i, args, wantPath)
		}
	}
	if calls[0]["call_id"] == calls[1]["call_id"] {
		t.Errorf("fan-out calls share call_id %v (must be unique)", calls[0]["call_id"])
	}
}
