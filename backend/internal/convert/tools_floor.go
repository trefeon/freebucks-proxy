package convert

import "encoding/json"

// Floor-only wire for OMP-family toolsets.
//
// Live bisect 2026-09-30 (5ac40785): OMP-tools + neutral-system 503s while
// official-tools + the full 77KB OMP system 200s. The gate keys on tool
// DEFINITIONS: any foreign-schema definition riding alongside the floor
// (mcp__ virtualized defs, find_files under a non-floor name) trips it.
// Substitution alone is insufficient — the riders must not ride at all.
//
// When the client toolset is OMP-family (intent-`i` schemas OMP injects per
// request, or OMP-signature tool names no other harness declares), the wire
// carries exactly the 16 canonical CLI definitions plus the end_turn/decide
// pins. Dropped client tools never reach the model, so their calls never
// need restoring; the model fills CLI-shaped args for the floor tools and
// the response leg reshapes them back (ReshapeArgs) + restores names.
//
// Non-OMP clients are untouched: their custom/virtualized tools keep riding
// with the top-up, exactly as before.

// ompSignatureNames are tool names unique to the OMP/pi family across the
// client corpus (verified: codex declares new_context+wait, pi declares
// find, Reasonix/kilocode declare task — none declare these). Two or more
// in one request identifies the family even when intent injection is off
// (PI_NO_INTENT).
var ompSignatureNames = map[string]bool{
	"eval":          true,
	"learn":         true,
	"manage_skill":  true,
	"context_notes": true,
}

// isOMPToolsetBody parses raw client tools from a request body for the
// family check. Invalid bodies report false (never floor-only on doubt).
func isOMPToolsetBody(body []byte) bool {
	var payload struct {
		Tools []any `json:"tools"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false
	}
	return isOMPToolset(payload.Tools)
}

// isOMPToolset reports whether raw client tools come from the OMP family:
// any parameters.properties carrying the injected `i` intent field, or two
// or more OMP-signature names.
func isOMPToolset(tools []any) bool {
	sig := 0
	seen := map[string]bool{}
	for _, t := range tools {
		m, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := m["function"].(map[string]any)
		if !ok {
			continue
		}
		if params, ok := fn["parameters"].(map[string]any); ok {
			if props, ok := params["properties"].(map[string]any); ok {
				if _, ok := props["i"]; ok {
					return true
				}
			}
		}
		if name, _ := fn["name"].(string); name != "" && ompSignatureNames[name] && !seen[name] {
			seen[name] = true
			sig++
			if sig >= 2 {
				return true
			}
		}
	}
	return false
}

// floorOnlyOMP rewrites payload["tools"] to the 16 canonical CLI definitions
// in fixture order, preserving the end_turn/decide pins already present.
// Non-floor entries (mcp__ virtualizations, find_files, unmapped customs)
// are dropped: they would trip the gate, and the model cannot miss tools it
// was never shown. A tool_choice pin naming a dropped wire name is removed
// (the OpenAI default is auto); pins on floor names are left for
// RenameRequestToolChoice, which runs after this pass.
func floorOnlyOMP(payload map[string]any) {
	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) == 0 {
		return
	}
	defs, err := canonicalToolDefs()
	if err != nil || len(defs) == 0 {
		return // fall back to substituted wire rather than fail the request
	}
	var pins []any
	for _, t := range tools {
		m, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := m["function"].(map[string]any)
		if !ok {
			continue
		}
		if name, _ := fn["name"].(string); name == "end_turn" || name == "decide" {
			pins = append(pins, t)
		}
	}
	wire := make([]any, 0, len(defs)+len(pins))
	wire = append(wire, defs...)
	wire = append(wire, pins...)
	payload["tools"] = wire
	if tc, ok := payload["tool_choice"].(map[string]any); ok {
		if fn, ok := tc["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); name != "" {
				keep := false
				for _, t := range wire {
					m, _ := t.(map[string]any)
					f, _ := m["function"].(map[string]any)
					if n, _ := f["name"].(string); n == name {
						keep = true
						break
					}
				}
				if !keep {
					delete(payload, "tool_choice")
				}
			}
		}
	}
}
