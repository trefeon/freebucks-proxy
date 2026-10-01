package server

// A stream that ends without a terminal finish_reason must still deliver the
// tool arguments the reshape buffer withheld. Field failure (live): the
// upstream closed mid-turn for deepseek-v4-flash, the client received a tool
// call whose argument bytes were never re-injected, and the turn surfaced as
// "The model did not complete a usable response" instead of running the call.

import (
	"encoding/json"
	"freebuff-proxy/backend/internal/convert"
	"testing"
)

// ompFloorBody declares two OMP-signature tools (eval + learn) so the request
// classifies as the OMP family and the wire goes floor-only, exactly like a
// real OMP turn.
func ompFloorBody() []byte {
	return []byte(`{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"go"}],` +
		`"tools":[` +
		`{"type":"function","function":{"name":"eval","description":"e","parameters":{"type":"object"}}},` +
		`{"type":"function","function":{"name":"learn","description":"l","parameters":{"type":"object"}}}` +
		`]}`)
}

// toolCallFragment renders one upstream delta carrying a tool call's name on
// the first fragment and argument bytes afterwards.
func toolCallFragment(id, wire, frag string) map[string]any {
	fn := map[string]any{}
	if wire != "" {
		fn["name"] = wire
	}
	if frag != "" {
		fn["arguments"] = frag
	}
	return map[string]any{
		"id":      "chatcmpl-die",
		"object":  "chat.completion.chunk",
		"created": 1,
		"model":   "m",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"tool_calls": []any{map[string]any{
				"index":    float64(0),
				"id":       id,
				"type":     "function",
				"function": fn,
			}}},
		}},
	}
}

// TestFlushReshapeTerminalDeliversWithheldArgs pins the recovery: the
// synthetic terminal chunk carries the reshaped whole under the client name,
// so a call whose stream died after the arguments arrived is still usable.
func TestFlushReshapeTerminalDeliversWithheldArgs(t *testing.T) {
	_, mapper, err := convert.NormalizeRequestMapped(ompFloorBody(), "")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !mapper.FloorOnly() {
		t.Fatal("request did not classify as the OMP floor family")
	}

	rw := newChunkRewriter(&relayStats{toolMap: mapper})
	rw.streamModel = "deepseek/deepseek-v4-flash"
	rw.xmlStreamID = "chatcmpl-die"

	// The model streams the CLI-shaped read in fragments; the rewriter
	// withholds every argument byte for the reshape rule.
	first := toolCallFragment("call_1", "read_files", `{"paths":["a.go"]}`)
	if !rw.reshapeBuffer(first) {
		t.Fatal("reshapeBuffer withheld nothing for read_files")
	}
	args := first["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if _, ok := args["arguments"]; ok {
		t.Fatal("arguments were relayed instead of withheld")
	}

	chunk, ok, complete := rw.flushReshapeTerminal(rw.xmlStreamID, rw.streamModel)
	if !ok || chunk == nil {
		t.Fatal("flushReshapeTerminal dropped the withheld call")
	}
	if !complete {
		t.Error("complete = false, want true for whole JSON arguments")
	}
	raw, err := json.Marshal(chunk)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	choice := out["choices"].([]any)[0].(map[string]any)
	if fr, _ := choice["finish_reason"].(string); fr != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", fr)
	}
	calls, _ := choice["delta"].(map[string]any)["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("injected calls = %v, want exactly 1", calls)
	}
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "read" {
		t.Errorf("restored name = %v, want the client's own tool name read", fn["name"])
	}
	if fn["arguments"] != `{"path":"a.go"}` {
		t.Errorf("arguments = %v, want the reshaped OMP shape", fn["arguments"])
	}
}

// TestFlushReshapeTerminalReportsTruncation keeps the honest failure: bytes
// cut mid-JSON must not be reported as a whole call.
func TestFlushReshapeTerminalReportsTruncation(t *testing.T) {
	_, mapper, err := convert.NormalizeRequestMapped(ompFloorBody(), "")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	rw := newChunkRewriter(&relayStats{toolMap: mapper})
	rw.reshapeBuffer(toolCallFragment("call_1", "read_files", `{"paths":["a.go`))

	if _, ok, complete := rw.flushReshapeTerminal("chatcmpl-die", "m"); !ok || complete {
		t.Errorf("(ok, complete) = (%v, %v), want (true, false) for truncated JSON", ok, complete)
	}
}

// TestFlushReshapeTerminalNoopWithoutBuffer: streams that never withheld
// anything must stay byte-identical (no synthetic chunk invented).
func TestFlushReshapeTerminalNoopWithoutBuffer(t *testing.T) {
	_, mapper, err := convert.NormalizeRequestMapped(ompFloorBody(), "")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	rw := newChunkRewriter(&relayStats{toolMap: mapper})
	if chunk, ok, _ := rw.flushReshapeTerminal("chatcmpl-die", "m"); ok || chunk != nil {
		t.Errorf("empty buffer produced a chunk: %v", chunk)
	}
}
