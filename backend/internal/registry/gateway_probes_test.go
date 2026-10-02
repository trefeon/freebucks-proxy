package registry_test

// Definition-vs-gate probe matrix (UNIVERSAL-GATEWAY-PLAN.md Phase 0 item
// 0.2; verdicts recorded in docs/UNIVERSAL-GATEWAY-PROBES.md).
//
// The upstream free-tier gate keys on tool DEFINITIONS (live bisect
// 2026-09-30: OMP defs + neutral prompt 503; 16 canonical defs + full OMP
// system prompt 200). No live traffic runs here, so these probes drive the
// real request leg (convert.NormalizeRequestMappedOpts — rename → substitute
// → floor) hermetically and pin the wire property the gate verdict follows:
// exactly the 16 canonical CLI defs (+ end_turn/decide pins) and zero foreign
// riders means gate-green; anything riding alongside is gate risk.
//
// Location note: the probes live in registry (not convert/server) per the
// Phase-0 lane contract. This is package registry_test, so the convert import
// is test-only: the archtest matrix (non-test files) and convert's purity
// guard are unaffected, and there is no import cycle (convert never imports
// registry).

import (
	"encoding/json"
	"freebuff-proxy/backend/internal/convert"
	"strings"
	"testing"
)

// canonicalFloorNames is the 16-tool official floor every gate-green wire
// carries, in fixture order (backend/internal/upstream/testdata/cli-tools.json,
// mirrored at backend/internal/convert/testdata/cli-tools.json).
var canonicalFloorNames = []string{
	"read_files",
	"str_replace",
	"write_file",
	"run_terminal_command",
	"code_search",
	"glob",
	"list_directory",
	"write_todos",
	"web_search",
	"read_url",
	"ask_user",
	"suggest_followups",
	"gravity_index",
	"render_ui",
	"skill",
	"report_project_profile",
}

func probeChatBody(tools string) []byte {
	return []byte(`{"model":"gpt-6-luna","messages":[{"role":"user","content":"hi"}],"tools":` + tools + `}`)
}

func probeFn(name, params string) string {
	return `{"type":"function","function":{"name":"` + name + `","description":"probe ` + name + `","parameters":` + params + `}}`
}

func probeWire(t *testing.T, body []byte) (map[string]any, convert.ToolMapper) {
	t.Helper()
	renamed, mapper, err := convert.NormalizeRequestMappedOpts(body, "", convert.DefaultOptions())
	if err != nil {
		t.Fatalf("NormalizeRequestMappedOpts: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(renamed, &payload); err != nil {
		t.Fatalf("unmarshal wire body: %v", err)
	}
	return payload, mapper
}

func probeWireNames(t *testing.T, payload map[string]any) []string {
	t.Helper()
	raw, ok := payload["tools"].([]any)
	if !ok {
		t.Fatalf("wire has no tools array: %T", payload["tools"])
	}
	names := make([]string, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("wire tool is not an object: %T", item)
		}
		fn, ok := m["function"].(map[string]any)
		if !ok {
			t.Fatalf("wire tool has no function block: %v", m)
		}
		name, _ := fn["name"].(string)
		names = append(names, name)
	}
	return names
}

// probeNoIntentProps fails when any wire def still carries the OMP
// injected intent `i` property: a foreign-schema rider the gate 503s.
func probeNoIntentProps(t *testing.T, payload map[string]any) {
	t.Helper()
	raw := payload["tools"].([]any)
	for _, item := range raw {
		fn := item.(map[string]any)["function"].(map[string]any)
		params, _ := fn["parameters"].(map[string]any)
		props, _ := params["properties"].(map[string]any)
		if _, ok := props["i"]; ok {
			t.Errorf("wire def %q still carries intent `i` schema", fn["name"])
		}
		if desc, _ := fn["description"].(string); strings.Contains(desc, " (client tool:") {
			t.Errorf("wire def %q carries a foreign client-tool annotation", fn["name"])
		}
	}
}

// TestGatewayProbeOMPFloorIsGateGreen pins the one bisected-green shape: an
// OMP-family toolset (intent-`i` schemas + OMP-only names) exits the request
// leg as exactly the 16 canonical defs in fixture order plus the
// end_turn/decide pins — zero riders, zero intent schemas.
func TestGatewayProbeOMPFloorIsGateGreen(t *testing.T) {
	tools := "[" +
		probeFn("bash", `{"type":"object","properties":{"command":{"type":"string"},"i":{"type":"string"}},"required":["command","i"]}`) + "," +
		probeFn("read", `{"type":"object","properties":{"path":{"type":"string"},"i":{"type":"string"}},"required":["path","i"]}`) + "," +
		probeFn("eval", `{"type":"object","properties":{"language":{"type":"string"},"code":{"type":"string"}}}`) + "," +
		probeFn("task", `{"type":"object","properties":{"agent":{"type":"string"},"task":{"type":"string"},"context":{"type":"string"},"i":{"type":"string"}},"required":["task"]}`) +
		"]"
	payload, mapper := probeWire(t, probeChatBody(tools))
	if !mapper.FloorOnly() {
		t.Fatalf("OMP toolset not detected as floor-only")
	}
	names := probeWireNames(t, payload)
	want := append(append([]string{}, canonicalFloorNames...), "end_turn", "decide")
	if len(names) != len(want) {
		t.Fatalf("wire carries %d defs, want %d (16 floor + 2 pins): %v", len(names), len(want), names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("wire def %d = %q, want %q (full order %v)", i, names[i], want[i], names)
		}
	}
	probeNoIntentProps(t, payload)
}

// TestGatewayProbeCodexApplyPatchRidesNamesOnly pins the unbisected Codex
// shape (UNIVERSAL-TOOLS-AUDIT.md §5; codex WIRE-NOTES §4): a native
// apply_patch def with a freeform `input` string is NOT floor material — it
// rides with the client's own schema (no canonical substitution exists for a
// name outside the 16-def fixture), so its gate verdict stays unbisected and
// Phase-3 arg-rule work must bisect it before touching the shape.
func TestGatewayProbeCodexApplyPatchRidesNamesOnly(t *testing.T) {
	tools := "[" + probeFn("apply_patch", `{"type":"object","properties":{"input":{"type":"string"}}}`) + "]"
	payload, mapper := probeWire(t, probeChatBody(tools))
	if mapper.FloorOnly() {
		t.Fatalf("codex apply_patch misdetected as OMP floor-only")
	}
	names := probeWireNames(t, payload)
	found := false
	for _, n := range names {
		if n == "apply_patch" {
			found = true
		}
		if strings.HasPrefix(n, "mcp__") {
			t.Errorf("codex wire carries virtualized rider %q", n)
		}
	}
	if !found {
		t.Fatalf("apply_patch missing from wire: %v", names)
	}
	raw := payload["tools"].([]any)
	for _, item := range raw {
		fn := item.(map[string]any)["function"].(map[string]any)
		if fn["name"] != "apply_patch" {
			continue
		}
		if desc, _ := fn["description"].(string); desc != "probe apply_patch" {
			t.Errorf("apply_patch schema substituted on the wire (desc %q); codex defs must ride names-only", desc)
		}
	}
	if got := mapper.RestoreName("apply_patch"); got != "apply_patch" {
		t.Errorf("RestoreName(apply_patch) = %q, want identity", got)
	}
}

// TestGatewayProbeOpenClawClassRenamesToOfficial pins the OpenAI-chat-shaped
// route-tool class (the OpenClaw custom-provider transport speaks
// openai-completions with plain function tools): non-family `read` renames
// onto `read_files` AND is substituted with the canonical def, so the gate
// sees CLI bytes. OpenClaw's exact vocabulary is audit-pending (no corpus
// row yet); this pins the class behavior its route tools fall into.
func TestGatewayProbeOpenClawClassRenamesToOfficial(t *testing.T) {
	tools := "[" +
		probeFn("read", `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`) + "," +
		probeFn("write_file", `{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}}}`) +
		"]"
	payload, mapper := probeWire(t, probeChatBody(tools))
	if mapper.FloorOnly() {
		t.Fatalf("plain route tools misdetected as OMP floor-only")
	}
	if mapper.ResponseRewrite() {
		t.Fatalf("plain route tools misdetected as a rewrite family")
	}
	names := probeWireNames(t, payload)
	has := func(n string) bool {
		for _, x := range names {
			if x == n {
				return true
			}
		}
		return false
	}
	if !has("read_files") || !has("write_file") {
		t.Fatalf("route tools not renamed onto official names: %v", names)
	}
	if has("read") {
		t.Fatalf("client name `read` rides the wire alongside the floor: %v", names)
	}
	raw := payload["tools"].([]any)
	for _, item := range raw {
		fn := item.(map[string]any)["function"].(map[string]any)
		if fn["name"] == "read_files" {
			if desc, _ := fn["description"].(string); desc == "probe read" {
				t.Errorf("read_files kept the foreign schema; route tools must be substituted with the canonical def")
			}
		}
	}
	if got := mapper.RestoreName("read_files"); got != "read" {
		t.Errorf("RestoreName(read_files) = %q, want read", got)
	}
}

// TestGatewayProbeDottedMCPVirtualizes pins the generic-MCP shape class
// (live case: Codewhale's dotted `web.run`, tool-name-translation.md rule
// 4): a grammar-illegal name never rides verbatim — it exits virtualized as
// a grammar-legal `mcp__` name and restores to the exact client name.
func TestGatewayProbeDottedMCPVirtualizes(t *testing.T) {
	tools := "[" + probeFn("web.run", `{"type":"object","properties":{"url":{"type":"string"}}}`) + "]"
	payload, mapper := probeWire(t, probeChatBody(tools))
	if mapper.FloorOnly() {
		t.Fatalf("dotted MCP tool misdetected as OMP floor-only")
	}
	names := probeWireNames(t, payload)
	var virt string
	for _, n := range names {
		if strings.HasPrefix(n, "mcp__") {
			virt = n
		}
		if strings.Contains(n, ".") {
			t.Errorf("grammar-illegal name %q rides the wire verbatim", n)
		}
	}
	if virt == "" {
		t.Fatalf("dotted tool not virtualized: %v", names)
	}
	if got := mapper.RestoreName(virt); got != "web.run" {
		t.Errorf("RestoreName(%q) = %q, want web.run", virt, got)
	}
}

// TestGatewayProbeToolChoiceFollowsTheFloor pins the pin contract on the
// floor leg: a tool_choice naming a dropped OMP-only tool is removed (the
// OpenAI default `auto` applies). Observed ordering consequence (pinned, not
// fixed — Phase 0 changes no prod behavior): floorOnlyOMP compares the pin
// against wire names BEFORE RenameRequestToolChoice runs, so a pin carrying
// the CLIENT name of a renamed floor tool (`bash`) also degrades to auto —
// only a pin that already names the wire tool survives.
func TestGatewayProbeToolChoiceFollowsTheFloor(t *testing.T) {
	ompTools := "[" +
		probeFn("bash", `{"type":"object","properties":{"command":{"type":"string"},"i":{"type":"string"}}}`) + "," +
		probeFn("task", `{"type":"object","properties":{"agent":{"type":"string"},"task":{"type":"string"},"i":{"type":"string"}}}`) +
		"]"
	withChoice := func(name string) []byte {
		return []byte(`{"model":"gpt-6-luna","messages":[{"role":"user","content":"hi"}],"tools":` + ompTools +
			`,"tool_choice":{"type":"function","function":{"name":"` + name + `"}}}`)
	}

	dropped, _ := probeWire(t, withChoice("task"))
	if _, ok := dropped["tool_choice"]; ok {
		t.Errorf("tool_choice on dropped OMP tool `task` survives the floor; upstream would see a pin for an unoffered tool")
	}

	clientNamed, _ := probeWire(t, withChoice("bash"))
	if _, ok := clientNamed["tool_choice"]; ok {
		t.Errorf("tool_choice on client name `bash` survives the floor; floorOnlyOMP compares the pin against wire names (run_terminal_command), so client-name pins degrade to auto")
	}

	// A pin that already names the wire tool survives the floor untouched.
	wirePinned, _ := probeWire(t, withChoice("run_terminal_command"))
	tc, ok := wirePinned["tool_choice"].(map[string]any)
	if !ok {
		t.Fatalf("tool_choice on wire name `run_terminal_command` removed; want it to survive the floor")
	}
	fn, _ := tc["function"].(map[string]any)
	if name, _ := fn["name"].(string); name != "run_terminal_command" {
		t.Errorf("tool_choice pin = %q, want run_terminal_command", name)
	}
}
