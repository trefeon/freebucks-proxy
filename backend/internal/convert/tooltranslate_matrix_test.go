package convert

import (
	"strings"
	"testing"
)

// Issue #729 translation matrix, request + convert-level response legs
// (mock-level, no live keys, no network).
//
// #729: POST /v1/chat/completions with ANY tools[] on
// deepseek/deepseek-v4-flash answers upstream 404 "No endpoints found for
// deepseek/deepseek-v4-flash", while the same body without tools succeeds.
// Name translation itself is proven good (#685 closed, corpus green); this
// file pins the full client→wire→client round-trip per shape so a future
// emission change shows up here first.
//
// Verdict vocabulary: translates (the call reaches the model under a wire
// name and comes back under the client's name), stripped (a proxy-injected
// pseudo-tool never reaches the client), errors (the proxy rejects before
// upstream).
//
// Emission ownership: the exact injected first-party pin (currently end_turn
// + decide, schemacache_endturn.go) is owned by LaneA's gate tests; rows here
// assert injection presence loosely (wire longer than client tools) so an
// emission change extends rather than breaks this file. If LaneA adds new
// injected names, add strip rows for them here after rebase.
//
// Live-gate verdict ownership: whether the #729 wire trips the CURRENT
// upstream gate is pinned by LaneA's cf_worker_signals_test.go (living mirror
// of detectCfWorker in convert/cf_worker_signals.go: the proxy egress sends
// no cf-worker header, so ProxyCfWorkerEgressVerdict reads CLEAR). This file
// pins translation only and duplicates no gate verdict.
//
// Matrix (shape → verdict), request leg via NormalizeRequestMappedOpts:
//  01 no tools → bare wire (translates; nothing to carry)
//  02 empty tools[] → bare wire, no injection (translates)
//  03 single custom test_tool (#729 shape) → verbatim + pin (translates)
//  04 mapped bash → run_terminal_command + pin (translates)
//  05..07 tool_choice auto/none/required strings → passthrough (translates)
//  08 tool_choice pinned mapped name → renamed upstream (translates)
//  09 tool_choice pinned custom name → verbatim (translates)
//  10 parallel_tool_calls:false → preserved (translates)
//  11 strict:true tool → strict preserved, rename+restore (translates)
//  12 dotted web.run → legalized, restores (translates)
//  13 spaced "my tool" → legalized, restores (translates)
//  14 duplicate illegal name twice → unique wire names, both restore (translates)
//  15 dedupe pair terminal+execute_code → first keeps official (translates)
//  16 client-native mcp__foo → verbatim both ways (translates)
//  17 harness PascalCase Task → virtualized, restores (translates)
//  18 legacy functions[] + function_call:"auto" → converted + renamed (translates)
//  19 legacy function_call named mapped → tool_choice renamed (translates)
//  20 client-declared end_turn → no duplicate injection (translates)
//  21 client-declared decide → no duplicate injection (translates)
//  22 #685 threaded-vs-rebuilt mapper on dedupe pair (translates, threaded)
//  23 client-declared end_turn → mapper identity (translates)
// Response legs (same file): native delta/message restore, end_turn+decide
// strip with finish_reason alignment, mixed real+injected turns.

func matrixFuncTool(name string) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": "test tool " + name,
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"x": map[string]any{"type": "string"}},
			},
		},
	}
}

func matrixWire(t *testing.T, body map[string]any) (map[string]any, ToolMapper) {
	t.Helper()
	wire, mapper, err := NormalizeRequestMappedOpts(mustJSON(t, body), "", DefaultOptions())
	if err != nil {
		t.Fatalf("NormalizeRequestMappedOpts: %v", err)
	}
	return decode(t, wire), mapper
}

func matrixWireNames(t *testing.T, wire map[string]any) []string {
	t.Helper()
	raw, ok := wire["tools"]
	if !ok {
		return nil
	}
	tools, ok := raw.([]any)
	if !ok {
		t.Fatalf("tools = %T, want array", raw)
	}
	var names []string
	for _, tv := range tools {
		tm, ok := tv.(map[string]any)
		if !ok {
			t.Fatalf("tool = %T, want object", tv)
		}
		fn, ok := tm["function"].(map[string]any)
		if !ok {
			t.Fatalf("tool has no function object: %v", tm)
		}
		name, _ := fn["name"].(string)
		names = append(names, name)
	}
	return names
}

// assertWireSane pins the strict-upstream contract on every emitted wire:
// names unique (DeepSeek/Muse Spark/MiMo reject duplicates outright) and
// grammar-legal (one illegal name 400s the whole request).
func assertWireSane(t *testing.T, names []string) {
	t.Helper()
	seen := map[string]bool{}
	for _, n := range names {
		if n == "" {
			t.Fatalf("wire carries an empty tool name: %q", names)
		}
		if seen[n] {
			t.Fatalf("duplicate wire tool name %q in %q (strict upstreams reject the request)", n, names)
		}
		seen[n] = true
		if !wireToolNameOK(n) {
			t.Fatalf("wire tool name %q violates the upstream grammar", n)
		}
	}
}

func matrixChatBody(tools []any, extra map[string]any) map[string]any {
	body := map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	if tools != nil {
		body["tools"] = tools
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func TestTranslateMatrixRequestLeg(t *testing.T) {
	t.Run("01 no tools bare wire", func(t *testing.T) {
		wire, _ := matrixWire(t, matrixChatBody(nil, nil))
		if _, ok := wire["tools"]; ok {
			t.Fatalf("wire carries tools for a tool-less request: %v", wire["tools"])
		}
	})

	t.Run("02 empty tools no injection", func(t *testing.T) {
		wire, _ := matrixWire(t, matrixChatBody([]any{}, nil))
		if names := matrixWireNames(t, wire); len(names) != 0 {
			t.Fatalf("wire tools = %q, want empty (no injection on empty)", names)
		}
	})

	t.Run("03 custom test_tool verbatim plus pin", func(t *testing.T) {
		wire, mapper := matrixWire(t, matrixChatBody([]any{matrixFuncTool("test_tool")}, nil))
		names := matrixWireNames(t, wire)
		assertWireSane(t, names)
		if names[0] != "test_tool" {
			t.Fatalf("wire tools = %q, want client tool first", names)
		}
		if len(names) <= 1 {
			t.Fatalf("wire tools = %q, want first-party pin appended", names)
		}
		if got := mapper.RestoreName("test_tool"); got != "test_tool" {
			t.Fatalf("RestoreName(test_tool) = %q, want identity", got)
		}
	})

	t.Run("04 mapped bash renamed", func(t *testing.T) {
		wire, mapper := matrixWire(t, matrixChatBody([]any{matrixFuncTool("bash")}, nil))
		names := matrixWireNames(t, wire)
		assertWireSane(t, names)
		if names[0] != "run_terminal_command" {
			t.Fatalf("wire tools = %q, want run_terminal_command first", names)
		}
		if got := mapper.RestoreName("run_terminal_command"); got != "bash" {
			t.Fatalf("RestoreName = %q, want bash", got)
		}
	})

	for _, tc := range []string{"auto", "none", "required"} {
		t.Run("05-07 tool_choice "+tc+" passthrough", func(t *testing.T) {
			wire, _ := matrixWire(t, matrixChatBody([]any{matrixFuncTool("bash")}, map[string]any{"tool_choice": tc}))
			if got, _ := wire["tool_choice"].(string); got != tc {
				t.Fatalf("tool_choice = %v, want %q passthrough", wire["tool_choice"], tc)
			}
			assertWireSane(t, matrixWireNames(t, wire))
		})
	}

	t.Run("08 tool_choice pinned mapped renamed", func(t *testing.T) {
		wire, _ := matrixWire(t, matrixChatBody([]any{matrixFuncTool("bash")}, map[string]any{
			"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "bash"}},
		}))
		fn, ok := wire["tool_choice"].(map[string]any)["function"].(map[string]any)
		if !ok {
			t.Fatalf("tool_choice lost its function shape: %v", wire["tool_choice"])
		}
		if fn["name"] != "run_terminal_command" {
			t.Fatalf("tool_choice name = %v, want run_terminal_command", fn["name"])
		}
	})

	t.Run("09 tool_choice pinned custom verbatim", func(t *testing.T) {
		wire, _ := matrixWire(t, matrixChatBody([]any{matrixFuncTool("get_weather")}, map[string]any{
			"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}},
		}))
		fn, ok := wire["tool_choice"].(map[string]any)["function"].(map[string]any)
		if !ok || fn["name"] != "get_weather" {
			t.Fatalf("tool_choice = %v, want get_weather verbatim", wire["tool_choice"])
		}
	})

	t.Run("10 parallel_tool_calls preserved", func(t *testing.T) {
		wire, _ := matrixWire(t, matrixChatBody([]any{matrixFuncTool("bash")}, map[string]any{"parallel_tool_calls": false}))
		if got, _ := wire["parallel_tool_calls"].(bool); got != false {
			t.Fatalf("parallel_tool_calls = %v, want false passthrough", wire["parallel_tool_calls"])
		}
	})

	t.Run("11 strict true preserved and restores", func(t *testing.T) {
		tool := matrixFuncTool("bash")
		tool["function"].(map[string]any)["strict"] = true
		wire, mapper := matrixWire(t, matrixChatBody([]any{tool}, nil))
		names := matrixWireNames(t, wire)
		assertWireSane(t, names)
		if names[0] != "run_terminal_command" {
			t.Fatalf("wire tools = %q, want renamed strict tool first", names)
		}
		raw := wire["tools"].([]any)
		fn := raw[0].(map[string]any)["function"].(map[string]any)
		if strict, _ := fn["strict"].(bool); !strict {
			t.Fatalf("strict flag dropped on the wire for %v", fn["name"])
		}
		if got := mapper.RestoreName("run_terminal_command"); got != "bash" {
			t.Fatalf("RestoreName = %q, want bash", got)
		}
	})

	for _, name := range []string{"web.run", "my tool"} {
		t.Run("12-13 illegal "+name+" legalized and restores", func(t *testing.T) {
			wire, mapper := matrixWire(t, matrixChatBody([]any{matrixFuncTool(name)}, nil))
			names := matrixWireNames(t, wire)
			assertWireSane(t, names)
			if names[0] == name {
				t.Fatalf("illegal client name %q reached the wire verbatim", name)
			}
			if !strings.HasPrefix(names[0], "mcp__") {
				t.Fatalf("legalized wire name = %q, want mcp__ namespace", names[0])
			}
			if got := mapper.RestoreName(names[0]); got != name {
				t.Fatalf("RestoreName(%q) = %q, want %q", names[0], got, name)
			}
		})
	}

	t.Run("14 duplicate illegal name unique and restores", func(t *testing.T) {
		wire, mapper := matrixWire(t, matrixChatBody([]any{matrixFuncTool("a.b"), matrixFuncTool("a.b")}, nil))
		names := matrixWireNames(t, wire)
		// Both client entries legalize from the same base: the wire must
		// carry two DISTINCT names that both restore to the client name.
		var virt []string
		for _, n := range names {
			if strings.HasPrefix(n, "mcp__") {
				virt = append(virt, n)
			}
		}
		if len(virt) != 2 {
			t.Fatalf("wire tools = %q, want two virtualized entries", names)
		}
		for _, v := range virt {
			if got := mapper.RestoreName(v); got != "a.b" {
				t.Fatalf("RestoreName(%q) = %q, want a.b", v, got)
			}
		}
	})

	t.Run("15 dedupe pair ownership", func(t *testing.T) {
		wire, mapper := matrixWire(t, matrixChatBody([]any{matrixFuncTool("terminal"), matrixFuncTool("execute_code")}, nil))
		names := matrixWireNames(t, wire)
		assertWireSane(t, names)
		if names[0] != "run_terminal_command" {
			t.Fatalf("wire tools = %q, want first claimer to keep the official name", names)
		}
		if names[1] == "run_terminal_command" {
			t.Fatalf("wire tools = %q, duplicate official name (strict upstreams reject)", names)
		}
		if got := mapper.RestoreName("run_terminal_command"); got != "terminal" {
			t.Fatalf("official restores to %q, want first claimer terminal", got)
		}
		if got := mapper.RestoreName(names[1]); got != "execute_code" {
			t.Fatalf("virtualized restores to %q, want execute_code", got)
		}
	})

	t.Run("16 client-native mcp verbatim", func(t *testing.T) {
		wire, mapper := matrixWire(t, matrixChatBody([]any{matrixFuncTool("mcp__serv__tool")}, nil))
		names := matrixWireNames(t, wire)
		assertWireSane(t, names)
		if names[0] != "mcp__serv__tool" {
			t.Fatalf("wire tools = %q, want client mcp__ name verbatim", names)
		}
		if got := mapper.RestoreName("mcp__serv__tool"); got != "mcp__serv__tool" {
			t.Fatalf("RestoreName = %q, want verbatim (no blind prefix strip)", got)
		}
	})

	t.Run("17 harness Task virtualized and restores", func(t *testing.T) {
		wire, mapper := matrixWire(t, matrixChatBody([]any{matrixFuncTool("Task")}, nil))
		names := matrixWireNames(t, wire)
		assertWireSane(t, names)
		if names[0] == "Task" {
			t.Fatalf("harness name Task reached the wire verbatim: %q", names)
		}
		if got := mapper.RestoreName(names[0]); got != "Task" {
			t.Fatalf("RestoreName(%q) = %q, want Task", names[0], got)
		}
	})

	t.Run("18 legacy functions converted and renamed", func(t *testing.T) {
		body := map[string]any{
			"model":    "deepseek/deepseek-v4-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			"functions": []any{map[string]any{
				"name":        "bash",
				"description": "run a command",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			}},
			"function_call": "auto",
		}
		wire, mapper := matrixWire(t, body)
		names := matrixWireNames(t, wire)
		assertWireSane(t, names)
		if names[0] != "run_terminal_command" {
			t.Fatalf("wire tools = %q, want converted legacy tool renamed", names)
		}
		if got, _ := wire["tool_choice"].(string); got != "auto" {
			t.Fatalf("tool_choice = %v, want auto passthrough", wire["tool_choice"])
		}
		if got := mapper.RestoreName("run_terminal_command"); got != "bash" {
			t.Fatalf("RestoreName = %q, want bash (legacy leg restores via ToUpstream ownership)", got)
		}
	})

	t.Run("19 legacy function_call named mapped renamed", func(t *testing.T) {
		body := map[string]any{
			"model":    "deepseek/deepseek-v4-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			"functions": []any{map[string]any{
				"name":        "bash",
				"description": "run a command",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			}},
			"function_call": map[string]any{"name": "bash"},
		}
		wire, _ := matrixWire(t, body)
		fn, ok := wire["tool_choice"].(map[string]any)["function"].(map[string]any)
		if !ok || fn["name"] != "run_terminal_command" {
			t.Fatalf("tool_choice = %v, want renamed official name", wire["tool_choice"])
		}
	})

	t.Run("20 client end_turn not duplicated", func(t *testing.T) {
		wire, _ := matrixWire(t, matrixChatBody([]any{matrixFuncTool("bash"), matrixFuncTool("end_turn")}, nil))
		names := matrixWireNames(t, wire)
		assertWireSane(t, names)
		count := 0
		for _, n := range names {
			if n == "end_turn" {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("end_turn appears %d times in %q, want exactly once", count, names)
		}
	})

	t.Run("21 client decide not duplicated", func(t *testing.T) {
		wire, _ := matrixWire(t, matrixChatBody([]any{matrixFuncTool("bash"), matrixFuncTool("decide")}, nil))
		names := matrixWireNames(t, wire)
		assertWireSane(t, names)
		count := 0
		for _, n := range names {
			if n == "decide" {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("decide appears %d times in %q, want exactly once", count, names)
		}
	})

	t.Run("22 #685 threaded mapper restores virtualized call", func(t *testing.T) {
		// opencode v2 offers bash + execute, both mapping to
		// run_terminal_command. The request leg virtualizes the later one;
		// ONLY the threaded mapper (the one ToUpstream ran on) knows the
		// virtual name. Handlers must thread it, never rebuild.
		body := matrixChatBody([]any{matrixFuncTool("bash"), matrixFuncTool("execute")}, nil)
		_, threaded := matrixWire(t, body)
		chunk := map[string]any{
			"choices": []any{map[string]any{
				"delta": map[string]any{"tool_calls": []any{
					map[string]any{"function": map[string]any{"name": "mcp__execute"}},
				}},
			}},
		}
		if !threaded.FromUpstreamChunk(chunk) {
			t.Fatalf("threaded mapper did not restore mcp__execute")
		}
		delta := chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		got := delta["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["name"]
		if got != "execute" {
			t.Fatalf("restored name = %v, want execute (a rebuild would leak mcp__execute)", got)
		}
		rebuilt := NewToolMapper(mustJSON(t, body))
		if name := rebuilt.RestoreName("mcp__execute"); name != "mcp__execute" {
			t.Fatalf("rebuilt mapper restores mcp__execute to %q: test premise changed (dedupe now visible pre-ToUpstream?)", name)
		}
	})

	t.Run("23 client-declared end_turn restores identity", func(t *testing.T) {
		// A client that declares end_turn itself owns the name on the wire
		// (row 20: no duplicate injection); the mapper holds no entry for
		// it, so its calls round-trip verbatim.
		wire, mapper := matrixWire(t, matrixChatBody([]any{matrixFuncTool("bash"), matrixFuncTool("end_turn")}, nil))
		names := matrixWireNames(t, wire)
		assertWireSane(t, names)
		if got := mapper.RestoreName("end_turn"); got != "end_turn" {
			t.Fatalf("RestoreName(end_turn) = %q, want identity for a client-declared name", got)
		}
	})
}

// matrixCompletion builds a non-streaming completion carrying upstream tool
// calls as (id, wireName, arguments) triples.
func matrixCompletion(finish string, calls ...[3]string) map[string]any {
	var tcs []any
	for _, c := range calls {
		tcs = append(tcs, map[string]any{
			"id":   c[0],
			"type": "function",
			"function": map[string]any{
				"name":      c[1],
				"arguments": c[2],
			},
		})
	}
	msg := map[string]any{}
	if tcs != nil {
		msg["tool_calls"] = tcs
	}
	return map[string]any{
		"choices": []any{map[string]any{
			"message":       msg,
			"finish_reason": finish,
		}},
	}
}

// matrixDeltaChunk builds a streaming chunk carrying upstream tool calls as
// (index, id, wireName, arguments) tuples.
func matrixDeltaChunk(finish string, calls ...[4]any) map[string]any {
	var tcs []any
	for _, c := range calls {
		tcs = append(tcs, map[string]any{
			"index": c[0],
			"id":    c[1],
			"type":  "function",
			"function": map[string]any{
				"name":      c[2],
				"arguments": c[3],
			},
		})
	}
	delta := map[string]any{}
	if tcs != nil {
		delta["tool_calls"] = tcs
	}
	choice := map[string]any{"delta": delta}
	if finish != "" {
		choice["finish_reason"] = finish
	}
	return map[string]any{"choices": []any{choice}}
}

func matrixCallNames(t *testing.T, comp map[string]any) []string {
	t.Helper()
	choice := comp["choices"].([]any)[0].(map[string]any)
	msg, ok := choice["message"].(map[string]any)
	if !ok {
		delta := choice["delta"].(map[string]any)
		tcs, ok := delta["tool_calls"].([]any)
		if !ok {
			return nil
		}
		var names []string
		for _, raw := range tcs {
			names = append(names, raw.(map[string]any)["function"].(map[string]any)["name"].(string))
		}
		return names
	}
	tcs, ok := msg["tool_calls"].([]any)
	if !ok {
		return nil
	}
	var names []string
	for _, raw := range tcs {
		names = append(names, raw.(map[string]any)["function"].(map[string]any)["name"].(string))
	}
	return names
}

func TestTranslateMatrixResponseLeg(t *testing.T) {
	newBashMapper := func(t *testing.T) ToolMapper {
		t.Helper()
		_, mapper := matrixWire(t, matrixChatBody([]any{matrixFuncTool("bash")}, nil))
		return mapper
	}

	t.Run("streamed native mapped name restores", func(t *testing.T) {
		mapper := newBashMapper(t)
		chunk := matrixDeltaChunk("", [4]any{0, "call_1", "run_terminal_command", `{"command":"pwd"}`})
		if !mapper.FromUpstreamChunk(chunk) {
			t.Fatalf("FromUpstreamChunk reported no change on a mapped name")
		}
		if names := matrixCallNames(t, chunk); len(names) != 1 || names[0] != "bash" {
			t.Fatalf("delta names = %q, want [bash]", names)
		}
	})

	t.Run("non-streaming message mapped name restores", func(t *testing.T) {
		mapper := newBashMapper(t)
		comp := matrixCompletion("tool_calls", [3]string{"call_1", "run_terminal_command", `{"command":"pwd"}`})
		if !mapper.FromUpstreamChunk(comp) {
			t.Fatalf("FromUpstreamChunk reported no change on a mapped name")
		}
		if names := matrixCallNames(t, comp); len(names) != 1 || names[0] != "bash" {
			t.Fatalf("message names = %q, want [bash]", names)
		}
	})

	t.Run("end_turn-only strips and flips to stop", func(t *testing.T) {
		comp := matrixCompletion("tool_calls", [3]string{"call_e", "end_turn", `{}`})
		remaining, reason := StripEndTurnToolCalls(comp)
		if remaining {
			t.Fatalf("tool calls remain after end_turn-only strip")
		}
		if reason != "stop" {
			t.Fatalf("finish reason = %q, want stop", reason)
		}
		if names := matrixCallNames(t, comp); len(names) != 0 {
			t.Fatalf("names = %q, want none delivered", names)
		}
	})

	t.Run("decide-only strips and flips to stop", func(t *testing.T) {
		comp := matrixCompletion("tool_calls", [3]string{"call_d", "decide", `{"choice":"x"}`})
		remaining, reason := StripEndTurnToolCalls(comp)
		if remaining {
			t.Fatalf("tool calls remain after decide-only strip")
		}
		if reason != "stop" {
			t.Fatalf("finish reason = %q, want stop", reason)
		}
	})

	t.Run("mixed real plus injected keeps real and reason", func(t *testing.T) {
		mapper := newBashMapper(t)
		comp := matrixCompletion("tool_calls",
			[3]string{"call_1", "run_terminal_command", `{"command":"pwd"}`},
			[3]string{"call_e", "end_turn", `{}`},
			[3]string{"call_d", "decide", `{"choice":"x"}`},
		)
		remaining, reason := StripEndTurnToolCalls(comp)
		if !remaining {
			t.Fatalf("real call stripped alongside the injected ones")
		}
		if reason != "tool_calls" {
			t.Fatalf("finish reason = %q, want tool_calls preserved", reason)
		}
		if !mapper.FromUpstreamChunk(comp) {
			t.Fatalf("FromUpstreamChunk reported no change on the surviving mapped call")
		}
		if names := matrixCallNames(t, comp); len(names) != 1 || names[0] != "bash" {
			t.Fatalf("message names = %q, want [bash] only", names)
		}
	})
}
