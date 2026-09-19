package convert

import "strings"

// FromUpstreamChunk restores client tool names in one upstream SSE chunk or
// completion object, IN PLACE: walks choices[].delta.tool_calls[] and
// choices[].message.tool_calls[], rewriting function.name. Returns the chunk
// unchanged (same semantics as callers' other in-place mutators). raw is the
// already-marshaled JSON; callers re-marshal only when this returns true.
func (m ToolMapper) FromUpstreamChunk(chunk map[string]any) bool {
	changed := false
	restore := func(fn map[string]any) {
		if fn == nil {
			return
		}
		name, _ := fn["name"].(string)
		if name == "" {
			return
		}
		// Single restore path: RestoreName is map-first and strips the mcp__
		// prefix only when the request leg added it, so both chunk shapes
		// share one collision-free semantic.
		if orig := m.RestoreName(name); orig != name {
			fn["name"] = orig
			changed = true
		}
	}
	choices, ok := chunk["choices"].([]any)
	if !ok {
		return false
	}
	for _, raw := range choices {
		choice, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if delta, ok := choice["delta"].(map[string]any); ok {
			if tcs, ok := delta["tool_calls"].([]any); ok {
				for _, tc := range tcs {
					tcMap, _ := tc.(map[string]any)
					if tcMap == nil {
						continue
					}
					fn, _ := tcMap["function"].(map[string]any)
					restore(fn)
				}
			}
		}
		if msg, ok := choice["message"].(map[string]any); ok {
			if tcs, ok := msg["tool_calls"].([]any); ok {
				for _, tc := range tcs {
					tcMap, _ := tc.(map[string]any)
					if tcMap == nil {
						continue
					}
					fn, _ := tcMap["function"].(map[string]any)
					restore(fn)
				}
			}
		}
	}
	return changed
}

// RestoreName maps one upstream tool name back to the client's original
// (identity when unmapped). Used by the Anthropic stream state machine,
// which tracks names outside the chunk-JSON shapes above.
//
// Map-first: consult upstreamToClient, then strip the mcp__ prefix ONLY when
// the request leg added it (the stripped name maps back to this exact
// upstream name in clientToUpstream, i.e. a virtualized foreign-harness
// name). A client-native mcp__* name passed through verbatim on the request
// leg — no map entry — so it passes through verbatim here instead of being
// corrupted by a blind prefix strip.
func (m ToolMapper) RestoreName(name string) string {
	if orig, ok := m.upstreamToClient[name]; ok {
		return orig
	}
	if strings.HasPrefix(name, "mcp__") {
		if stripped := strings.TrimPrefix(name, "mcp__"); stripped != "" {
			if up, ok := m.clientToUpstream[stripped]; ok && up == name {
				return stripped
			}
		}
	}
	return name
}

// Len reports how many names the mapper restores. Zero = identity mapper;
// relays use it to skip their restore pass entirely.
func (m ToolMapper) Len() int { return len(m.upstreamToClient) }
