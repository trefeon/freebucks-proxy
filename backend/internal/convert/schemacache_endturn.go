package convert

// injectEndTurnTool appends the end_turn pseudo-tool and the genuine custom
// signature tool decide to pass Codebuff foreign_toolset validation.
// Existing tools are never duplicated.
func injectEndTurnTool(payload map[string]any, tools []any, hasEndTurn bool, hasDecide bool) {
	raw, ok := payload["tools"].([]any)
	if !ok {
		raw = tools
	}
	if !hasEndTurn {
		raw = append(raw, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "end_turn",
				"description": "Signal the end of the current task.",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			},
		})
	}
	if !hasDecide {
		raw = append(raw, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "decide",
				"description": "Decide next step or action.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"choice": map[string]any{"type": "string"},
					},
				},
			},
		})
	}
	payload["tools"] = raw
}
