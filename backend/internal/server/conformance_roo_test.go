package server_test

// Conformance replay for Roo Code's strict function tools against the
// OpenAI chat/completions surface. Roo converts every function schema for
// OpenAI strict mode (reference/agents/Roo-Code/WIRE-NOTES.md §5:
// base-provider.ts:33-110). Under the canonical-substitution policy the
// wire carries the canonical CLI definition (the gate requires CLI
// definitions, not client schemas), so the client's strict flag is
// superseded while the canonical required/additionalProperties shape
// survives; only the tool NAME restores downstream.

import (
	"freebuff-proxy/backend/internal/testutil"
	"net/http"
	"strings"
	"testing"
)

func TestConformanceRooStrictSchemaPreserved(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: conformance lane excluded; run `go test ./backend/...` for the full tier")
	}
	mock := testutil.NewMock()
	defer mock.Close()
	// Non-streaming: single JSON completion answering a native tool call.
	mock.ChatBody = `{"id":"chatcmpl-roo1","object":"chat.completion","created":900,"model":"` + modelA + `",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":null,` +
		`"tool_calls":[{"index":0,"id":"call_roo_1","type":"function","function":{"name":"execute_command","arguments":"{\"command\":\"ls\"}"}}]},` +
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":60,"completion_tokens":20,"total_tokens":80}}`
	ts, _ := newTestServer(t, nil, mock)

	// Roo strict shape: strict:true + additionalProperties:false + full
	// required list on the function parameters.
	body := `{"model":"` + modelA + `",` +
		`"messages":[{"role":"user","content":"list files"}],` +
		`"tools":[{"type":"function","function":{"name":"execute_command","description":"Run a command",` +
		`"strict":true,"parameters":{"type":"object","properties":{"command":{"type":"string"}}},` +
		`"required":["command"],"additionalProperties":false}}],` +
		`"tool_choice":"auto"}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
	}

	// The upstream sees the canonical CLI definition under the renamed
	// tool (substitution policy): canonical required/additionalProperties
	// survive, the client's strict flag is superseded by the canonical
	// def (which carries no strict marker).
	recorded := mock.LastChatBody()
	for _, want := range []string{`"additionalProperties":false`, `"required":["command"]`} {
		if !strings.Contains(recorded, want) {
			t.Errorf("upstream body missing canonical marker %s: %s", want, truncate(recorded, 500))
		}
	}
	if !strings.Contains(recorded, `"run_terminal_command"`) {
		t.Errorf("upstream body missing renamed tool: %s", truncate(recorded, 400))
	}
}
