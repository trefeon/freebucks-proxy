package server

import "encoding/json"

// stripToolsEnvelope returns the request body with the tool-calling
// envelope removed (tools, tool_choice, plus the legacy functions /
// function_call aliases the normalizer folds into them), reporting
// whether a non-empty tools[] was present. Issue #729 (reopened #630
// symptom): upstream routing 404s "No endpoints found for <model>" on a
// tools-bearing request while the same body without tools succeeds, so
// the retry must resolve endpoints exactly like a tools-less request.
// Pure function, no I/O — the caller decides whether to re-issue.
func stripToolsEnvelope(body []byte) ([]byte, bool) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false
	}
	tools, _ := payload["tools"].([]any)
	if len(tools) == 0 {
		return nil, false
	}
	delete(payload, "tools")
	delete(payload, "tool_choice")
	delete(payload, "functions")
	delete(payload, "function_call")
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, false
	}
	return out, true
}
