package server

// Stream-leg regression for glued tool-call arguments: one buffered argument
// string carrying two complete JSON objects back to back must flush as two
// valid calls at fresh indexes, never decode-first-and-drop-the-rest.

import (
	"encoding/json"
	"freebuff-proxy/backend/internal/convert"
	"testing"
)

const splitStreamArgs = `{"path":"skill://using-superpowers"}{"path":"memory://root/memory_summary.md"}`

func splitStreamMapper(t *testing.T) convert.ToolMapper {
	t.Helper()
	_, mapper, err := convert.NormalizeRequestMapped(ompFloorBody(), "")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !mapper.FloorOnly() {
		t.Fatal("request did not classify as the OMP floor family")
	}
	return mapper
}

func splitTerminalChunk() map[string]any {
	return map[string]any{
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "tool_calls",
		}},
	}
}

func splitFlushedCalls(t *testing.T, chunk map[string]any) []any {
	t.Helper()
	delta, _ := chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	tcs, _ := delta["tool_calls"].([]any)
	return tcs
}

// toolCallFragmentAt is toolCallFragment (reshape_terminal_test.go) aimed at
// one upstream tool-call index: parallel calls in one turn ride distinct
// indexes the client SDK assembles separately.
func toolCallFragmentAt(id, wire, frag string, idx int) map[string]any {
	chunk := toolCallFragment(id, wire, frag)
	tcs := chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["tool_calls"].([]any)
	tcs[0].(map[string]any)["index"] = float64(idx)
	return chunk
}

func TestReshapeFlushSplitsGluedArgs(t *testing.T) {
	rw := newChunkRewriter(&relayStats{toolMap: splitStreamMapper(t)})
	frag := toolCallFragment("call_1", "read_files", splitStreamArgs)
	if !rw.reshapeBuffer(frag) {
		t.Fatal("reshapeBuffer withheld nothing for read_files")
	}
	// Glued bytes are whole values, not a truncation: the early-close
	// terminal still settles the turn on the split calls.
	chunk, ok, complete := rw.flushReshapeTerminal("chatcmpl-split", "m")
	if !ok || chunk == nil {
		t.Fatal("flushReshapeTerminal dropped the glued call")
	}
	if !complete {
		t.Error("complete = false, want true for concatenated whole JSON values")
	}
	tcs := splitFlushedCalls(t, chunk)
	if len(tcs) != 2 {
		t.Fatalf("injected calls = %d, want 2", len(tcs))
	}
	wants := []struct {
		idx  int
		name string
		args string
	}{
		{0, "read", `{"path":"skill://using-superpowers"}`},
		{1, "read", `{"path":"memory://root/memory_summary.md"}`},
	}
	for i, w := range wants {
		tc := tcs[i].(map[string]any)
		if idx, _ := tc["index"].(int); idx != w.idx {
			t.Errorf("call[%d] index = %v, want %d", i, tc["index"], w.idx)
		}
		fn := tc["function"].(map[string]any)
		if fn["name"] != w.name {
			t.Errorf("call[%d] name = %v, want %s (restored client name)", i, fn["name"], w.name)
		}
		if fn["arguments"] != w.args {
			t.Errorf("call[%d] arguments = %v, want %s", i, fn["arguments"], w.args)
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(fn["arguments"].(string)), &v); err != nil {
			t.Errorf("call[%d] arguments invalid JSON: %v", i, err)
		}
	}
	if tcs[0].(map[string]any)["index"] == tcs[1].(map[string]any)["index"] {
		t.Error("split calls share one index, want distinct indexes")
	}
}

func TestReshapeFlushSplitKeepsIndexDensity(t *testing.T) {
	rw := newChunkRewriter(&relayStats{toolMap: splitStreamMapper(t)})
	// Index 1 names an unruled wire (no reshape rule): it flows through live
	// but still counts for reshapeMaxIdx, so every extra lands above every
	// observed upstream index.
	rw.reshapeBuffer(toolCallFragmentAt("call_0", "read_files", splitStreamArgs, 0))
	rw.reshapeBuffer(toolCallFragmentAt("call_u", "suggest_followups", `{"followups":[{"prompt":"go"}]}`, 1))
	rw.reshapeBuffer(toolCallFragmentAt("call_2", "read_files", `{"paths":["c.go"]}`, 2))
	chunk := splitTerminalChunk()
	if !rw.reshapeFlush(chunk) {
		t.Fatal("reshapeFlush injected nothing")
	}
	tcs := splitFlushedCalls(t, chunk)
	// idx0 splits to 2 (indexes 0 and 3), idx2 reshapes 1:1 at 2. Index 1
	// flowed through live (unruled), never buffered.
	if len(tcs) != 3 {
		t.Fatalf("injected calls = %d, want 3", len(tcs))
	}
	var idxs []int
	for _, raw := range tcs {
		idx, _ := raw.(map[string]any)["index"].(int)
		idxs = append(idxs, idx)
	}
	for i, want := range []int{0, 3, 2} {
		if idxs[i] != want {
			t.Errorf("call[%d] index = %d, want %d", i, idxs[i], want)
		}
	}
	seen := map[int]bool{}
	for _, idx := range idxs {
		if seen[idx] {
			t.Fatalf("duplicate injected index %d", idx)
		}
		seen[idx] = true
	}
}

func TestReshapeFlushSplitShapes(t *testing.T) {
	newRW := func(t *testing.T) *chunkRewriter {
		t.Helper()
		return newChunkRewriter(&relayStats{toolMap: splitStreamMapper(t)})
	}
	t.Run("three-way split", func(t *testing.T) {
		rw := newRW(t)
		rw.reshapeBuffer(toolCallFragment("call_3", "read", `{"path":"a"}{"path":"b"}{"path":"c"}`))
		chunk := splitTerminalChunk()
		if !rw.reshapeFlush(chunk) {
			t.Fatal("reshapeFlush injected nothing")
		}
		if tcs := splitFlushedCalls(t, chunk); len(tcs) != 3 {
			t.Fatalf("injected calls = %d, want 3", len(tcs))
		}
	})
	t.Run("single JSON unchanged", func(t *testing.T) {
		rw := newRW(t)
		rw.reshapeBuffer(toolCallFragment("call_1", "read_files", `{"paths":["a.go"]}`))
		chunk := splitTerminalChunk()
		if !rw.reshapeFlush(chunk) {
			t.Fatal("reshapeFlush injected nothing")
		}
		tcs := splitFlushedCalls(t, chunk)
		if len(tcs) != 1 {
			t.Fatalf("injected calls = %d, want 1", len(tcs))
		}
		fn := tcs[0].(map[string]any)["function"].(map[string]any)
		if fn["arguments"] != `{"path":"a.go"}` {
			t.Errorf("arguments = %v, want the reshaped OMP shape", fn["arguments"])
		}
	})
	t.Run("malformed flushes verbatim", func(t *testing.T) {
		rw := newRW(t)
		rw.reshapeBuffer(toolCallFragment("call_1", "read_files", `{"paths":`))
		chunk := splitTerminalChunk()
		if !rw.reshapeFlush(chunk) {
			t.Fatal("reshapeFlush dropped the unreshapable buffer")
		}
		tcs := splitFlushedCalls(t, chunk)
		if len(tcs) != 1 {
			t.Fatalf("injected calls = %d, want 1 verbatim", len(tcs))
		}
		fn := tcs[0].(map[string]any)["function"].(map[string]any)
		if fn["arguments"] != `{"paths":` {
			t.Errorf("arguments = %v, want verbatim bytes (never swallowed)", fn["arguments"])
		}
	})
}
