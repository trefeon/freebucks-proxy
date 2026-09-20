package server_test

// Strict tool-calling translation layer tests (strict_tools.go):
//
//   - Loose clients (Hermes terminal, OpenClaw bash — no strict flag) pass
//     through byte-identical, and unusable upstream arguments still fall back
//     to the legacy {} input.
//   - A tool declaring strict:true must carry parameters.type=object with
//     required[] covering every declared property and
//     additionalProperties:false on all three ingress shapes (chat tools[],
//     Responses flat tools, Anthropic input_schema), else 400
//     strict_violation before any upstream call. On the chat shape both
//     strict placements count (function.strict and the top-level marker).
//   - Replayed tool history for a strict tool (Responses function_call,
//     Anthropic tool_use including case variants and server_tool_use, which
//     the converter replays as tool_calls) with unusable arguments fails
//     with 400 invalid_tool_arguments instead of coercing to "{}".
//   - Unusable (bad-JSON or empty) arguments returned for a strict:true tool
//     fail the Anthropic turn with 400 invalid_tool_arguments.

import (
	"net/http"
	"strings"
	"testing"

	"freebucks-proxy/backend/internal/testutil"
)

// TestStrictTools_LooseHermesTerminalPasses pins the loose Hermes path:
// a terminal tool with NO strict flag is accepted and renamed to the
// official run_terminal_command upstream (toolmap), with the schema
// forwarded verbatim (no strict marker injected, no validation applied).
func TestStrictTools_LooseHermesTerminalPasses(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: strict-tools lane excluded; run `go test ./backend/...` for the full tier")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = responsesChunks()
	ts, _ := newTestServer(t, nil, mock)
	// NOTE: no "strict" key at all — the Hermes shape.
	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"run it"}],` +
		`"tools":[{"type":"function","function":{"name":"terminal","description":"Run a command",` +
		`"parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}}]}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
	}
	recorded := mock.LastChatBody()
	// terminal renames to the official run_terminal_command on the wire
	// (toolmap); the schema around it stays verbatim.
	if !strings.Contains(recorded, `"run_terminal_command"`) {
		t.Errorf("upstream body missing renamed loose tool: %s", truncate(recorded, 300))
	}
	if strings.Contains(recorded, "strict") {
		t.Errorf("loose tool gained a strict marker upstream: %s", truncate(recorded, 300))
	}
}

// TestStrictTools_LooseOpenClawBashPasses pins the loose OpenClaw path on
// the Responses surface: a flat bash tool with NO strict flag is accepted.
func TestStrictTools_LooseOpenClawBashPasses(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: strict-tools lane excluded; run `go test ./backend/...` for the full tier")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = responsesChunks()
	ts, _ := newTestServer(t, nil, mock)

	// NOTE: no "strict" key at all — the OpenClaw shape.
	body := `{"model":"` + modelA + `","input":"run it",` +
		`"tools":[{"type":"function","name":"bash","description":"Run a command",` +
		`"parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}]}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
	}
	recorded := mock.LastChatBody()
	// bash renames to the official run_terminal_command on the wire
	// (toolmap, pre-existing); the schema around it stays verbatim.
	if !strings.Contains(recorded, `"run_terminal_command"`) {
		t.Errorf("upstream body missing renamed loose tool: %s", truncate(recorded, 300))
	}
	if strings.Contains(recorded, "strict") {
		t.Errorf("loose tool gained a strict marker upstream: %s", truncate(recorded, 300))
	}
}

// TestStrictTools_LooseBadJSONArgsFallback pins the legacy {} fallback: a
// LOOSE Anthropic tool whose upstream arguments are not valid JSON still
// delivers a tool_use block with an empty-object input (Hermes/OpenClaw
// unchanged).
func TestStrictTools_LooseBadJSONArgsFallback(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: strict-tools lane excluded; run `go test ./backend/...` for the full tier")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-sl1", 201,
		`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"toolu_01","type":"function","function":{"name":"get_weather","arguments":"not-json-at-all"}}]},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-sl1", 201, `"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]`))
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"weather?"}],` +
		`"tools":[{"name":"get_weather","description":"weather lookup",` +
		`"input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	if !strings.Contains(string(data), `"input":{}`) {
		t.Errorf("loose tool_use input != {} fallback: %s", truncate(string(data), 300))
	}
}

// TestStrictTools_ChatCloser rejects strict:true chat tools whose schema
// violates the strict contract, and accepts a fully strict declaration.
func TestStrictTools_ChatCloser(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: strict-tools lane excluded; run `go test ./backend/...` for the full tier")
	}
	cases := []struct {
		name       string
		function   string // raw function-object JSON (inside {"type":"function","function":...})
		wantStatus int
		wantErr    string
	}{
		{
			"missing-required",
			`{"name":"run_it","description":"Run it","strict":true,` +
				`"parameters":{"type":"object","properties":{"command":{"type":"string"}}},` +
				`"additionalProperties":false}`,
			http.StatusBadRequest, "strict_violation",
		},
		{
			"extra-prop-not-required",
			`{"name":"run_it","description":"Run it","strict":true,` +
				`"parameters":{"type":"object","properties":{"command":{"type":"string"},"cwd":{"type":"string"}}},` +
				`"required":["command"],"additionalProperties":false}`,
			http.StatusBadRequest, "strict_violation",
		},
		{
			"no-additionalProperties-false",
			`{"name":"run_it","description":"Run it","strict":true,` +
				`"parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}`,
			http.StatusBadRequest, "strict_violation",
		},
		{
			"additionalProperties-true",
			`{"name":"run_it","description":"Run it","strict":true,` +
				`"parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"],"additionalProperties":true}}`,
			http.StatusBadRequest, "strict_violation",
		},
		{
			"non-object-type",
			`{"name":"run_it","description":"Run it","strict":true,` +
				`"parameters":{"type":"array","items":{"type":"string"}},"required":[],"additionalProperties":false}`,
			http.StatusBadRequest, "strict_violation",
		},
		{
			// Roo-style sibling placement, fully strict: accepted (the
			// round-trip marker assertions live in conformance_roo_test.go).
			"valid-strict-passes",
			`{"name":"run_it","description":"Run it","strict":true,` +
				`"parameters":{"type":"object","properties":{"command":{"type":"string"}}},` +
				`"required":["command"],"additionalProperties":false}`,
			http.StatusOK, "",
		},
		{
			// Inline placement (markers inside parameters): accepted too.
			"valid-strict-inline-passes",
			`{"name":"run_it","description":"Run it","strict":true,` +
				`"parameters":{"type":"object","properties":{"command":{"type":"string"}},` +
				`"required":["command"],"additionalProperties":false}}`,
			http.StatusOK, "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			mock.ChatBody = responsesChunks()
			ts, _ := newTestServer(t, nil, mock)
			body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"run it"}],` +
				`"tools":[{"type":"function","function":` + tc.function + `}]}`
			resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), nil)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.wantStatus, truncate(string(data), 200))
			}
			if tc.wantErr != "" {
				if !strings.Contains(string(data), tc.wantErr) {
					t.Errorf("error body missing %q: %s", tc.wantErr, truncate(string(data), 200))
				}
				if mock.RequestsSnapshot() != 0 {
					t.Errorf("upstream requests = %d, want 0 (rejected before pool)", mock.RequestsSnapshot())
				}
			}
		})
	}
}

// TestStrictTools_ResponsesCloser pins the closer on the flat Responses
// function tools: a strict tool missing required fails with strict_violation
// before any upstream call.
func TestStrictTools_ResponsesCloser(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: strict-tools lane excluded; run `go test ./backend/...` for the full tier")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = responsesChunks()
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"` + modelA + `","input":"run it",` +
		`"tools":[{"type":"function","name":"run_it","description":"Run it","strict":true,` +
		`"parameters":{"type":"object","properties":{"command":{"type":"string"}}}}]}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, truncate(string(data), 200))
	}
	if !strings.Contains(string(data), "strict_violation") {
		t.Errorf("error body missing strict_violation: %s", truncate(string(data), 200))
	}
	if mock.RequestsSnapshot() != 0 {
		t.Errorf("upstream requests = %d, want 0 (rejected before pool)", mock.RequestsSnapshot())
	}
}

// TestStrictTools_AnthropicCloser pins the closer on Anthropic
// input_schema tools, in the Anthropic error envelope.
func TestStrictTools_AnthropicCloser(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: strict-tools lane excluded; run `go test ./backend/...` for the full tier")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = responsesChunks()
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"run it"}],` +
		`"tools":[{"name":"run_it","description":"Run it","strict":true,` +
		`"input_schema":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}]}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, truncate(string(data), 200))
	}
	got := string(data)
	if !strings.Contains(got, "strict_violation") {
		t.Errorf("error body missing strict_violation: %s", truncate(got, 200))
	}
	if !strings.Contains(got, `"type":"error"`) {
		t.Errorf("error body missing Anthropic envelope: %s", truncate(got, 200))
	}
	if mock.RequestsSnapshot() != 0 {
		t.Errorf("upstream requests = %d, want 0 (rejected before pool)", mock.RequestsSnapshot())
	}
}

// TestStrictTools_StrictBadJSONArgs pins the response-side gate: unusable
// upstream arguments for a STRICT tool fail the Anthropic turn with 400
// invalid_tool_arguments (bad-JSON and empty variants).
func TestStrictTools_StrictBadJSONArgs(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: strict-tools lane excluded; run `go test ./backend/...` for the full tier")
	}
	strictTool := `"tools":[{"name":"get_strict","description":"strict lookup","strict":true,` +
		`"input_schema":{"type":"object","properties":{"city":{"type":"string"}},` +
		`"required":["city"],"additionalProperties":false}}]`
	for _, tc := range []struct {
		name string
		args string // raw upstream arguments string
	}{
		{"bad-json", `not-json-at-all`},
		{"empty", ``},
		{"null-literal", `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			argsJSON := `"arguments":"` + tc.args + `"`
			if tc.args == "" {
				argsJSON = `"arguments":""`
			}
			// "null" literal is valid JSON decoding to nil -> still unusable.
			mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-ss1", 201,
				`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"toolu_09","type":"function","function":{"name":"get_strict",`+argsJSON+`}}]},"finish_reason":null}]`)) +
				testutil.SSEEvent(chunk("chatcmpl-ss1", 201, `"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]`))
			ts, _ := newTestServer(t, nil, mock)

			body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"weather?"}],` + strictTool + `}`
			resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), nil)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", resp.StatusCode, truncate(string(data), 300))
			}
			if !strings.Contains(string(data), "invalid_tool_arguments") {
				t.Errorf("error body missing invalid_tool_arguments: %s", truncate(string(data), 300))
			}
		})
	}
}

// TestStrictTools_StrictValidArgsPass pins the other side of the gate: a
// strict:true tool with well-formed arguments still delivers the tool_use
// block.
func TestStrictTools_StrictValidArgsPass(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: strict-tools lane excluded; run `go test ./backend/...` for the full tier")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-ss2", 201,
		`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"toolu_09","type":"function","function":{"name":"get_strict","arguments":"{\"city\":\"SF\"}"}}]},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-ss2", 201, `"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]`))
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"weather?"}],` +
		`"tools":[{"name":"get_strict","description":"strict lookup","strict":true,` +
		`"input_schema":{"type":"object","properties":{"city":{"type":"string"}},` +
		`"required":["city"],"additionalProperties":false}}]}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	if !strings.Contains(string(data), `"city":"SF"`) {
		t.Errorf("tool_use input missing parsed arguments: %s", truncate(string(data), 300))
	}
	// The strict declaration survives conversion onto the upstream wire.
	if !mock.BodyContains(`"strict":true`) {
		t.Errorf("upstream body missing forwarded strict marker: %s", truncate(mock.LastChatBody(), 300))
	}
}

// TestStrictTools_AnthropicReplayCaseParity pins the replay gate to exactly
// the converter's replay set (anthropicAssistantToOpenAI case-folds the block
// type and replays tool_use plus server_tool_use as tool_calls): an exact,
// case-variant ("Tool_Use") or server_tool_use block carrying unusable
// arguments for a strict tool fails with 400 invalid_tool_arguments before
// any upstream call. The loose control proves the same block shape for a
// non-strict tool still delivers ({} fallback byte-identical).
func TestStrictTools_AnthropicReplayCaseParity(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: strict-tools lane excluded; run `go test ./backend/...` for the full tier")
	}
	strictTool := `"tools":[{"name":"get_strict","description":"strict lookup","strict":true,` +
		`"input_schema":{"type":"object","properties":{"city":{"type":"string"}},` +
		`"required":["city"],"additionalProperties":false}}]`
	replayBody := func(tools, blockType string) []byte {
		return []byte(`{"model":"` + modelA + `","messages":[` +
			`{"role":"user","content":"weather?"},` +
			`{"role":"assistant","content":[{"type":"` + blockType + `","id":"toolu_1",` +
			`"name":"get_strict","input":"not-json"}]}]` +
			`,` + tools + `}`)
	}
	for _, tc := range []struct {
		name      string
		blockType string
	}{
		{"exact", "tool_use"},
		{"case-variant", "Tool_Use"},
		{"server", "server_tool_use"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			mock.ChatBody = responsesChunks()
			ts, _ := newTestServer(t, nil, mock)

			resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", replayBody(strictTool, tc.blockType), nil)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", resp.StatusCode, truncate(string(data), 300))
			}
			if !strings.Contains(string(data), "invalid_tool_arguments") {
				t.Errorf("error body missing invalid_tool_arguments: %s", truncate(string(data), 300))
			}
			if mock.RequestsSnapshot() != 0 {
				t.Errorf("upstream requests = %d, want 0 (rejected before pool)", mock.RequestsSnapshot())
			}
		})
	}
	t.Run("loose-control", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.ChatBody = responsesChunks()
		ts, _ := newTestServer(t, nil, mock)

		looseTool := `"tools":[{"name":"get_strict","description":"loose echo",` +
			`"input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]`
		resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", replayBody(looseTool, "Tool_Use"), nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
		}
		if mock.RequestsSnapshot() == 0 {
			t.Errorf("upstream requests = 0, want >0 (loose history still replays)")
		}
	})
}

// TestStrictTools_ChatTopLevelStrictCloser pins the chat closer's top-level
// placement: a chat tool carrying strict ONLY beside type/function (not
// inside it) with a non-conforming schema still fails with 400
// strict_violation before any upstream call — the same acceptance the
// response-side lookup (strictToolsFromBody) counts.
func TestStrictTools_ChatTopLevelStrictCloser(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: strict-tools lane excluded; run `go test ./backend/...` for the full tier")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = responsesChunks()
	ts, _ := newTestServer(t, nil, mock)
	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"run it"}],` +
		`"tools":[{"type":"function","strict":true,"function":{"name":"run_it",` +
		`"description":"Run it","parameters":{"type":"object",` +
		`"properties":{"command":{"type":"string"}}}}}]}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, truncate(string(data), 200))
	}
	if !strings.Contains(string(data), "strict_violation") {
		t.Errorf("error body missing strict_violation: %s", truncate(string(data), 200))
	}
	if mock.RequestsSnapshot() != 0 {
		t.Errorf("upstream requests = %d, want 0 (rejected before pool)", mock.RequestsSnapshot())
	}
}
