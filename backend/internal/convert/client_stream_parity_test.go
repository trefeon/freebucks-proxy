package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

// OMP + pi request bodies triggering each family. OMP via intent-i (chat
// shape); pi via the batch-edit fingerprint (pi declares read+edit core).
const (
	parityOMPBody = `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[` +
		`{"type":"function","function":{"name":"read","description":"r","parameters":{"type":"object","properties":{"path":{"type":"string"},"i":{"type":"string"}}}}},` +
		`{"type":"function","function":{"name":"edit","description":"e","parameters":{"type":"object","properties":{"path":{"type":"string"},"i":{"type":"string"}}}}},` +
		`{"type":"function","function":{"name":"eval","description":"v","parameters":{"type":"object","properties":{"language":{"type":"string"}}}}},` +
		`{"type":"function","function":{"name":"learn","description":"l","parameters":{"type":"object","properties":{"memory":{"type":"string"}}}}}]}`

	parityPiBody = `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[` +
		`{"type":"function","function":{"name":"read","description":"r","parameters":{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"number"},"limit":{"type":"number"}}}}},` +
		`{"type":"function","function":{"name":"edit","description":"e","parameters":{"type":"object","properties":{"path":{"type":"string"},"edits":{"type":"array","items":{"type":"object","properties":{"oldText":{"type":"string"},"newText":{"type":"string"}}}}}}}}]}`

	parityGenericBody = `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[` +
		`{"type":"function","function":{"name":"bash","description":"b","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}}]}`

	parityReadArgs = `{"paths":["a.go","b.go"]}`
	parityEditArgs = `{"path":"f.go","replacements":[{"oldString":"a","newString":"b"},{"oldString":"c","newString":"d"}]}`
	parityBashArgs = `{"command":"go test","timeout_seconds":120}`
	parityTodoArgs = `{"todos":[{"task":"a","completed":true},{"task":"b","completed":false}]}`
	parityAskArgs  = `{"questions":[{"question":"ok?","options":[{"label":"yes"}]}]}`
	parityTaskArgs = `{"agent":"researcher","task":"audit"}`
	parityFupArgs  = `{"followups":[{"prompt":"Ship it"}]}`
)

// nonStreamingResult runs one wire call through the non-streaming legs in
// order (fallback, reshape, restore) and returns the client calls plus any
// fallback text, mirroring what the chat/Anthropic/Responses JSON relays
// deliver.
func nonStreamingResult(t *testing.T, mapper ToolMapper, wire, id, args string) ([]StreamedCall, string) {
	t.Helper()
	comp := map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{map[string]any{
					"id":   id,
					"type": "function",
					"function": map[string]any{
						"name":      wire,
						"arguments": args,
					},
				}},
			},
			"finish_reason": "tool_calls",
		}},
	}
	mapper.ApplyTextFallbacks(comp)
	mapper.ReshapeCompletionCalls(comp)
	mapper.FromUpstreamChunk(comp)
	msg := comp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	var out []StreamedCall
	if tcs, ok := msg["tool_calls"].([]any); ok {
		for _, raw := range tcs {
			tc := raw.(map[string]any)
			fn := tc["function"].(map[string]any)
			tcID, _ := tc["id"].(string)
			fnName, _ := fn["name"].(string)
			fnArgs, _ := fn["arguments"].(string)
			out = append(out, StreamedCall{ID: tcID, Name: fnName, Arguments: fnArgs})
		}
	}
	content, _ := msg["content"].(string)
	return out, content
}

// parityMapper classifies a chat-shaped request body through the full
// request leg (NewToolMapper classifies nothing on its own — family +
// reverse maps come from NormalizeRequestMappedOpts).
func parityMapper(t *testing.T, body string) ToolMapper {
	t.Helper()
	_, mapper, err := NormalizeRequestMappedOpts([]byte(body), "", DefaultOptions())
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	return mapper
}

// TestFinalizeStreamedCallParity pins the streaming finalize helper to the
// non-streaming legs: for every (family, wire, args) row the finalized
// streaming calls + text must equal what ApplyTextFallbacks →
// ReshapeCompletionCalls → FromUpstreamChunk deliver for the same wire
// call. Any drift between the streaming and whole-args legs fails here,
// not in a client.
func TestFinalizeStreamedCallParity(t *testing.T) {
	rows := []struct {
		wire string
		args string
	}{
		{"read_files", parityReadArgs},
		{"str_replace", parityEditArgs},
		{"run_terminal_command", parityBashArgs},
		{"write_todos", parityTodoArgs},
		{"ask_user", parityAskArgs},
		{"task", parityTaskArgs},
		{"mcp__task", parityTaskArgs},
		{"suggest_followups", parityFupArgs},
		{"report_project_profile", `{"status":"x"}`},
		{"read_files", `{"paths":[]}`},       // empty dump: no rule output
		{"read_files", `{"paths":["a.go"`},   // truncated: verbatim
		{"read_files", ``},                   // empty: verbatim
		{"list_directory", `{"path":"."}`},   // unruled: verbatim
		{"advise", `{"note":"nit"}`},         // OMP verbatim name
		{"mcp__spawn_agent", `{"task":"x"}`}, // pi extension verbatim
	}
	families := []struct {
		name string
		body string
	}{
		{"omp", parityOMPBody},
		{"pi", parityPiBody},
		{"generic", parityGenericBody},
	}
	for _, fam := range families {
		t.Run(fam.name, func(t *testing.T) {
			mapper := parityMapper(t, fam.body)
			for _, row := range rows {
				wantCalls, wantText := nonStreamingResult(t, mapper, row.wire, "call_1", row.args)
				gotCalls, gotText, absorb := mapper.FinalizeStreamedCall(row.wire, "call_1", row.args)
				if absorb && (len(wantCalls) > 0 || wantText != "") {
					t.Errorf("%s %q: streaming absorbed but non-streaming delivered calls=%v text=%q",
						fam.name, row.wire, wantCalls, wantText)
					continue
				}
				if !absorb && len(wantCalls) == 0 && wantText == "" {
					t.Errorf("%s %q: non-streaming dropped the call but streaming delivered calls=%v text=%q",
						fam.name, row.wire, gotCalls, gotText)
					continue
				}
				if absorb {
					continue
				}
				if gotText != wantText {
					t.Errorf("%s %q: text = %q, want %q", fam.name, row.wire, gotText, wantText)
				}
				if len(gotCalls) != len(wantCalls) {
					t.Errorf("%s %q: calls = %v, want %v", fam.name, row.wire, gotCalls, wantCalls)
					continue
				}
				for i := range gotCalls {
					if gotCalls[i] != wantCalls[i] {
						t.Errorf("%s %q call[%d] = %+v, want %+v",
							fam.name, row.wire, i, gotCalls[i], wantCalls[i])
					}
				}
			}
		})
	}
}

// TestWithholdStreamArgsGate pins the buffer gate: ruled + fallback wires
// withhold on rewriting families, everything else (generic family,
// unruled names, end_turn/decide pins) streams live.
func TestWithholdStreamArgsGate(t *testing.T) {
	omp := parityMapper(t, parityOMPBody)
	if !omp.ResponseRewrite() {
		t.Fatal("OMP body lost its rewrite family")
	}
	for _, wire := range []string{
		"read_files", "str_replace", "run_terminal_command", "write_file",
		"code_search", "glob", "find", "write_todos", "web_search", "ask_user",
		"read_url", "skill", "gravity_index", "task", "mcp__task",
		"suggest_followups", "render_ui", "report_project_profile",
	} {
		if !omp.WithholdStreamArgs(wire) {
			t.Errorf("OMP %q does not withhold", wire)
		}
	}
	for _, wire := range []string{"end_turn", "decide", "advise", "list_directory", "eval"} {
		if omp.WithholdStreamArgs(wire) {
			t.Errorf("OMP %q withholds but has no rule or fallback", wire)
		}
	}
	pi := parityMapper(t, parityPiBody)
	if pi.FloorOnly() || !pi.ResponseRewrite() {
		t.Fatal("pi body misclassified")
	}
	for _, wire := range []string{"read_files", "str_replace", "write_file", "run_terminal_command", "code_search", "glob"} {
		if !pi.WithholdStreamArgs(wire) {
			t.Errorf("pi %q does not withhold", wire)
		}
	}
	// pi list_directory restores verbatim ({path} is already pi shape: no
	// rule, no fallback) so it streams live like an extension tool.
	if pi.WithholdStreamArgs("list_directory") {
		t.Error("pi list_directory withholds (must stream live)")
	}
	// pi find dispatches its own vocabulary (no rule on this family), so it
	// streams live alongside the other unruled names.
	if pi.WithholdStreamArgs("find") {
		t.Error("pi find withholds (must stream live)")
	}
	if pi.WithholdStreamArgs("mcp__spawn_agent") {
		t.Error("pi extension mcp__spawn_agent withholds (must stream live)")
	}
	gen := parityMapper(t, parityGenericBody)
	if gen.ResponseRewrite() {
		t.Fatal("generic body gained a rewrite family")
	}
	for _, wire := range []string{"run_terminal_command", "read_files", "suggest_followups"} {
		if gen.WithholdStreamArgs(wire) {
			t.Errorf("generic %q withholds (must stay byte-identical live)", wire)
		}
	}
	if !strings.Contains(parityOMPBody, `"i"`) {
		t.Error("OMP parity body lost its intent-i trigger")
	}
}

// TestFinalizeStreamedCallFanoutIDs pins unique dispatch ids across fan-out
// (the client echoes results per id).
func TestFinalizeStreamedCallFanoutIDs(t *testing.T) {
	omp := parityMapper(t, parityOMPBody)
	calls, _, absorb := omp.FinalizeStreamedCall("read_files", "call_9", parityReadArgs)
	if absorb || len(calls) != 2 {
		t.Fatalf("calls = %v absorb = %v, want 2", calls, absorb)
	}
	if calls[0].ID != "call_9" || calls[1].ID != "call_9-fanout-1" {
		t.Errorf("ids = %q %q, want call_9 + call_9-fanout-1", calls[0].ID, calls[1].ID)
	}
	if calls[0].Name != "read" || calls[1].Name != "read" {
		t.Errorf("names = %q %q, want read/read", calls[0].Name, calls[1].Name)
	}
	var args0, args1 map[string]any
	_ = json.Unmarshal([]byte(calls[0].Arguments), &args0)
	_ = json.Unmarshal([]byte(calls[1].Arguments), &args1)
	if args0["path"] != "a.go" || args1["path"] != "b.go" {
		t.Errorf("args = %v %v, want per-path fan-out", args0, args1)
	}
}
