package server_test

import (
	"encoding/json"
	"freebuff-proxy/backend/internal/testutil"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Phase 3 client surfaces, second half: the OMP floor-capability reminder
// rides the shared envelope on every surface (§3.1), Codex apply_patch
// strictness (§3.3), OpenClaw transport fit (§3.5), Hermes + Claude Code
// baselines (§3.6). Streaming reshape/fan-out/fallback parity lives in
// conformance_phase3_streaming_test.go +
// conformance_phase3_responses_test.go.

// floorReminderSentinel guards the prompt nudge appended for OMP-floor-only
// turns (upstream/chat.go ompFloorReminderSentinel). Schema stays
// authoritative; the prompt is advisory — this pins the nudge is present,
// never that the model obeyed it.
const floorReminderSentinel = "callable by name with these exact arguments though absent from tools"

// phase3ContentTurn serves one upstream text turn for non-streaming relays.
func phase3ContentTurn(w http.ResponseWriter, id string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, testutil.SSEEvent(chunk(id, 1,
		`"choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]`)))
	_, _ = io.WriteString(w, testutil.SSEEvent(chunk(id, 1,
		`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}`)))
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

// TestPhase3FloorReminderAllSurfaces pins the §3.1 reminder equivalent: an
// OMP-family turn carries the harness-only capability nudge in its upstream
// system message on chat, Anthropic, AND Responses — while the wire keeps
// the 16 canonical defs with zero riders.
func TestPhase3FloorReminderAllSurfaces(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	t.Run("chat", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) { phase3ContentTurn(w, "cmpl-p3r1") }
		ts, _ := newTestServer(t, nil, mock)
		body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"go"}],"stream":false,"tools":` + ompFloorTools() + `}`
		resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
		}
		assertFloorOnlyWire(t, mock)
		if recorded := mock.LastChatBody(); !strings.Contains(recorded, floorReminderSentinel) {
			t.Errorf("upstream chat body missing floor reminder: %s", truncate(recorded, 400))
		}
	})
	t.Run("anthropic", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) { phase3ContentTurn(w, "cmpl-p3r2") }
		ts, _ := newTestServer(t, nil, mock)
		body := `{"model":"` + modelA + `","max_tokens":256,"messages":[{"role":"user","content":"go"}],"tools":` + anthropicFloorTools() + `,"stream":false}`
		resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body),
			map[string]string{"Content-Type": "application/json", "anthropic-version": "2023-06-01"})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
		}
		assertFloorOnlyWire(t, mock)
		if recorded := mock.LastChatBody(); !strings.Contains(recorded, floorReminderSentinel) {
			t.Errorf("upstream body (via Anthropic) missing floor reminder: %s", truncate(recorded, 400))
		}
	})
	t.Run("responses", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) { phase3ContentTurn(w, "cmpl-p3r3") }
		ts, _ := newTestServer(t, nil, mock)
		body := `{"model":"` + modelA + `","input":[{"role":"user","content":[{"type":"input_text","text":"go"}]}],"stream":false,"tools":` + responsesFloorTools() + `}`
		resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
		}
		assertFloorOnlyWire(t, mock)
		if recorded := mock.LastChatBody(); !strings.Contains(recorded, floorReminderSentinel) {
			t.Errorf("upstream body (via Responses) missing floor reminder: %s", truncate(recorded, 400))
		}
	})
}

// TestPhase3NonFamilyNoReminder pins the negative: a generic turn never
// carries the OMP nudge on any surface (byte-identical envelope).
func TestPhase3NonFamilyNoReminder(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) { phase3ContentTurn(w, "cmpl-p3r4") }
	ts, _ := newTestServer(t, nil, mock)
	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"go"}],"stream":false,` +
		`"tools":[{"type":"function","function":{"name":"bash","description":"b","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}}]}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	if recorded := mock.LastChatBody(); strings.Contains(recorded, floorReminderSentinel) {
		t.Errorf("generic turn carries the OMP reminder: %s", truncate(recorded, 300))
	}
}

// TestPhase3CodexApplyPatchStrictness pins §3.3 Responses-only strictness
// for the Codex shape: a strict:false apply_patch tool with a freeform
// input schema rides the gate (no 503), the model call restores under the
// client name with byte-identical args, and the turn terminates with the
// usage triple Codex requires. Auth story: the recipe key (env_key →
// proxy key) arrives as Bearer; a missing credential 401s like every
// other surface (asserted here against a keyed server).
func TestPhase3CodexApplyPatchStrictness(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	codexBody := func() string {
		return `{"model":"` + modelA + `",` +
			`"instructions":"You are a helpful coding agent.",` +
			`"input":[{"role":"user","content":[{"type":"input_text","text":"Add a helper."}]}],` +
			`"tools":[{"type":"function","name":"apply_patch","description":"Apply a patch","strict":false,` +
			`"parameters":{"type":"object","properties":{"input":{"type":"string"}},"required":["input"]}}],` +
			`"tool_choice":"auto","store":false,"stream":true,` +
			`"include":["reasoning.encrypted_content"],"reasoning":{"effort":"high"}}`
	}
	t.Run("turn", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, phase3ToolChunk("chatcmpl-cx1",
				phase3Call(0, "call_ap1", "apply_patch", `{"input":"*** Begin Patch\n*** End Patch"}`, true)))
			_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-cx1", 2,
				`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]`)))
			_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-cx1", 2,
				`"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}`)))
		}
		ts, _ := newTestServer(t, []string{"sk-codex"}, mock)
		resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(codexBody()),
			map[string]string{"Authorization": "Bearer sk-codex"})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
		}
		sse := string(data)
		if strings.Contains(sse, "[DONE]") {
			t.Error("stream must not relay [DONE] to codex")
		}
		if !mock.BodyContains(`"name":"apply_patch"`) {
			t.Errorf("upstream body missing apply_patch def/call: %s", truncate(mock.LastChatBody(), 300))
		}
		events := collectResponsesEvents(t, sse)
		done := phase3ResponsesDoneItems(events)
		if len(done) != 1 {
			t.Fatalf("function_call done items = %d, want 1: %s", len(done), truncate(sse, 400))
		}
		if done[0]["name"] != "apply_patch" {
			t.Errorf("item name = %v, want apply_patch", done[0]["name"])
		}
		if args, _ := done[0]["arguments"].(string); args != `{"input":"*** Begin Patch\n*** End Patch"}` {
			t.Errorf("item arguments = %q, want byte-identical freeform input", args)
		}
		completed := codexEventByType(events, "response.completed")
		if completed == nil {
			t.Fatal("stream missing terminal response.completed")
		}
		usage, _ := completed["response"].(map[string]any)["usage"].(map[string]any)
		for _, key := range []string{"input_tokens", "output_tokens", "total_tokens"} {
			if _, ok := usage[key]; !ok {
				t.Errorf("completed usage missing %s: %v", key, usage)
			}
		}
	})
	t.Run("auth", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) { phase3ContentTurn(w, "cmpl-p3cx") }
		ts, _ := newTestServer(t, []string{"sk-codex"}, mock)
		resp, _ := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(codexBody()), nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("bare codex request status = %d, want 401 (env_key is required)", resp.StatusCode)
		}
	})
}

// TestPhase3OpenClawTransportFit pins §3.5: OpenClaw's three transports
// (openai-completions | openai-responses | anthropic-messages) each carry
// a generic tool turn end to end with Bearer auth — instructions embed as
// the upstream system message on Responses, and every surface closes with
// its usage/stop contract.
func TestPhase3OpenClawTransportFit(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	chatTools := `"tools":[{"type":"function","function":{"name":"exec","description":"run a command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}}}]`
	ocTurn := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, phase3ToolChunk("cmpl-oc1",
			phase3Call(0, "call_oc1", "exec", `{"cmd":"ls"}`, true)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-oc1", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":12,"completion_tokens":4,"total_tokens":16}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	t.Run("chat", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.ChatHandler = ocTurn
		ts, _ := newTestServer(t, []string{"oc-key"}, mock)
		h := map[string]string{"Authorization": "Bearer oc-key"}
		body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"list"}],"stream":true,` + chatTools + `}`
		resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), h)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
		}
		frames, done := collectOpenAIFrames(t, string(data))
		if !done {
			t.Error("stream missing [DONE]")
		}
		if name := toolCallName(frames, 0); name != "exec" {
			t.Errorf("tool name = %q, want exec", name)
		}
		if fr, ok := findTerminalFinish(frames); !ok || fr != "tool_calls" {
			t.Errorf("terminal finish = %q, want tool_calls", fr)
		}
	})
	t.Run("responses", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.ChatHandler = ocTurn
		ts, _ := newTestServer(t, []string{"oc-key"}, mock)
		h := map[string]string{"Authorization": "Bearer oc-key"}
		body := `{"model":"` + modelA + `","instructions":"Be terse.",` +
			`"input":[{"role":"user","content":[{"type":"input_text","text":"list"}]}],"stream":false,` +
			`"tools":[{"type":"function","name":"exec","description":"run","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]}`
		resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), h)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
		}
		// instructions-embedding: the stateless gateway carries
		// instructions as the upstream system message.
		if recorded := mock.LastChatBody(); !strings.Contains(recorded, `"role":"system"`) || !strings.Contains(recorded, "Be terse.") {
			t.Errorf("upstream body missing embedded instructions: %s", truncate(recorded, 300))
		}
		var obj map[string]any
		if err := json.Unmarshal(data, &obj); err != nil {
			t.Fatalf("response not JSON: %v", err)
		}
		output, _ := obj["output"].([]any)
		found := false
		for _, o := range output {
			if om, _ := o.(map[string]any); om["type"] == "function_call" && om["name"] == "exec" {
				found = true
			}
		}
		if !found {
			t.Errorf("no exec function_call in output: %s", truncate(string(data), 300))
		}
		usage, _ := obj["usage"].(map[string]any)
		for _, key := range []string{"input_tokens", "output_tokens", "total_tokens"} {
			if _, ok := usage[key]; !ok {
				t.Errorf("responses object usage missing %s: %v", key, usage)
			}
		}
	})
	t.Run("anthropic", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.ChatHandler = ocTurn
		ts, _ := newTestServer(t, []string{"oc-key"}, mock)
		h := map[string]string{"Content-Type": "application/json", "x-api-key": "oc-key", "anthropic-version": "2023-06-01"}
		body := `{"model":"` + modelA + `","max_tokens":256,"system":"Be terse.",` +
			`"messages":[{"role":"user","content":"list"}],` +
			`"tools":[{"name":"exec","description":"run","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}}}}],"stream":false}`
		resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), h)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
		}
		var msg map[string]any
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatalf("response not JSON: %v", err)
		}
		found := false
		for _, b := range msg["content"].([]any) {
			if bm, _ := b.(map[string]any); bm["type"] == "tool_use" && bm["name"] == "exec" {
				found = true
				if bm["id"] == "" {
					t.Error("tool_use block missing id")
				}
			}
		}
		if !found {
			t.Errorf("no exec tool_use block: %s", truncate(string(data), 300))
		}
		if msg["stop_reason"] != "tool_use" {
			t.Errorf("stop_reason = %v, want tool_use", msg["stop_reason"])
		}
	})
}

// TestPhase3HermesChatBaseline pins §3.6 (P3): Hermes speaks the generic
// names-only chat path — its native tool names ride verbatim, restore by
// identity, and /v1/models answers the auto-detect probe.
func TestPhase3HermesChatBaseline(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	t.Run("models", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		ts, _ := newTestServer(t, []string{"h-key"}, mock)
		resp, data := doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil,
			map[string]string{"Authorization": "Bearer h-key"})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
		}
		if !strings.Contains(string(data), modelA) {
			t.Errorf("/v1/models missing served id %s: %s", modelA, truncate(string(data), 300))
		}
	})
	t.Run("chat", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, phase3ToolChunk("cmpl-he1",
				phase3Call(0, "call_he1", "browser_navigate", `{"url":"https://example.test"}`, true)))
			_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-he1", 1,
				`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":12,"completion_tokens":4,"total_tokens":16}`)))
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		}
		ts, _ := newTestServer(t, []string{"h-key"}, mock)
		h := map[string]string{"Authorization": "Bearer h-key"}
		body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"open it"}],"stream":true,"tools":[` +
			tool("browser_navigate", `{"type":"object","properties":{"url":{"type":"string"}}}`) + `]}`
		resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), h)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
		}
		frames, done := collectOpenAIFrames(t, string(data))
		if !done {
			t.Error("stream missing [DONE]")
		}
		if name := toolCallName(frames, 0); name != "browser_navigate" {
			t.Errorf("tool name = %q, want browser_navigate (verbatim)", name)
		}
		if args := joinToolArgs(frames, 0); args != `{"url":"https://example.test"}` {
			t.Errorf("args = %q, want verbatim", args)
		}
	})
}

// TestPhase3ClaudeCodeMappedBaseline pins §3.6 (P4, opportunistic): Claude
// Code's PascalCase set maps case-insensitively onto the official wire
// names (toolmap coverage: Claude-Code Bash→run_terminal_command), so the
// defs never ride as foreign riders — and restore to the client's exact
// casing, with every streamed tool_use carrying an id.
func TestPhase3ClaudeCodeMappedBaseline(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, phase3ToolChunk("cmpl-cc1",
			phase3Call(0, "call_cc1", "run_terminal_command", `{"command":"ls"}`, true)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-cc1", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":15,"completion_tokens":8,"total_tokens":23}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServer(t, []string{"cc-key"}, mock)
	h := map[string]string{"Content-Type": "application/json", "x-api-key": "cc-key", "anthropic-version": "2023-06-01"}
	body := `{"model":"` + modelA + `","max_tokens":256,"system":"You are Claude Code.",` +
		`"messages":[{"role":"user","content":"list"}],` +
		`"tools":[{"name":"Bash","description":"run a shell command","input_schema":{"type":"object","properties":{"command":{"type":"string"}}}},` +
		`{"name":"Read","description":"read a file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}}}}],` +
		`"stream":true}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), h)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	// Renamed on the wire (no foreign rider), never the bare client name.
	if recorded := mock.LastChatBody(); !strings.Contains(recorded, `"name":"run_terminal_command"`) {
		t.Errorf("upstream body missing renamed run_terminal_command: %s", truncate(recorded, 300))
	}
	if recorded := mock.LastChatBody(); strings.Contains(recorded, `"function":{"name":"Bash"`) {
		t.Errorf("upstream body carries the bare client name: %s", truncate(recorded, 300))
	}
	events := collectAnthropicEvents(t, string(data))
	starts := phase3ToolBlockStarts(events)
	if len(starts) != 1 {
		t.Fatalf("tool_use blocks = %d, want 1: %v", len(starts), starts)
	}
	if starts[0][1] != "Bash" {
		t.Errorf("block name = %q, want Bash (client dispatch name restored)", starts[0][1])
	}
	if starts[0][2] == "" {
		t.Error("streamed tool_use missing id (P4 hard requirement)")
	}
}
