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

// Phase 3 streaming parity (UNIVERSAL-GATEWAY-PLAN §§3.1/3.2): OMP/pi-family
// turns on the Anthropic + Responses streaming translators must reshape,
// fan out, and degrade exactly like the whole-args legs — CLI-shaped bytes
// never reach the client, multi-path reads / multi-replacement edits expand,
// unroutable calls render as text (or absorb), and generic families stay
// byte-identical live streams.
//
// Chunk shape note: every tool_calls chunk below ends its delta as
// ...ARGS}}]}, — function}, call}, array], delta}, — then the terminal
// ,"finish_reason". Argument fragments are embedded with strconv.Quote so
// no hand-escaped JSON can unbalance the envelope again.

// phase3ToolBlockStarts returns (index, name, id) for every tool_use
// content_block_start in Anthropic stream order.
func phase3ToolBlockStarts(events []map[string]any) [][3]string {
	var out [][3]string
	for _, ev := range events {
		if ev["type"] != "content_block_start" {
			continue
		}
		idx, _ := ev["index"].(float64)
		block, _ := ev["content_block"].(map[string]any)
		if block["type"] != "tool_use" {
			continue
		}
		name, _ := block["name"].(string)
		id, _ := block["id"].(string)
		out = append(out, [3]string{strconv.Itoa(int(idx)), name, id})
	}
	return out
}

// phase3BlockDeltas joins the input_json_delta bytes for one Anthropic
// block index, in order.
func phase3BlockDeltas(events []map[string]any, idx int) (joined string, count int) {
	var sb strings.Builder
	for _, ev := range events {
		if ev["type"] != "content_block_delta" {
			continue
		}
		evIdx, _ := ev["index"].(float64)
		if int(evIdx) != idx {
			continue
		}
		delta, _ := ev["delta"].(map[string]any)
		if delta["type"] != "input_json_delta" {
			continue
		}
		if frag, _ := delta["partial_json"].(string); frag != "" {
			sb.WriteString(frag)
			count++
		}
	}
	return sb.String(), count
}

// phase3ToolChunk renders one upstream chat chunk carrying delta tool_calls
// entries (already-braced call JSON) plus an optional terminal
// finish_reason/usage tail.
func phase3ToolChunk(id string, calls string) string {
	return testutil.SSEEvent(chunk(id, 1,
		`"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[`+calls+`]},"finish_reason":null}]`))
}

// phase3Call renders one delta tool_calls entry: a complete call (id + wire
func phase3Call(index int, id, wire, argsOrFrag string, complete bool) string {
	if complete {
		return `{"index":` + strconv.Itoa(index) + `,"id":` + strconv.Quote(id) +
			`,"type":"function","function":{"name":` + strconv.Quote(wire) +
			`,"arguments":` + strconv.Quote(argsOrFrag) + `}}`
	}
	return `{"index":` + strconv.Itoa(index) + `,"function":{"arguments":` + strconv.Quote(argsOrFrag) + `}}`
}

// TestPhase3OMPAnthropicStreamFanout replays an OMP floor turn on the
// STREAMING Anthropic surface with CLI-shaped args split across fragments:
// the client must see per-path read + per-replacement edit tool_use blocks
// in OMP shape, each delivered as exactly one whole delta (withhold proof —
// no live CLI-shaped fragment), with unique ids and a tool_use stop.
func TestPhase3OMPAnthropicStreamFanout(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, phase3ToolChunk("cmpl-p3a",
			phase3Call(0, "call_o1", "read_files", `{"paths":["a.go"`, true)))
		_, _ = io.WriteString(w, phase3ToolChunk("cmpl-p3a",
			phase3Call(0, "", "", `,"b.go"]}`, false)+`,`+
				phase3Call(1, "call_o2", "str_replace", `{"path":"f"`, true)))
		_, _ = io.WriteString(w, phase3ToolChunk("cmpl-p3a",
			phase3Call(1, "", "", `,"replacements":[{"oldString":"a","newString":"b"}`, false)))
		_, _ = io.WriteString(w, phase3ToolChunk("cmpl-p3a",
			phase3Call(1, "", "", `,{"oldString":"c","newString":"d"}]}`, false)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-p3a", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":60,"completion_tokens":20,"total_tokens":80}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServer(t, nil, mock)
	body := `{"model":"` + modelA + `","max_tokens":256,"messages":[{"role":"user","content":"do it"}],"tools":` + anthropicFloorTools() + `,"stream":true}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body),
		map[string]string{"Content-Type": "application/json", "anthropic-version": "2023-06-01"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	assertFloorOnlyWire(t, mock)
	s := string(data)
	for _, leaked := range []string{`"paths"`, `"replacements"`, `timeout_seconds`} {
		if strings.Contains(s, leaked) {
			t.Errorf("CLI-shaped arg key %s reached the client stream: %s", leaked, truncate(s, 400))
		}
	}
	events := collectAnthropicEvents(t, s)
	starts := phase3ToolBlockStarts(events)
	if len(starts) != 4 {
		t.Fatalf("tool_use blocks = %d, want 4 (read x2 + edit x2): %s", len(starts), truncate(s, 500))
	}
	wantNames := []string{"read", "read", "edit", "edit"}
	wantArgs := []string{`{"path":"a.go"}`, `{"path":"b.go"}`, `{"path":"f","old_string":"a","new_string":"b"}`, `{"path":"f","old_string":"c","new_string":"d"}`}
	seenIDs := map[string]bool{}
	for i, st := range starts {
		idx, _ := strconv.Atoi(st[0])
		if st[1] != wantNames[i] {
			t.Errorf("block[%d] name = %q, want %s", i, st[1], wantNames[i])
		}
		if st[2] == "" {
			t.Errorf("block[%d] missing id (every streamed tool_use needs one)", i)
		}
		if seenIDs[st[2]] {
			t.Errorf("block[%d] reuses id %q (fan-out ids must be unique)", i, st[2])
		}
		seenIDs[st[2]] = true
		joined, n := phase3BlockDeltas(events, idx)
		if n != 1 {
			t.Errorf("block[%d] deltas = %d, want exactly 1 whole (withhold proof)", i, n)
		}
		var gotM, wantM map[string]any
		_ = json.Unmarshal([]byte(joined), &gotM)
		_ = json.Unmarshal([]byte(wantArgs[i]), &wantM)
		gotB, _ := json.Marshal(gotM)
		wantB, _ := json.Marshal(wantM)
		if string(gotB) != string(wantB) {
			t.Errorf("block[%d] input = %s, want %s", i, gotB, wantB)
		}
	}
	if stop, _ := replayMessageDelta(events); stop != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", stop)
	}
	if last := events[len(events)-1]; last["type"] != "message_stop" {
		t.Errorf("last event = %v, want message_stop", last["type"])
	}
}

// TestPhase3AnthropicStreamAbsorbOnly pins the degrade edge: a turn whose
// only call is absorbed telemetry (report_project_profile) emits no
// tool_use block and finalizes end_turn — never a tool_use stop with zero
// blocks.
func TestPhase3AnthropicStreamAbsorbOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, phase3ToolChunk("cmpl-p3b",
			phase3Call(0, "call_t1", "report_project_profile", `{"status":"x"}`, true)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-p3b", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServer(t, nil, mock)
	body := `{"model":"` + modelA + `","max_tokens":256,"messages":[{"role":"user","content":"go"}],"tools":` + anthropicFloorTools() + `,"stream":true}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body),
		map[string]string{"Content-Type": "application/json", "anthropic-version": "2023-06-01"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	s := string(data)
	if strings.Contains(s, "report_project_profile") {
		t.Errorf("absorbed call name reached the client: %s", truncate(s, 300))
	}
	events := collectAnthropicEvents(t, s)
	if starts := phase3ToolBlockStarts(events); len(starts) != 0 {
		t.Errorf("tool_use blocks = %d, want 0 (absorbed): %v", len(starts), starts)
	}
	if stop, _ := replayMessageDelta(events); stop != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn (no dispatchable call)", stop)
	}
	if last := events[len(events)-1]; last["type"] != "message_stop" {
		t.Errorf("last event = %v, want message_stop", last["type"])
	}
}
