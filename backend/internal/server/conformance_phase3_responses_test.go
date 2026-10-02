package server_test

import (
	"encoding/json"
	"freebuff-proxy/backend/internal/testutil"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Phase 3 streaming parity on the Responses surface: pi reshape + OMP
// fallback through the withhold/finalize path (shared builders live in
// conformance_phase3_streaming_test.go).

// phase3ResponsesDeltas joins the function_call_arguments.delta bytes for
// one Responses item id, in order, and counts them.
func phase3ResponsesDeltas(events []map[string]any, itemID string) (joined string, count int) {
	var sb strings.Builder
	for _, ev := range events {
		if ev["type"] != "response.function_call_arguments.delta" {
			continue
		}
		if id, _ := ev["item_id"].(string); id != itemID {
			continue
		}
		if d, _ := ev["delta"].(string); d != "" {
			sb.WriteString(d)
			count++
		}
	}
	return sb.String(), count
}

// phase3ResponsesDoneItems returns the function_call items of every
// response.output_item.done event, in order.
func phase3ResponsesDoneItems(events []map[string]any) []map[string]any {
	var out []map[string]any
	for _, ev := range events {
		if ev["type"] != "response.output_item.done" {
			continue
		}
		item, _ := ev["item"].(map[string]any)
		if item["type"] != "function_call" {
			continue
		}
		out = append(out, item)
	}
	return out
}

// phase3PiStreamTools renders pi's core read/edit/bash in the flat
// Responses shape, carrying the batch-edit fingerprint that identifies the
// pi family on this surface.
func phase3PiStreamTools() string {
	mk := func(name, schema string) string {
		return `{"type":"function","name":"` + name + `","description":"` + name + ` tool","parameters":` + schema + `}`
	}
	return `[` +
		mk("read", `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`) + `,` +
		mk("edit", `{"type":"object","properties":{"path":{"type":"string"},"edits":{"type":"array","items":{"type":"object","properties":{"oldText":{"type":"string"},"newText":{"type":"string"}}}}},"required":["path","edits"]}`) + `,` +
		mk("bash", `{"type":"object","properties":{"command":{"type":"string"},"timeout":{"type":"number"}},"required":["command"]}`) + `]`
}

// TestPhase3PiResponsesStreamReshape replays a pi turn on the STREAMING
// Responses surface with CLI-shaped args split across fragments: bash
// resolves to {command,timeout} and the multi-replacement edit to ONE
// batch edit — each as a single whole delta (withhold proof), with the
// terminal completed carrying the usage triple.
func TestPhase3PiResponsesStreamReshape(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, phase3ToolChunk("cmpl-p3c",
			phase3Call(0, "call_b1", "run_terminal_command", `{"command":"go test"`, true)))
		_, _ = io.WriteString(w, phase3ToolChunk("cmpl-p3c",
			phase3Call(0, "", "", `,"timeout_seconds":120}`, false)+`,`+
				phase3Call(1, "call_e1", "str_replace", `{"path":"f.go"`, true)))
		_, _ = io.WriteString(w, phase3ToolChunk("cmpl-p3c",
			phase3Call(1, "", "", `,"replacements":[{"oldString":"a","newString":"b"},{"oldString":"c","newString":"d"}]}`, false)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-p3c", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":30,"completion_tokens":20,"total_tokens":50}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServer(t, nil, mock)
	body := `{"model":"` + modelA + `","input":[{"role":"user","content":[{"type":"input_text","text":"do it"}]}],"stream":true,"tools":` + phase3PiStreamTools() + `}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	s := string(data)
	for _, leaked := range []string{`timeout_seconds`, `"paths"`, `"replacements"`, `"run_terminal_command"`, `"str_replace"`} {
		if strings.Contains(s, leaked) {
			t.Errorf("CLI-shaped payload %s reached the client stream: %s", leaked, truncate(s, 400))
		}
	}
	events := collectResponsesEvents(t, s)
	done := phase3ResponsesDoneItems(events)
	if len(done) != 2 {
		t.Fatalf("function_call done items = %d, want 2 (bash + ONE batch edit): %s", len(done), truncate(s, 500))
	}
	type wantCall struct {
		name, callID, args string
	}
	wants := []wantCall{
		{"bash", "call_b1", `{"command":"go test","timeout":120}`},
		{"edit", "call_e1", `{"path":"f.go","edits":[{"oldText":"a","newText":"b"},{"oldText":"c","newText":"d"}]}`},
	}
	for i, w := range wants {
		if done[i]["name"] != w.name {
			t.Errorf("item[%d] name = %v, want %s", i, done[i]["name"], w.name)
		}
		if done[i]["call_id"] != w.callID {
			t.Errorf("item[%d] call_id = %v, want %s", i, done[i]["call_id"], w.callID)
		}
		args, _ := done[i]["arguments"].(string)
		var gotM, wantM map[string]any
		_ = json.Unmarshal([]byte(args), &gotM)
		_ = json.Unmarshal([]byte(w.args), &wantM)
		gotB, _ := json.Marshal(gotM)
		wantB, _ := json.Marshal(wantM)
		if string(gotB) != string(wantB) {
			t.Errorf("item[%d] arguments = %s, want %s", i, gotB, wantB)
		}
		itemID, _ := done[i]["id"].(string)
		joined, n := phase3ResponsesDeltas(events, itemID)
		if n != 1 {
			t.Errorf("item[%d] deltas = %d, want exactly 1 whole (withhold proof)", i, n)
		}
		var joinedM map[string]any
		_ = json.Unmarshal([]byte(joined), &joinedM)
		joinedB, _ := json.Marshal(joinedM)
		if string(joinedB) != string(wantB) {
			t.Errorf("item[%d] streamed args = %s, want %s", i, joinedB, wantB)
		}
	}
	completed := codexEventByType(events, "response.completed")
	if completed == nil {
		t.Fatal("stream missing terminal response.completed")
	}
	completedResp, _ := completed["response"].(map[string]any)
	usage, _ := completedResp["usage"].(map[string]any)
	for _, key := range []string{"input_tokens", "output_tokens", "total_tokens"} {
		if _, ok := usage[key]; !ok {
			t.Errorf("completed usage missing %s: %v", key, usage)
		}
	}
	if types := eventTypes(events); types[len(types)-1] != "response.completed" {
		t.Errorf("last event = %q, want response.completed", types[len(types)-1])
	}
}

// TestPhase3OMPResponsesStreamFallback replays an OMP floor turn on the
// STREAMING Responses surface mixing a renderable call, an absorbed call,
// and a real call: the renderable call becomes output text, the absorbed
// call vanishes from the output entirely, and the real call fans out in
// OMP shape.
func TestPhase3OMPResponsesStreamFallback(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, phase3ToolChunk("cmpl-p3d",
			phase3Call(0, "call_f1", "suggest_followups", `{"followups":[{"prompt":"Ship it"}]}`, true)+`,`+
				phase3Call(1, "call_r1", "report_project_profile", `{"status":"x"}`, true)+`,`+
				phase3Call(2, "call_m1", "read_files", `{"paths":["a.go","b.go"]}`, true)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-p3d", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":10,"total_tokens":30}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServer(t, nil, mock)
	body := `{"model":"` + modelA + `","input":[{"role":"user","content":[{"type":"input_text","text":"go"}]}],"stream":true,"tools":` + responsesFloorTools() + `}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	assertFloorOnlyWire(t, mock)
	s := string(data)
	if strings.Contains(s, "report_project_profile") {
		t.Errorf("absorbed call name reached the client: %s", truncate(s, 300))
	}
	if strings.Contains(s, `"name":"suggest_followups"`) {
		t.Errorf("renderable call relayed as a function_call: %s", truncate(s, 300))
	}
	if !strings.Contains(s, "Suggested next steps") || !strings.Contains(s, "Ship it") {
		t.Errorf("rendered followups missing from output text: %s", truncate(s, 400))
	}
	events := collectResponsesEvents(t, s)
	done := phase3ResponsesDoneItems(events)
	if len(done) != 2 {
		t.Fatalf("function_call done items = %d, want 2 (read fan-out x2): %s", len(done), truncate(s, 500))
	}
	for i, wantPath := range []string{"a.go", "b.go"} {
		if done[i]["name"] != "read" {
			t.Errorf("item[%d] name = %v, want read", i, done[i]["name"])
		}
		var args map[string]any
		_ = json.Unmarshal([]byte(done[i]["arguments"].(string)), &args)
		if args["path"] != wantPath || len(args) != 1 {
			t.Errorf("item[%d] arguments = %v, want {path:%s}", i, args, wantPath)
		}
	}
	if done[0]["call_id"] == done[1]["call_id"] {
		t.Errorf("fan-out items share call_id %v (must be unique)", done[0]["call_id"])
	}
	if completed := codexEventByType(events, "response.completed"); completed == nil {
		t.Error("stream missing terminal response.completed")
	}
}
