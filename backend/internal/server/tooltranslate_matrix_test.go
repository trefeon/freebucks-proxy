package server

import (
	"context"
	"encoding/json"
	"freebucks-proxy/backend/internal/convert"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Issue #729 translation matrix, relay + ingress legs (mock-level, no live
// keys, no network). Companion to convert/tooltranslate_matrix_test.go, which
// pins the chat-surface request leg and the convert-level response helpers;
// this file drives the relays directly with scripted upstream SSE through the
// SAME mapper the request leg produced (the #685 contract: handlers thread
// the NormalizeRequestMapped mapper into chatCore, never rebuild it).
//
// Verdict vocabulary: translates (client-visible output carries the client's
// dispatch name), stripped (injected end_turn/decide never surface).
//
// Matrix (surface → verdict):
//  S1 OpenAI stream, mapped call + end_turn → bash, finish tool_calls (translates, stripped)
//  S2 OpenAI non-stream, mapped call + end_turn → bash, finish tool_calls (translates, stripped)
//  S3 OpenAI non-stream, end_turn-only → zero calls, finish stop (stripped)
//  S4 OpenAI stream, decide-only → zero calls, finish stop (stripped)
//  S5 Anthropic stream, mapped call + injected → tool_use bash, stop tool_use (translates, stripped)
//  S6 Anthropic non-stream, mapped call + injected → tool_use bash (translates, stripped)
//  S6b Anthropic non-stream, end_turn-only → no tool_use, stop end_turn (stripped)
//  S7 Responses stream, mapped call + end_turn → function_call bash (translates, stripped)
//  S8 Responses non-stream, mapped call + end_turn → function_call bash (translates, stripped)
//  S9 Anthropic ingress (input_schema tools) → official wire names (translates)
//  S10 Responses ingress (function tools) → official wire names (translates)
//  S11 Responses ingress named tool_choice → renamed (translates)
//
// Emission ownership (same as the convert matrix): exact injected pin names
// are owned by LaneA's gate tests. If LaneA adds new injected names, add
// strip rows here after rebase.

func tmxFuncTool(name string) map[string]any {
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

// tmxMappedMapper runs the real handler path for a chat body offering tools:
// NormalizeRequestMappedOpts returns the wire bytes plus the SAME mapper
// chatCore threads into the relays.
func tmxMappedMapper(t *testing.T, tools []string) (map[string]any, convert.ToolMapper) {
	t.Helper()
	var list []any
	for _, name := range tools {
		list = append(list, tmxFuncTool(name))
	}
	body, err := json.Marshal(map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools":    list,
	})
	if err != nil {
		t.Fatalf("marshal chat body: %v", err)
	}
	wire, mapper, err := convert.NormalizeRequestMappedOpts(body, "", convert.DefaultOptions())
	if err != nil {
		t.Fatalf("NormalizeRequestMappedOpts: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatalf("decode wire: %v", err)
	}
	return decoded, mapper
}

type tmxEvent struct {
	event string
	data  map[string]any
}

// tmxParseEvents parses SSE event:/data: pairs; OpenAI data-only frames
// arrive with an empty event name.
func tmxParseEvents(t *testing.T, body string) []tmxEvent {
	t.Helper()
	var out []tmxEvent
	event := ""
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		trim := strings.TrimSpace(line)
		if !strings.HasPrefix(trim, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trim, "data:"))
		if payload == "" || payload == "[DONE]" {
			event = ""
			continue
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(payload), &data); err != nil {
			event = ""
			continue
		}
		out = append(out, tmxEvent{event: event, data: data})
		event = ""
	}
	return out
}

func tmxChunk(id string, deltaFrag string, finish string) string {
	delta := `{}`
	if deltaFrag != "" {
		delta = `{"tool_calls":[` + deltaFrag + `]}`
	}
	choice := `{"index":0,"delta":` + delta
	if finish == "" {
		choice += `,"finish_reason":null}`
	} else {
		choice += `,"finish_reason":"` + finish + `"}`
	}
	return testutilSSE(`{"id":"` + id + `","object":"chat.completion.chunk","created":1,"model":"m","choices":[` + choice + `]}`)
}

func tmxNativeCall(index int, id, name, args string) string {
	return `{"index":` + itoa(index) + `,"id":"` + id + `","type":"function","function":{"name":"` + name + `","arguments":` + strconvQuote(args) + `}}`
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := []byte{}
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func tmxTerminalChunk(id, finish string) string {
	return testutilSSE(`{"id":"` + id + `","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"` + finish + `"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
}

// tmxFrameNames collects every function.name in delta/message tool_calls
// across parsed frames plus every terminal finish_reason.
func tmxFrameNames(events []tmxEvent) (names []string, finishes []string) {
	for _, ev := range events {
		choices, _ := ev.data["choices"].([]any)
		for _, raw := range choices {
			choice, _ := raw.(map[string]any)
			if choice == nil {
				continue
			}
			if fr, _ := choice["finish_reason"].(string); fr != "" {
				finishes = append(finishes, fr)
			}
			for _, key := range []string{"delta", "message"} {
				holder, _ := choice[key].(map[string]any)
				if holder == nil {
					continue
				}
				tcs, _ := holder["tool_calls"].([]any)
				for _, tc := range tcs {
					tcMap, _ := tc.(map[string]any)
					if tcMap == nil {
						continue
					}
					fn, _ := tcMap["function"].(map[string]any)
					if fn == nil {
						continue
					}
					if name, _ := fn["name"].(string); name != "" {
						names = append(names, name)
					}
				}
			}
		}
	}
	return names, finishes
}

func tmxAssertNoInjected(t *testing.T, body string) {
	t.Helper()
	// Tool-name shape only: "stop_reason":"end_turn" is a legitimate Anthropic
	// value, not a pseudo-tool leak.
	for _, leaked := range []string{`"name":"end_turn"`, `"name":"decide"`} {
		if strings.Contains(body, leaked) {
			t.Fatalf("injected pseudo-tool %s leaked to the client: %s", leaked, truncateStr(body, 400))
		}
	}
}

func TestTranslateMatrixOpenAIRelays(t *testing.T) {
	_, tm := tmxMappedMapper(t, []string{"bash"})
	native := tmxChunk("chatcmpl-m1", tmxNativeCall(0, "call_1", "run_terminal_command", `{"command":"pwd"}`), "") +
		tmxChunk("chatcmpl-m1", tmxNativeCall(1, "call_e", "end_turn", `{}`), "") +
		tmxTerminalChunk("chatcmpl-m1", "tool_calls")

	t.Run("S1 stream restores and strips", func(t *testing.T) {
		s := testRelayServer()
		rec := httptest.NewRecorder()
		s.relayStream(context.Background(), rec, strings.NewReader(native), &relayStats{toolMap: tm}, time.Now())
		body := rec.Body.String()
		tmxAssertNoInjected(t, body)
		names, finishes := tmxFrameNames(tmxParseEvents(t, body))
		foundBash := false
		for _, n := range names {
			if n == "bash" {
				foundBash = true
			}
		}
		if !foundBash {
			t.Fatalf("streamed names = %q, want bash restored", names)
		}
		if len(finishes) == 0 || finishes[len(finishes)-1] != "tool_calls" {
			t.Fatalf("terminal finish = %q, want tool_calls", finishes)
		}
	})

	t.Run("S2 non-stream restores and strips", func(t *testing.T) {
		s := testRelayServer()
		rec := httptest.NewRecorder()
		s.relayJSON(context.Background(), rec, strings.NewReader(native), &relayStats{toolMap: tm}, time.Now())
		var comp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &comp); err != nil {
			t.Fatalf("decode completion: %v", err)
		}
		tmxAssertNoInjected(t, rec.Body.String())
		choice := comp["choices"].([]any)[0].(map[string]any)
		msg := choice["message"].(map[string]any)
		tcs, _ := msg["tool_calls"].([]any)
		if len(tcs) != 1 {
			t.Fatalf("tool_calls = %d, want exactly the real call", len(tcs))
		}
		name := tcs[0].(map[string]any)["function"].(map[string]any)["name"]
		if name != "bash" {
			t.Fatalf("call name = %v, want bash", name)
		}
		if fr := choice["finish_reason"]; fr != "tool_calls" {
			t.Fatalf("finish_reason = %v, want tool_calls", fr)
		}
	})

	t.Run("S3 non-stream end_turn-only stops", func(t *testing.T) {
		s := testRelayServer()
		rec := httptest.NewRecorder()
		ss := tmxChunk("chatcmpl-m2", tmxNativeCall(0, "call_e", "end_turn", `{}`), "") +
			tmxTerminalChunk("chatcmpl-m2", "tool_calls")
		s.relayJSON(context.Background(), rec, strings.NewReader(ss), &relayStats{toolMap: tm}, time.Now())
		var comp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &comp); err != nil {
			t.Fatalf("decode completion: %v", err)
		}
		tmxAssertNoInjected(t, rec.Body.String())
		choice := comp["choices"].([]any)[0].(map[string]any)
		if tcs, _ := choice["message"].(map[string]any)["tool_calls"].([]any); len(tcs) != 0 {
			t.Fatalf("tool_calls = %d, want 0 (end_turn stripped)", len(tcs))
		}
		if fr := choice["finish_reason"]; fr != "stop" {
			t.Fatalf("finish_reason = %v, want stop", fr)
		}
	})

	t.Run("S4 stream decide-only stops", func(t *testing.T) {
		s := testRelayServer()
		rec := httptest.NewRecorder()
		ss := tmxChunk("chatcmpl-m3", tmxNativeCall(0, "call_d", "decide", `{"choice":"x"}`), "") +
			tmxTerminalChunk("chatcmpl-m3", "tool_calls")
		s.relayStream(context.Background(), rec, strings.NewReader(ss), &relayStats{toolMap: tm}, time.Now())
		body := rec.Body.String()
		tmxAssertNoInjected(t, body)
		names, finishes := tmxFrameNames(tmxParseEvents(t, body))
		if len(names) != 0 {
			t.Fatalf("streamed names = %q, want none (decide stripped)", names)
		}
		if len(finishes) == 0 || finishes[len(finishes)-1] != "stop" {
			t.Fatalf("terminal finish = %q, want stop", finishes)
		}
	})
}

func tmxAnthropicToolUseNames(events []tmxEvent) []string {
	var names []string
	for _, ev := range events {
		if ev.event != "content_block_start" {
			continue
		}
		block, _ := ev.data["content_block"].(map[string]any)
		if block == nil {
			continue
		}
		if typ, _ := block["type"].(string); typ != "tool_use" {
			continue
		}
		if name, _ := block["name"].(string); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func tmxAnthropicStopReason(events []tmxEvent) string {
	stop := ""
	for _, ev := range events {
		if ev.event != "message_delta" {
			continue
		}
		if delta, _ := ev.data["delta"].(map[string]any); delta != nil {
			if s, _ := delta["stop_reason"].(string); s != "" {
				stop = s
			}
		}
	}
	return stop
}

func TestTranslateMatrixAnthropicRelays(t *testing.T) {
	_, tm := tmxMappedMapper(t, []string{"bash"})
	native := tmxChunk("chatcmpl-a1", tmxNativeCall(0, "call_1", "run_terminal_command", `{"command":"pwd"}`), "") +
		tmxChunk("chatcmpl-a1", tmxNativeCall(1, "call_e", "end_turn", `{}`), "") +
		tmxChunk("chatcmpl-a1", tmxNativeCall(2, "call_d", "decide", `{"choice":"x"}`), "") +
		tmxTerminalChunk("chatcmpl-a1", "tool_calls")

	t.Run("S5 stream restores and strips", func(t *testing.T) {
		s := testRelayServer()
		rec := httptest.NewRecorder()
		s.relayAnthropicStream(context.Background(), rec, strings.NewReader(native), &relayStats{toolMap: tm}, time.Now(), "m", 0)
		body := rec.Body.String()
		tmxAssertNoInjected(t, body)
		events := tmxParseEvents(t, body)
		if names := tmxAnthropicToolUseNames(events); len(names) != 1 || names[0] != "bash" {
			t.Fatalf("tool_use blocks = %q, want [bash]", names)
		}
		if stop := tmxAnthropicStopReason(events); stop != "tool_use" {
			t.Fatalf("stop_reason = %q, want tool_use", stop)
		}
	})

	t.Run("S6 non-stream restores and strips", func(t *testing.T) {
		s := testRelayServer()
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		s.relayAnthropicJSON(context.Background(), rec, r, strings.NewReader(native), &relayStats{toolMap: tm}, time.Now(), "m")
		var msg map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
			t.Fatalf("decode message: %v", err)
		}
		tmxAssertNoInjected(t, rec.Body.String())
		blocks, _ := msg["content"].([]any)
		var names []string
		for _, raw := range blocks {
			b, _ := raw.(map[string]any)
			if b == nil || b["type"] != "tool_use" {
				continue
			}
			names = append(names, b["name"].(string))
		}
		if len(names) != 1 || names[0] != "bash" {
			t.Fatalf("tool_use blocks = %q, want [bash]", names)
		}
		if stop := msg["stop_reason"]; stop != "tool_use" {
			t.Fatalf("stop_reason = %v, want tool_use", stop)
		}
	})

	t.Run("S6b non-stream end_turn-only demotes", func(t *testing.T) {
		s := testRelayServer()
		rec := httptest.NewRecorder()
		ss := tmxChunk("chatcmpl-a2", tmxNativeCall(0, "call_e", "end_turn", `{}`), "") +
			tmxTerminalChunk("chatcmpl-a2", "tool_calls")
		r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		s.relayAnthropicJSON(context.Background(), rec, r, strings.NewReader(ss), &relayStats{toolMap: tm}, time.Now(), "m")
		var msg map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
			t.Fatalf("decode message: %v", err)
		}
		tmxAssertNoInjected(t, rec.Body.String())
		blocks, _ := msg["content"].([]any)
		for _, raw := range blocks {
			b, _ := raw.(map[string]any)
			if b != nil && b["type"] == "tool_use" {
				t.Fatalf("end_turn-only turn leaked a tool_use block: %v", b)
			}
		}
		if stop := msg["stop_reason"]; stop != "end_turn" {
			t.Fatalf("stop_reason = %v, want end_turn", stop)
		}
	})
}

func tmxResponsesFunctionCalls(events []tmxEvent) []map[string]any {
	var out []map[string]any
	for _, ev := range events {
		if ev.event != "response.output_item.done" {
			continue
		}
		item, _ := ev.data["item"].(map[string]any)
		if item == nil || item["type"] != "function_call" {
			continue
		}
		out = append(out, item)
	}
	return out
}

func TestTranslateMatrixResponsesRelays(t *testing.T) {
	_, tm := tmxMappedMapper(t, []string{"bash"})
	native := tmxChunk("chatcmpl-r1", tmxNativeCall(0, "call_1", "run_terminal_command", `{"command":"pwd"}`), "") +
		tmxChunk("chatcmpl-r1", tmxNativeCall(1, "call_e", "end_turn", `{}`), "") +
		tmxTerminalChunk("chatcmpl-r1", "tool_calls")

	t.Run("S7 stream restores and strips", func(t *testing.T) {
		s := testRelayServer()
		rec := httptest.NewRecorder()
		s.relayResponsesStream(context.Background(), rec, strings.NewReader(native), &relayStats{toolMap: tm}, time.Now(), "m", "resp_1")
		body := rec.Body.String()
		tmxAssertNoInjected(t, body)
		calls := tmxResponsesFunctionCalls(tmxParseEvents(t, body))
		if len(calls) != 1 {
			t.Fatalf("function_call items = %d, want exactly the real call", len(calls))
		}
		if calls[0]["name"] != "bash" {
			t.Fatalf("function_call name = %v, want bash", calls[0]["name"])
		}
		if calls[0]["arguments"] != `{"command":"pwd"}` {
			t.Fatalf("function_call arguments = %v, want client-shaped arguments", calls[0]["arguments"])
		}
	})

	t.Run("S8 non-stream restores and strips", func(t *testing.T) {
		s := testRelayServer()
		rec := httptest.NewRecorder()
		s.relayResponsesJSON(context.Background(), rec, strings.NewReader(native), &relayStats{toolMap: tm}, time.Now(), "m", "resp_1")
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		tmxAssertNoInjected(t, rec.Body.String())
		out, _ := resp["output"].([]any)
		var calls []map[string]any
		for _, raw := range out {
			item, _ := raw.(map[string]any)
			if item != nil && item["type"] == "function_call" {
				calls = append(calls, item)
			}
		}
		if len(calls) != 1 {
			t.Fatalf("function_call items = %d, want exactly the real call", len(calls))
		}
		if calls[0]["name"] != "bash" {
			t.Fatalf("function_call name = %v, want bash", calls[0]["name"])
		}
	})
}

func tmxWireNames(t *testing.T, wire map[string]any) []string {
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
		fn, ok := tv.(map[string]any)["function"].(map[string]any)
		if !ok {
			t.Fatalf("tool has no function object: %v", tv)
		}
		name, _ := fn["name"].(string)
		names = append(names, name)
	}
	return names
}

func TestTranslateMatrixIngress(t *testing.T) {
	t.Run("S9 anthropic input_schema tools reach official wire", func(t *testing.T) {
		raw := map[string]any{
			"model":      "m",
			"max_tokens": float64(64),
			"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
			"tools": []any{map[string]any{
				"name":         "bash",
				"description":  "run a command",
				"input_schema": map[string]any{"type": "object", "properties": map[string]any{}},
			}},
			"tool_choice": map[string]any{"type": "auto"},
		}
		chatParams, err := anthropicToChatParams(raw)
		if err != nil {
			t.Fatalf("anthropicToChatParams: %v", err)
		}
		wireBytes, mapper, err := convert.NormalizeRequestMappedOpts(chatParams, "", convert.DefaultOptions())
		if err != nil {
			t.Fatalf("NormalizeRequestMappedOpts: %v", err)
		}
		var wire map[string]any
		if err := json.Unmarshal(wireBytes, &wire); err != nil {
			t.Fatalf("decode wire: %v", err)
		}
		names := tmxWireNames(t, wire)
		if len(names) == 0 || names[0] != "run_terminal_command" {
			t.Fatalf("wire tools = %q, want renamed bash first", names)
		}
		if got := mapper.RestoreName("run_terminal_command"); got != "bash" {
			t.Fatalf("RestoreName = %q, want bash", got)
		}
	})

	t.Run("S10 responses function tools reach official wire", func(t *testing.T) {
		raw := map[string]any{
			"model": "m",
			"input": "hi",
			"tools": []any{map[string]any{
				"type":        "function",
				"name":        "bash",
				"description": "run a command",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			}},
			"tool_choice": "auto",
		}
		chatParams, err := responsesToChatParams(raw)
		if err != nil {
			t.Fatalf("responsesToChatParams: %v", err)
		}
		wireBytes, mapper, err := convert.NormalizeRequestMappedOpts(chatParams, "", convert.DefaultOptions())
		if err != nil {
			t.Fatalf("NormalizeRequestMappedOpts: %v", err)
		}
		var wire map[string]any
		if err := json.Unmarshal(wireBytes, &wire); err != nil {
			t.Fatalf("decode wire: %v", err)
		}
		names := tmxWireNames(t, wire)
		if len(names) == 0 || names[0] != "run_terminal_command" {
			t.Fatalf("wire tools = %q, want renamed bash first", names)
		}
		if got := mapper.RestoreName("run_terminal_command"); got != "bash" {
			t.Fatalf("RestoreName = %q, want bash", got)
		}
	})

	t.Run("S11 responses named tool_choice renamed", func(t *testing.T) {
		raw := map[string]any{
			"model": "m",
			"input": "hi",
			"tools": []any{map[string]any{
				"type":        "function",
				"name":        "bash",
				"description": "run a command",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			}},
			"tool_choice": map[string]any{"type": "function", "name": "bash"},
		}
		chatParams, err := responsesToChatParams(raw)
		if err != nil {
			t.Fatalf("responsesToChatParams: %v", err)
		}
		wireBytes, _, err := convert.NormalizeRequestMappedOpts(chatParams, "", convert.DefaultOptions())
		if err != nil {
			t.Fatalf("NormalizeRequestMappedOpts: %v", err)
		}
		var wire map[string]any
		if err := json.Unmarshal(wireBytes, &wire); err != nil {
			t.Fatalf("decode wire: %v", err)
		}
		fn, ok := wire["tool_choice"].(map[string]any)["function"].(map[string]any)
		if !ok || fn["name"] != "run_terminal_command" {
			t.Fatalf("tool_choice = %v, want renamed official name", wire["tool_choice"])
		}
	})
}
