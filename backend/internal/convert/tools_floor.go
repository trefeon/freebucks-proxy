package convert

import "encoding/json"

// Client toolset families whose response leg needs translation beyond the
// name restore every mapped client gets.
//
//   - familyOMP (Oh My Pi): floor-only wire (this file) + OMP-shaped arg
//     reshape + unroutable text fallback (tools_reshape.go,
//     tools_textfallback.go).
//   - familyPi (pi, the upstream OMP forked from): the wire already carries
//     official names with canonical substituted definitions for every pi
//     core tool (read/bash/powershell/edit/write/grep/find/ls all have
//     clientToOfficial entries), so nothing is floored — but responses still
//     need pi-shaped args (read {path}, edit {path, edits[]}, bash
//     {command, timeout}) and pi-unroutable floor calls (write_todos,
//     web_search, ask_user, …) must degrade to text, not relay a name pi's
//     dispatcher answers with "Tool <name> not found".
//
// Live bisect 2026-09-30 (5ac40785): OMP-tools + neutral-system 503s while
// official-tools + the full 77KB OMP system 200s. The gate keys on tool
// DEFINITIONS: any foreign-schema definition riding alongside the floor
// (mcp__ virtualized defs, unmapped customs) trips it. Substitution alone
// is insufficient — the riders must not ride at all. (OMP `find` is not a
// rider: it maps to the official floor name `glob` and restores shape-aware
// to find — see clientToOfficial + fanoutGlobArgs.)
//
// When the client toolset is OMP-family (intent-`i` schemas OMP injects per
// request, or OMP-signature tool names no other harness declares), the wire
// carries exactly the 16 canonical CLI definitions plus the end_turn/decide
// pins. Dropped client tools never reach the model, so their calls never
// need restoring; the model fills CLI-shaped args for the floor tools and
// the response leg reshapes them back (ReshapeArgs) + restores names.
//
// Non-family clients are untouched: their custom/virtualized tools keep
// riding with the top-up, exactly as before.
type clientFamily int

const (
	familyNone clientFamily = iota
	familyOMP
	familyPi
)

// ompSignatureNames are tool names unique to the OMP family across the
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

// piToolVocabulary is pi's complete built-in model-facing vocabulary
// (reference/harnesses/pi packages/coding-agent/src/core/tools/index.ts
// ToolName union; powershell is registered on Windows in place of bash).
// A request whose every declared tool name is in this set is pi or a
// pi-shaped client: across the 21-harness corpus only pi's own sets are
// subsets (opencode/Claude Code share some names but add glob/todowrite/
// PascalCase tools, so they never match). Reports from pi with extension/
// custom tools are not detected — extension schemas may themselves be
// foreign, and the family heuristic deliberately stays conservative.
var piToolVocabulary = map[string]bool{
	"read":       true,
	"bash":       true,
	"powershell": true,
	"edit":       true,
	"write":      true,
	"grep":       true,
	"find":       true,
	"ls":         true,
}

// isPiToolset reports whether every raw client tool name is pi vocabulary
// (pi's default session declares exactly read/bash/edit/write; read-only
// sessions declare read/grep/find/ls). At least two names are required: a
// single generic tool (`read`, `edit`) is every client's name and carries no
// family signal.
func isPiToolset(tools []any) bool {
	count := 0
	for _, t := range tools {
		m, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := m["function"].(map[string]any)
		if !ok {
			continue
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		if !piToolVocabulary[name] {
			return false
		}
		count++
	}
	return count >= 2
}

// hasPiEditFingerprint recognizes pi's batch edit schema — an `edit` tool
// whose parameters carry `edits` as an array of {oldText,newText} objects
// (packages/coding-agent/src/core/tools/edit.ts:34-54, the schema every pi
// session ships). This is the detection path for a pi session carrying
// extension/custom tools (subagent spawners, MCP bridges, …): the extra
// names leave piToolVocabulary, so the name-subset rule cannot see it, but
// the core edit schema still can. OMP never advertises this shape (its
// replace mode is old_string/new_string, its patch mode op/diff) and no
// other corpus harness declares a batch edits[] edit.
func hasPiEditFingerprint(tools []any) bool {
	for _, t := range tools {
		m, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := m["function"].(map[string]any)
		if !ok {
			continue
		}
		if name, _ := fn["name"].(string); name != "edit" {
			continue
		}
		params, _ := fn["parameters"].(map[string]any)
		props, _ := params["properties"].(map[string]any)
		edits, _ := props["edits"].(map[string]any)
		items, _ := edits["items"].(map[string]any)
		itemProps, _ := items["properties"].(map[string]any)
		if _, ok := itemProps["oldText"]; !ok {
			continue
		}
		if _, ok := itemProps["newText"]; !ok {
			continue
		}
		return true
	}
	return false
}

// detectClientFamily classifies the raw client tools for the translation
// layer. OMP is checked first: a pi-vocabulary-only subset (e.g. an OMP
// session stripped to read/bash/edit/write, or with intent tracing off)
// would otherwise read as pi, and the two families restore different arg
// shapes. pi matches the bare vocabulary subset or the schema fingerprint.
func detectClientFamily(tools []any) clientFamily {
	if isOMPToolset(tools) {
		return familyOMP
	}
	if isPiToolset(tools) || hasPiEditFingerprint(tools) {
		return familyPi
	}
	return familyNone
}

// detectFamilyBody parses raw client tools from a request body for the
// family check. Invalid bodies report familyNone (never translate a family
// on doubt).
func detectFamilyBody(body []byte) clientFamily {
	var payload struct {
		Tools []any `json:"tools"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return familyNone
	}
	return detectClientFamily(payload.Tools)
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
// Non-floor entries (mcp__ virtualizations, unmapped customs) are dropped:
// they would trip the gate, and the model cannot miss tools it was never
// shown. A tool_choice pin naming a dropped wire name is removed
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

// floorFallbacks routes floor tools the OMP family never declares to the
// OMP equivalent: the model sees all 16 floor defs (gate requirement) but
// OMP dispatches only its own names. list_directory rides arg-verbatim
// (CLI {path} is already valid OMP read shape). The three remaining floor
// names have no OMP equivalent at all and are handled by the text fallback
// (tools_textfallback.go): the response leg suppresses the call and renders
// its payload as assistant text (or absorbs it) instead of relaying a name
// the OMP dispatcher rejects with "not found".
var floorFallbacks = map[string]string{
	// Declared by a full OMP toolset; also registered unconditionally so a
	// trimmed toolset (--tools, PI_NO_INTENT) can never leak a wire-only
	// name: resolution must be total, not dependent on what the client
	// happened to declare. First-claim-wins, so a real mapping still owns
	// the pair.
	"run_terminal_command": "bash",
	"read_files":           "read",
	"str_replace":          "edit",
	"write_file":           "write",
	"code_search":          "grep",
	"glob":                 "glob",
	"write_todos":          "todo",
	"web_search":           "web_search",
	// Never declared by OMP: the floor still rides (gate requirement) and
	// these are the only restore target for the call.
	"ask_user":       "ask",
	"read_url":       "read",
	"list_directory": "read",
	"skill":          "read",
	"gravity_index":  "web_search",
}

// RegisterFloorFallbacks records the fallback routes on the mapper so
// response calls to undeclared floor tools restore + reshape to the OMP
// equivalent. Never overrides a real mapping (first claim wins).
func (m *ToolMapper) RegisterFloorFallbacks() {
	if m.upstreamToClient == nil || m.clientToUpstream == nil {
		return
	}
	for wire, client := range floorFallbacks {
		if _, taken := m.upstreamToClient[wire]; !taken {
			m.upstreamToClient[wire] = client
		}
		if _, taken := m.clientToUpstream[client]; !taken {
			m.clientToUpstream[client] = wire
		}
	}
}
