package upstream

// topUpCliTools enforces the free-tier traffic-gate tool floor on the wire
// (gate shape: docs/FREE-TIER-GATE.md §4): after the convert layer's
// client-tool mapping step, the wire tools array is topped up with any of
// the 16 fixture definitions whose wire name is absent, compared by
// function.name.
//
// A request that carried no tools gets the full canonical 16. A request
// that DID carry tools keeps every one of its declarations verbatim and in
// order — a client-mapped tool WINS on collision, so a floor entry whose
// wire name is already present is skipped and a wire name is never
// duplicated (strict upstreams reject duplicate tool names).
//
// Floor entries are official names, so they pass the convert layer's
// RestoreName through untouched — no mapper entries are added for them,
// and response-side restore only renames what the request leg mapped.
//
// tool_choice is left alone: a client that sent none keeps none (the
// OpenAI default is auto).
func topUpCliTools(payload map[string]any) {
	rawTools, ok := payload["tools"].([]any)
	if !ok || len(rawTools) == 0 {
		payload["tools"] = defaultCliTools()
		return
	}
	present := make(map[string]bool, len(rawTools))
	for _, t := range rawTools {
		if name := wireFunctionName(t); name != "" {
			present[name] = true
		}
	}
	for _, def := range defaultCliTools() {
		name := wireFunctionName(def)
		if name == "" || present[name] {
			continue
		}
		rawTools = append(rawTools, def)
		present[name] = true
	}
	payload["tools"] = rawTools
}

// wireFunctionName extracts the function name from one tools-array entry,
// or "" when the entry is not a well-formed function tool.
func wireFunctionName(def any) string {
	m, ok := def.(map[string]any)
	if !ok {
		return ""
	}
	fn, ok := m["function"].(map[string]any)
	if !ok {
		return ""
	}
	name, _ := fn["name"].(string)
	return name
}
