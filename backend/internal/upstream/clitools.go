package upstream

import "strings"

// topUpCliTools enforces the free-tier traffic-gate tool floor on the wire
// (gate shape: docs/FREE-TIER-GATE.md §4):
// 1. Cleans client tools:
//   - Drops "decide" (a foreign/banned signature tool that trips 503 on upstream free tier).
//   - Strips " (client tool: ...)" annotations from descriptions (upstream gate detects
//     and rejects foreign client hints with 503).
//
// 2. Tops up any of the 16 canonical fixture definitions whose wire name is absent.
//
// A request that carried no tools gets the full canonical 16. A request
// that DID carry tools keeps its declarations (cleaned) and is topped up
// with missing official tools so the upstream gate always sees the complete
// 16-tool official set.
func topUpCliTools(payload map[string]any) {
	rawTools, ok := payload["tools"].([]any)
	if !ok || len(rawTools) == 0 {
		payload["tools"] = defaultCliTools()
		return
	}
	cleaned := make([]any, 0, len(rawTools))
	present := make(map[string]bool, len(rawTools))
	for _, t := range rawTools {
		name := wireFunctionName(t)
		if name == "decide" {
			continue
		}
		if tm, ok := t.(map[string]any); ok {
			if fn, ok := tm["function"].(map[string]any); ok {
				if desc, ok := fn["description"].(string); ok && strings.Contains(desc, " (client tool:") {
					fn["description"] = strings.TrimSpace(strings.Split(desc, " (client tool:")[0])
				}
			}
		}
		if name != "" {
			present[name] = true
		}
		cleaned = append(cleaned, t)
	}
	// Append any floor tools whose wire name is absent.
	for _, def := range defaultCliTools() {
		if name := wireFunctionName(def); name != "" && !present[name] {
			cleaned = append(cleaned, def)
		}
	}
	payload["tools"] = cleaned
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
