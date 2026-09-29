package upstream

import (
	"encoding/json"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/convert"
)

// expectedCliToolNames is the exact 16-tool official set captured from the
// CLI (backend/internal/upstream/testdata/cli-tools.json, in fixture order).
var expectedCliToolNames = []string{
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

func wireToolNames(t *testing.T, payload map[string]any) []string {
	t.Helper()
	raw, ok := payload["tools"].([]any)
	if !ok {
		t.Fatalf("tools = %T, want []any", payload["tools"])
	}
	names := make([]string, 0, len(raw))
	for _, def := range raw {
		name := wireFunctionName(def)
		if name == "" {
			t.Fatalf("malformed tool entry: %v", def)
		}
		names = append(names, name)
	}
	return names
}

func assertNoDupes(t *testing.T, names []string) {
	t.Helper()
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Fatalf("duplicate wire tool %q in %v", n, names)
		}
		seen[n] = true
	}
}

func assertExactNameSet(t *testing.T, names []string) {
	t.Helper()
	if len(names) != len(expectedCliToolNames) {
		t.Fatalf("wire tools = %v, want the %d official declarations", names, len(expectedCliToolNames))
	}
	want := map[string]bool{}
	for _, n := range expectedCliToolNames {
		want[n] = true
	}
	for _, n := range names {
		if !want[n] {
			t.Errorf("unexpected wire tool %q, want only the official 16", n)
		}
	}
	assertNoDupes(t, names)
}

// TestCliToolsFixtureParsesToSixteen pins the embedded fixture: 16
// definitions with the exact official name set, each a full function schema
// (object params with properties — never a minimal skeleton).
func TestCliToolsFixtureParsesToSixteen(t *testing.T) {
	tools := defaultCliTools()
	if len(tools) != len(expectedCliToolNames) {
		t.Fatalf("defaultCliTools() = %d defs, want %d", len(tools), len(expectedCliToolNames))
	}
	for i, def := range tools {
		m, ok := def.(map[string]any)
		if !ok || m["type"] != "function" {
			t.Fatalf("tool[%d] = %v, want a function tool", i, def)
		}
		fn, ok := m["function"].(map[string]any)
		if !ok {
			t.Fatalf("tool[%d] has no function dict", i)
		}
		if fn["name"] != expectedCliToolNames[i] {
			t.Errorf("tool[%d].name = %q, want %q (fixture order)", i, fn["name"], expectedCliToolNames[i])
		}
		params, ok := fn["parameters"].(map[string]any)
		if !ok || params["type"] != "object" {
			t.Errorf("%v parameters = %v, want a full object schema", fn["name"], fn["parameters"])
			continue
		}
		if props, ok := params["properties"].(map[string]any); !ok || len(props) == 0 {
			t.Errorf("%v parameters.properties = %v, want a non-empty full schema (not a skeleton)", fn["name"], params["properties"])
		}
	}
	// Spot-pin load-bearing schema keys so a skeleton regression fails loudly.
	byName := map[string]map[string]any{}
	for _, def := range tools {
		fn := def.(map[string]any)["function"].(map[string]any)
		byName[fn["name"].(string)] = fn["parameters"].(map[string]any)["properties"].(map[string]any)
	}
	for tool, key := range map[string]string{
		"run_terminal_command": "command",
		"read_files":           "paths",
		"code_search":          "pattern",
	} {
		if _, ok := byName[tool][key]; !ok {
			t.Errorf("%v schema missing property %q (have %v)", tool, key, byName[tool])
		}
	}
}

// firstSystemContent extracts the first system message's string content.
func firstSystemContent(t *testing.T, payload map[string]any) string {
	t.Helper()
	msgs, ok := payload["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("messages = %v, want a non-empty list", payload["messages"])
	}
	content, ok := msgs[0].(map[string]any)["content"].(string)
	if !ok {
		t.Fatalf("first message content = %v, want a string", msgs[0])
	}
	return content
}

const sixthBullet = "Don't run destructive or hard-to-undo commands (git push, resets, deploys) unless the user asks for them."

func assertFullBase3Head(t *testing.T, content string) {
	t.Helper()
	if !strings.HasPrefix(content, "You are Buffy, the coding agent behind Codebuff.") {
		t.Errorf("head does not open with the base3 canonical sentence: %.80q...", content)
	}
	if !strings.Contains(content, "Current date: ") {
		t.Error("head missing the dynamic `Current date: ` line")
	}
	if !strings.Contains(content, sixthBullet) {
		t.Error("head missing the 6th convention bullet (not the full canonical head)")
	}
}

// TestCliSystemMarkerFullBase3Head pins the gate's system shape: system-less
// and foreign-system bodies get the full base3 canonical head (opening +
// dynamic date + 6 convention bullets), while a body that already opens with
// a canonical identity is left untouched.
func TestCliSystemMarkerFullBase3Head(t *testing.T) {
	opts := ChatOptions{RunID: "r", AgentID: "base3-free-test", Model: "z-ai/glm-5.3-flash"}

	t.Run("system-less body gets the full head", func(t *testing.T) {
		out, err := injectEnvelope([]byte(`{"model":"m"}`), "free", opts)
		if err != nil {
			t.Fatal(err)
		}
		var sent map[string]any
		if err := json.Unmarshal(out, &sent); err != nil {
			t.Fatal(err)
		}
		assertFullBase3Head(t, firstSystemContent(t, sent))
	})

	t.Run("foreign-system body is sanitized and headed", func(t *testing.T) {
		body := `{"model":"m","messages":[{"role":"system","content":"You are Claude Code, Anthropic's official CLI. Be helpful."}]}`
		out, err := injectEnvelope([]byte(body), "free", opts)
		if err != nil {
			t.Fatal(err)
		}
		var sent map[string]any
		if err := json.Unmarshal(out, &sent); err != nil {
			t.Fatal(err)
		}
		got := firstSystemContent(t, sent)
		assertFullBase3Head(t, got)
		if strings.Contains(got, "You are Claude Code") {
			t.Error("foreign harness marker survived the envelope")
		}
	})

	t.Run("canonical body untouched", func(t *testing.T) {
		const canonical = "You are Buffy, the coding agent behind Codebuff. Custom persona."
		body := `{"model":"m","messages":[{"role":"system","content":"` + canonical + `"}]}`
		out, err := injectEnvelope([]byte(body), "free", opts)
		if err != nil {
			t.Fatal(err)
		}
		var sent map[string]any
		if err := json.Unmarshal(out, &sent); err != nil {
			t.Fatal(err)
		}
		if got := firstSystemContent(t, sent); got != canonical {
			t.Errorf("canonical content rewritten: %q", got)
		}
	})
}

// TestCliToolsFloorTopsUpMappedClientTools pins the production flow: the
// convert layer maps the client's bash onto run_terminal_command (and appends
// its end_turn/decide signature tools), then the envelope floor tops up the
// remaining 15 officials — every official present, no dupes — while the
// mapper restores official names to the client's own (mapped names restore
// to the client name, floor-only names pass through).
func TestCliToolsFloorTopsUpMappedClientTools(t *testing.T) {
	clientBody := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"bash","description":"Run a command","parameters":{"type":"object"}}}],` +
		`"tool_choice":"auto"}`)
	wireBody, mapper, err := convert.NormalizeRequestMapped(clientBody, "")
	if err != nil {
		t.Fatal(err)
	}
	var mapped map[string]any
	if err := json.Unmarshal(wireBody, &mapped); err != nil {
		t.Fatal(err)
	}
	// Convert maps bash first, then appends its end_turn/decide injections.
	if got := wireToolNames(t, mapped); len(got) != 3 || got[0] != "run_terminal_command" || got[1] != "end_turn" || got[2] != "decide" {
		t.Fatalf("mapped wire tools = %v, want [run_terminal_command end_turn decide]", got)
	}

	out, err := injectEnvelope(wireBody, "free", ChatOptions{RunID: "r", AgentID: "base3-free-test", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	if err := json.Unmarshal(out, &sent); err != nil {
		t.Fatal(err)
	}
	names := wireToolNames(t, sent)
	assertNoDupes(t, names)
	if len(names) != len(expectedCliToolNames)+2 {
		t.Fatalf("wire tools = %v, want the 16 officials plus the convert end_turn/decide injections", names)
	}
	seen := map[string]bool{}
	for _, n := range names {
		seen[n] = true
	}
	for _, want := range expectedCliToolNames {
		if !seen[want] {
			t.Errorf("floor tool %q missing from the wire set", want)
		}
	}
	if sent["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %v, want the client's own %q", sent["tool_choice"], "auto")
	}

	if got := mapper.RestoreName("run_terminal_command"); got != "bash" {
		t.Errorf("RestoreName(run_terminal_command) = %q, want bash", got)
	}
	if got := mapper.RestoreName("list_directory"); got != "list_directory" {
		t.Errorf("RestoreName(list_directory) = %q, want identity (floor-only names pass through)", got)
	}
}

// TestCliToolsFloorFillsEmptyToolset pins the tool-less path: absent or
// empty client tools get the full 16 floor.
func TestCliToolsFloorFillsEmptyToolset(t *testing.T) {
	opts := ChatOptions{RunID: "r", AgentID: "base3-free-test", Model: "m"}
	for _, body := range []string{`{"model":"m"}`, `{"model":"m","tools":[]}`} {
		out, err := injectEnvelope([]byte(body), "free", opts)
		if err != nil {
			t.Fatal(err)
		}
		var sent map[string]any
		if err := json.Unmarshal(out, &sent); err != nil {
			t.Fatal(err)
		}
		assertExactNameSet(t, wireToolNames(t, sent))
		if _, present := sent["tool_choice"]; present {
			t.Error("tool_choice fabricated for a request that sent none")
		}
	}
}
