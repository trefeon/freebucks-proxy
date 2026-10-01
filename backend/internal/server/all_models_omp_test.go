package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/modelcat"
	"freebuff-proxy/backend/internal/testutil"
)

// The 8 models served on the live dashboard (and in the catalog):
var allServedModels = []struct {
	ID              string
	ExpectedAgent   string
	HasReasoning    bool
	Mediumless      bool
	StrictReasoning bool
}{
	{
		ID:              "z-ai/glm-5.3-flash",
		ExpectedAgent:   "base3-free-glm-5-3-flash",
		HasReasoning:    true,
		Mediumless:      true,
		StrictReasoning: false,
	},
	{
		ID:              "mimo/mimo-v2.5",
		ExpectedAgent:   "base3-free-mimo",
		HasReasoning:    true,
		Mediumless:      false,
		StrictReasoning: true,
	},
	{
		ID:              "upstage/solar-mini4",
		ExpectedAgent:   "base3-free-solar-mini4",
		HasReasoning:    false,
		Mediumless:      false,
		StrictReasoning: false,
	},
	{
		ID:              "upstage/solar-pro4",
		ExpectedAgent:   "base3-free-solar-pro4",
		HasReasoning:    false,
		Mediumless:      false,
		StrictReasoning: false,
	},
	{
		ID:              "stealth/space-bunny-alpha",
		ExpectedAgent:   "base3-free-space-bunny-alpha",
		HasReasoning:    true,
		Mediumless:      false,
		StrictReasoning: false,
	},
	{
		ID:              "deepseek/deepseek-v4-flash",
		ExpectedAgent:   "base3-free-deepseek-flash",
		HasReasoning:    true,
		Mediumless:      true,
		StrictReasoning: true,
	},
	{
		ID:              "openai/gpt-6-luna",
		ExpectedAgent:   "base3-free-luna-6",
		HasReasoning:    true,
		Mediumless:      false,
		StrictReasoning: false,
	},
	{
		ID:              "mimo/mimo-v2.6-pro",
		ExpectedAgent:   "base3-free-mimo-2-6-pro",
		HasReasoning:    true,
		Mediumless:      false,
		StrictReasoning: true,
	},
}

// TestAllEightServedModelsOMPToolCalls verifies that every served model
// accepts OMP-format tool requests, rewrites the wire to canonical 17 tools
// (16 floor + end_turn) with zero foreign riders, routes to the correct upstream
// agent ID, and reshapes/restores tool calls back to OMP client format.
func TestAllEightServedModelsOMPToolCalls(t *testing.T) {
	for _, m := range allServedModels {
		t.Run(m.ID, func(t *testing.T) {
			// 1. Verify catalog properties match expectation
			if got := modelcat.IsServed(m.ID); !got {
				t.Fatalf("model %s is not marked Served in catalog", m.ID)
			}
			if got := modelcat.IsStrictReasoningModel(m.ID); got != m.StrictReasoning {
				t.Errorf("IsStrictReasoningModel(%s) = %v, want %v", m.ID, got, m.StrictReasoning)
			}
			if got := modelcat.IsMediumlessLadderModel(m.ID); got != m.Mediumless {
				t.Errorf("IsMediumlessLadderModel(%s) = %v, want %v", m.ID, got, m.Mediumless)
			}

			// 2. Mock upstream server simulating model response
			mock := testutil.NewMock()
			defer mock.Close()

			mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)

				// Model emits CLI-format run_terminal_command
				_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-audit", 1,
					`"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_b1","type":"function","function":{"name":"run_terminal_command","arguments":"{\"command\":\"git status\",\"cwd\":\"/repo\"}"}}]},"index":0}]`)))
				_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-audit", 1,
					`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":10,"total_tokens":30}`)))
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			}

			ts, _ := newTestServer(t, []string{"audit-key"}, mock)

			// 3. Send OMP chat completion request with OMP tools and reasoning_effort: medium
			reqBody := `{"model":"` + m.ID + `",` +
				`"reasoning_effort":"medium",` +
				`"messages":[{"role":"user","content":"check status"}],` +
				`"stream":true,` +
				`"tools":` + ompFloorTools() + `}`

			resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(reqBody), map[string]string{
				"Authorization": "Bearer audit-key",
			})
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
			}

			// 4. Verify upstream wire format
			recorded := mock.LastChatBody()
			if recorded == "" {
				t.Fatal("no chat body recorded upstream")
			}

			// Wire must have zero foreign riders
			for _, foreign := range []string{`"name":"eval"`, `"name":"mcp__`, `"parameters":{"type":"object","properties":{"i":`} {
				if strings.Contains(recorded, foreign) {
					t.Errorf("wire leaked foreign OMP rider %s: %s", foreign, truncate(recorded, 300))
				}
			}

			// Wire must have canonical floor tools + end_turn
			var parsed map[string]any
			if err := json.Unmarshal([]byte(recorded), &parsed); err != nil {
				t.Fatalf("upstream body invalid JSON: %v", err)
			}
			tools, ok := parsed["tools"].([]any)
			if !ok {
				t.Fatalf("upstream tools not an array: %v", parsed["tools"])
			}
			if len(tools) != 17 {
				t.Errorf("wire tool count = %d, want 17 (16 floor + end_turn)", len(tools))
			}

			// Verify reasoning_effort clamping on wire
			if m.Mediumless {
				// Mediumless ladder models (DeepSeek V4 Flash, GLM 5.3 Flash) clamp medium -> high
				if !strings.Contains(recorded, `"reasoning_effort":"high"`) {
					t.Errorf("model %s expected reasoning_effort clamped to high, got wire: %s", m.ID, truncate(recorded, 300))
				}
			} else if m.HasReasoning {
				// Other reasoning models retain or clamp to valid ladder
				if effort, ok := parsed["reasoning_effort"].(string); !ok || effort == "" {
					t.Errorf("model %s expected reasoning_effort on wire, got none", m.ID)
				}
			}

			// 5. Verify client received restored OMP tool call
			frames, done := collectOpenAIFrames(t, string(data))
			if !done {
				t.Error("stream missing [DONE] sentinel")
			}
			if !frameSetHasToolCall(frames, "bash") {
				t.Error("client stream missing restored 'bash' tool call (got un-restored run_terminal_command or suppressed)")
			}
		})
	}
}

// TestAllEightServedModelsTurn2ToolResultReplay verifies Turn 2 (assistant tool call
// + tool result submission) across all 8 models, validating that strict reasoning
// models (DeepSeek, MiMo) restore reasoning_content and set content: null when the
// client omits them.
func TestAllEightServedModelsTurn2ToolResultReplay(t *testing.T) {
	for _, m := range allServedModels {
		t.Run(m.ID+"/turn2", func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()

			callCount := 0
			turn1Reasoning := "Checking git status for repo"

			mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
				callCount++
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)

				if callCount == 1 {
					// Turn 1: Model emits reasoning + tool call
					_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-t1", 1,
						`"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"`+turn1Reasoning+`","tool_calls":[{"index":0,"id":"call_turn1","type":"function","function":{"name":"run_terminal_command","arguments":"{\"command\":\"git status\"}"}}]},"index":0}]`)))
					_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-t1", 1,
						`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":15,"total_tokens":25}`)))
					_, _ = io.WriteString(w, "data: [DONE]\n\n")
					return
				}

				// Turn 2: Model returns final content answer
				_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-t2", 2,
					`"choices":[{"index":0,"delta":{"role":"assistant","content":"All clean."},"index":0}]`)))
				_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-t2", 2,
					`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":30,"completion_tokens":5,"total_tokens":35}`)))
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			}

			ts, _ := newTestServer(t, []string{"audit-key"}, mock)

			// Turn 1 Request (stream: true)
			turn1Req := `{"model":"` + m.ID + `","messages":[{"role":"user","content":"status"}],"stream":true,"tools":` + ompFloorTools() + `}`
			resp1, data1 := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(turn1Req), map[string]string{"Authorization": "Bearer audit-key"})
			if resp1.StatusCode != http.StatusOK {
				t.Fatalf("turn 1 status = %d: %s", resp1.StatusCode, truncate(string(data1), 200))
			}

			// Turn 2 Request: Replay Turn 1 tool call (OMP client sends restored 'bash' call, tool result, without reasoning_content)
			turn2Req := `{
				"model":"` + m.ID + `",
				"stream":true,
				"messages":[
					{"role":"user","content":"status"},
					{"role":"assistant","content":null,"tool_calls":[{"id":"call_turn1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"git status\"}"}}]},
					{"role":"tool","tool_call_id":"call_turn1","content":"clean"}
				],
				"tools":` + ompFloorTools() + `
			}`
			resp2, data2 := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(turn2Req), map[string]string{"Authorization": "Bearer audit-key"})
			if resp2.StatusCode != http.StatusOK {
				t.Fatalf("turn 2 status = %d: %s", resp2.StatusCode, truncate(string(data2), 200))
			}

			// Verify Turn 2 upstream body
			recorded2 := mock.LastChatBody()
			if recorded2 == "" {
				t.Fatal("turn 2: no upstream body recorded")
			}

			// In Turn 2 upstream: 'bash' must be renamed back to 'run_terminal_command'
			if !strings.Contains(recorded2, `"run_terminal_command"`) {
				t.Errorf("turn 2 upstream missing renamed 'run_terminal_command': %s", truncate(recorded2, 300))
			}
			if strings.Contains(recorded2, `"name":"bash"`) {
				t.Errorf("turn 2 upstream leaked client name 'bash': %s", truncate(recorded2, 300))
			}

			// If model is StrictReasoning, verify reasoning_content was restored into assistant message
			if m.StrictReasoning {
				var parsed2 map[string]any
				if err := json.Unmarshal([]byte(recorded2), &parsed2); err != nil {
					t.Fatalf("turn 2 upstream body invalid JSON: %v", err)
				}
				msgs, _ := parsed2["messages"].([]any)
				var asstMsg map[string]any
				for _, msgItem := range msgs {
					mMap, _ := msgItem.(map[string]any)
					if mMap != nil && mMap["role"] == "assistant" {
						asstMsg = mMap
						break
					}
				}
				if asstMsg == nil {
					t.Fatalf("turn 2 expected assistant message in upstream body, got %#v", msgs)
				}
				rc, _ := asstMsg["reasoning_content"].(string)
				if rc != turn1Reasoning {
					t.Errorf("model %s strict reasoning_content not restored: got %q, want %q", m.ID, rc, turn1Reasoning)
				}
				if asstMsg["content"] != nil {
					t.Errorf("model %s assistant content = %v, want nil", m.ID, asstMsg["content"])
				}
			}
		})
	}
}
