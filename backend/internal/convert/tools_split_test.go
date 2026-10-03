package convert

import (
	"encoding/json"
	"testing"
)

// Live regression: one arguments string carrying two complete JSON objects
// back to back. The response leg must deliver both operations, never decode
// the first and silently drop the second.
const splitRegressionArgs = `{"path":"skill://using-superpowers"}{"path":"memory://root/memory_summary.md"}`

func splitMapper() ToolMapper {
	return ToolMapper{
		family:           familyOMP,
		upstreamToClient: map[string]string{"read_files": "read", "str_replace": "edit"},
	}
}

func TestSplitConcatenatedJSONValues(t *testing.T) {
	t.Run("two-way regression", func(t *testing.T) {
		parts, ok := SplitConcatenatedJSONValues(splitRegressionArgs)
		if !ok {
			t.Fatal("split = false, want 2 parts")
		}
		if len(parts) != 2 {
			t.Fatalf("parts = %d, want 2", len(parts))
		}
		if parts[0] != `{"path":"skill://using-superpowers"}` || parts[1] != `{"path":"memory://root/memory_summary.md"}` {
			t.Fatalf("parts = %q, want the two exact objects", parts)
		}
		for i, p := range parts {
			if !json.Valid([]byte(p)) {
				t.Errorf("part[%d] is not valid JSON: %q", i, p)
			}
		}
	})
	t.Run("three-way", func(t *testing.T) {
		parts, ok := SplitConcatenatedJSONValues(`{"path":"a"}{"path":"b"}{"path":"c"}`)
		if !ok || len(parts) != 3 {
			t.Fatalf("split = (%v, %d), want (true, 3)", ok, len(parts))
		}
		for i, want := range []string{`{"path":"a"}`, `{"path":"b"}`, `{"path":"c"}`} {
			if parts[i] != want {
				t.Errorf("part[%d] = %q, want %q", i, parts[i], want)
			}
		}
	})
	t.Run("whitespace between values still splits", func(t *testing.T) {
		parts, ok := SplitConcatenatedJSONValues(" \n" + `{"a":1}` + "  \t" + `{"b":2}` + "\n")
		if !ok || len(parts) != 2 {
			t.Fatalf("split = (%v, %d), want (true, 2)", ok, len(parts))
		}
	})
	t.Run("braces inside strings do not confuse the split", func(t *testing.T) {
		in := `{"a":"}{"}{"b":2}`
		parts, ok := SplitConcatenatedJSONValues(in)
		if !ok || len(parts) != 2 {
			t.Fatalf("split = (%v, %d), want (true, 2)", ok, len(parts))
		}
		if parts[0] != `{"a":"}{"}` || parts[1] != `{"b":2}` {
			t.Fatalf("parts = %q, want exact reslice", parts)
		}
	})
	for _, tc := range []struct {
		name string
		in   string
	}{
		{"single object unchanged", `{"path":"a"}`},
		{"single array unchanged", `["a","b"]`},
		{"single primitive unchanged", `42`},
		{"empty unchanged", ``},
		{"whitespace-only unchanged", "  \n\t "},
		{"truncated second value unchanged", `{"path":"a"}{"path":`},
		{"trailing garbage unchanged", `{"a":1} garbage`},
		{"leading garbage unchanged", `garbage{"a":1}`},
		{"unclosed first value unchanged", `{"a":1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if parts, ok := SplitConcatenatedJSONValues(tc.in); ok || parts != nil {
				t.Fatalf("split(%q) = (%q, true), want (nil, false) untouched", tc.in, parts)
			}
		})
	}
}

func TestReshapeArgsFanoutSplit(t *testing.T) {
	m := splitMapper()
	// Each split object still receives the family reshape: CLI-shaped parts
	// reshape per part, not just the first.
	bodies, ok := m.ReshapeArgsFanout("read_files", `{"paths":["a.go"]}{"paths":["b.go"]}`)
	if !ok || len(bodies) != 2 {
		t.Fatalf("fanout = (%v, %d), want (true, 2)", ok, len(bodies))
	}
	if bodies[0] != `{"path":"a.go"}` || bodies[1] != `{"path":"b.go"}` {
		t.Fatalf("bodies = %q, want reshaped per part", bodies)
	}
	// Already-OMP-shaped glued parts ride verbatim — never dropped, never
	// corrupted into a blanked shape.
	bodies, ok = m.ReshapeArgsFanout("read_files", splitRegressionArgs)
	if !ok || len(bodies) != 2 {
		t.Fatalf("fanout = (%v, %d), want (true, 2)", ok, len(bodies))
	}
	for i, want := range []string{`{"path":"skill://using-superpowers"}`, `{"path":"memory://root/memory_summary.md"}`} {
		if bodies[i] != want {
			t.Errorf("body[%d] = %q, want %q", i, bodies[i], want)
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(bodies[i]), &v); err != nil {
			t.Errorf("body[%d] invalid JSON: %v", i, err)
		}
	}
	// Single and malformed inputs keep the legacy verdicts.
	if bodies, ok := m.ReshapeArgsFanout("read_files", `{"paths":["a.go"]}`); !ok || len(bodies) != 1 {
		t.Errorf("single CLI shape fanout = (%v, %d), want (true, 1)", ok, len(bodies))
	}
	// Truncated glue keeps the legacy path: the first complete value still
	// reshapes, the incomplete tail is dropped exactly as before.
	if bodies, ok := m.ReshapeArgsFanout("read_files", `{"paths":["a.go"]}{`); !ok || len(bodies) != 1 || bodies[0] != `{"path":"a.go"}` {
		t.Errorf("truncated glue fanout = (%v, %q), want (true, [{path:a.go}])", ok, bodies)
	}
	if parts, ok := (ToolMapper{}).ReshapeArgsFanout("read_files", splitRegressionArgs); ok || parts != nil {
		t.Error("non-floor mapper split, want passthrough")
	}
}

func mkSplitCall(id, wire, args string) any {
	return map[string]any{
		"id":   id,
		"type": "function",
		"function": map[string]any{
			"name":      wire,
			"arguments": args,
		},
	}
}

func TestReshapeMessageCallsSplit(t *testing.T) {
	m := splitMapper()
	t.Run("wire name splits into two valid calls with distinct ids", func(t *testing.T) {
		expanded, changed := m.ReshapeMessageCalls([]any{mkSplitCall("call_1", "read_files", splitRegressionArgs)})
		if !changed {
			t.Fatal("changed = false, want split")
		}
		if len(expanded) != 2 {
			t.Fatalf("calls = %d, want 2", len(expanded))
		}
		for i, want := range []struct{ id, args string }{
			{"call_1", `{"path":"skill://using-superpowers"}`},
			{"call_1-fanout-1", `{"path":"memory://root/memory_summary.md"}`},
		} {
			tc := expanded[i].(map[string]any)
			if tc["id"] != want.id {
				t.Errorf("call[%d] id = %v, want %s", i, tc["id"], want.id)
			}
			fn := tc["function"].(map[string]any)
			if fn["arguments"] != want.args {
				t.Errorf("call[%d] args = %v, want %s", i, fn["arguments"], want.args)
			}
			var v map[string]any
			if err := json.Unmarshal([]byte(fn["arguments"].(string)), &v); err != nil {
				t.Errorf("call[%d] args invalid JSON: %v", i, err)
			}
		}
	})
	t.Run("OMP-vocabulary name splits verbatim", func(t *testing.T) {
		expanded, changed := m.ReshapeMessageCalls([]any{mkSplitCall("call_2", "read", splitRegressionArgs)})
		if !changed || len(expanded) != 2 {
			t.Fatalf("split = (%v, %d), want (true, 2)", changed, len(expanded))
		}
	})
	t.Run("three-way split", func(t *testing.T) {
		expanded, changed := m.ReshapeMessageCalls([]any{mkSplitCall("call_3", "read", `{"path":"a"}{"path":"b"}{"path":"c"}`)})
		if !changed || len(expanded) != 3 {
			t.Fatalf("split = (%v, %d), want (true, 3)", changed, len(expanded))
		}
		if id := expanded[2].(map[string]any)["id"]; id != "call_3-fanout-2" {
			t.Errorf("call[2] id = %v, want call_3-fanout-2", id)
		}
	})
	t.Run("single JSON unchanged", func(t *testing.T) {
		tcs := []any{mkSplitCall("call_4", "read", `{"path":"a"}`)}
		if expanded, changed := m.ReshapeMessageCalls(tcs); changed || len(expanded) != 1 {
			t.Fatalf("single = (%v, %d), want (false, 1)", changed, len(expanded))
		}
	})
	t.Run("malformed unchanged", func(t *testing.T) {
		tcs := []any{mkSplitCall("call_5", "read", `{"path":"a"}{`)}
		if expanded, changed := m.ReshapeMessageCalls(tcs); changed || len(expanded) != 1 {
			t.Fatalf("malformed = (%v, %d), want (false, 1)", changed, len(expanded))
		}
	})
	t.Run("non-floor unchanged", func(t *testing.T) {
		tcs := []any{mkSplitCall("call_6", "read_files", splitRegressionArgs)}
		if expanded, changed := (ToolMapper{}).ReshapeMessageCalls(tcs); changed || len(expanded) != 1 {
			t.Fatalf("non-floor = (%v, %d), want (false, 1)", changed, len(expanded))
		}
	})
}

// TestReshapeCompletionCallsSplitRestore pins the whole non-streaming leg:
// split, then name restoration — the order every JSON relay runs.
func TestReshapeCompletionCallsSplitRestore(t *testing.T) {
	m := splitMapper()
	comp := completionWith(
		`[{"id":"call_1","type":"function","function":{"name":"read_files","arguments":"`+jsonEscape(splitRegressionArgs)+`"}}]`,
		"tool_calls")
	if !m.ReshapeCompletionCalls(comp) {
		t.Fatal("ReshapeCompletionCalls reported no change, want split")
	}
	m.FromUpstreamChunk(comp)
	msg := comp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	tcs, ok := msg["tool_calls"].([]any)
	if !ok || len(tcs) != 2 {
		t.Fatalf("tool_calls = %v, want 2 calls", msg["tool_calls"])
	}
	for i, want := range []struct{ id, name, args string }{
		{"call_1", "read", `{"path":"skill://using-superpowers"}`},
		{"call_1-fanout-1", "read", `{"path":"memory://root/memory_summary.md"}`},
	} {
		tc := tcs[i].(map[string]any)
		fn := tc["function"].(map[string]any)
		if tc["id"] != want.id || fn["name"] != want.name || fn["arguments"] != want.args {
			t.Errorf("call[%d] = (%v, %v, %v), want (%s, %s, %s)",
				i, tc["id"], fn["name"], fn["arguments"], want.id, want.name, want.args)
		}
	}
}

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	out := string(b)
	return out[1 : len(out)-1]
}

// TestFinalizeStreamedCallSplitParity pins the Anthropic/Responses streaming
// leg to the non-streaming legs for glued arguments: one glued upstream call
// finalizes to exactly what ApplyTextFallbacks -> ReshapeCompletionCalls ->
// FromUpstreamChunk deliver — never a dropped second operation.
func TestFinalizeStreamedCallSplitParity(t *testing.T) {
	m := parityMapper(t, parityOMPBody)
	streamed, text, absorb := m.FinalizeStreamedCall("read_files", "call_1", splitRegressionArgs)
	if absorb || text != "" {
		t.Fatalf("finalize = (absorb=%v, text=%q), want dispatchable calls", absorb, text)
	}
	if len(streamed) != 2 {
		t.Fatalf("finalized calls = %d, want 2", len(streamed))
	}
	whole, wholeText := nonStreamingResult(t, m, "read_files", "call_1", splitRegressionArgs)
	if wholeText != "" || len(whole) != len(streamed) {
		t.Fatalf("non-streaming = (%d calls, %q), want parity with streaming", len(whole), wholeText)
	}
	for i := range whole {
		if whole[i] != streamed[i] {
			t.Errorf("call[%d] = %+v, want %+v (streaming/non-streaming drift)", i, streamed[i], whole[i])
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(streamed[i].Arguments), &v); err != nil {
			t.Errorf("call[%d] arguments invalid JSON: %v", i, err)
		}
	}
	// Truncated glue still relays verbatim on both legs (never swallowed).
	single, _, _ := m.FinalizeStreamedCall("read_files", "call_9", `{"path":"a"}{`)
	if len(single) != 1 || single[0].Arguments != `{"path":"a"}{` {
		t.Errorf("truncated glue finalize = %+v, want verbatim passthrough", single)
	}
}
