package convert

import "testing"

// Detector unit tests for the foreign-harness signal mirror (vendor
// 3420c99, foreign-client-signals.ts): the genuine-vs-hollow schema rule,
// the enforced-signal set, and the evidence helpers. Pure functions over
// wire tool entries — no network, no keys.

func wireTool(name string, props map[string]any, desc string) map[string]any {
	params := map[string]any{"type": "object"}
	if props != nil {
		pp := map[string]any{}
		for k := range props {
			pp[k] = map[string]any{"type": "string"}
		}
		params["properties"] = pp
	}
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": desc,
			"parameters":  params,
		},
	}
}

func TestIsGenuineSignatureTool(t *testing.T) {
	t.Run("subset schema counts", func(t *testing.T) {
		// pi's powershell shape renamed to run_terminal_command: {command}
		// is a non-empty subset of the canonical keys, so a client one
		// release behind an added optional field still clears.
		if !IsGenuineSignatureTool("run_terminal_command", map[string]any{
			"type":       "object",
			"properties": map[string]any{"command": map[string]any{"type": "string"}},
		}) {
			t.Error("subset schema should be genuine")
		}
	})
	t.Run("foreign keys fail", func(t *testing.T) {
		// Claude Code's Read relabelled read_files still asks for
		// file_path/offset/limit, not paths.
		if IsGenuineSignatureTool("read_files", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file_path": map[string]any{"type": "string"},
				"offset":    map[string]any{"type": "number"},
				"limit":     map[string]any{"type": "number"},
			},
		}) {
			t.Error("renamed foreign schema must not be genuine")
		}
	})
	t.Run("empty schema never counts", func(t *testing.T) {
		if IsGenuineSignatureTool("read_files", map[string]any{
			"type": "object", "properties": map[string]any{},
		}) {
			t.Error("bare {} under a signature name must not be genuine")
		}
		if IsGenuineSignatureTool("read_files", nil) {
			t.Error("absent parameters must not be genuine")
		}
	})
	t.Run("zero-param tools never count", func(t *testing.T) {
		// end_turn and task_completed carry no structural signal to
		// verify: a copied name plus {} is byte-identical to the real
		// thing, so upstream takes them at neither face value.
		if IsGenuineSignatureTool("end_turn", map[string]any{
			"type": "object", "properties": map[string]any{},
		}) {
			t.Error("end_turn must never be genuine")
		}
		if IsGenuineSignatureTool("task_completed", map[string]any{
			"type": "object", "properties": map[string]any{},
		}) {
			t.Error("task_completed must never be genuine")
		}
	})
	t.Run("custom decide counts by name", func(t *testing.T) {
		if !IsGenuineSignatureTool("decide", nil) {
			t.Error("custom decide has no schema to check and counts by name")
		}
	})
	t.Run("custom complete_compaction counts by name", func(t *testing.T) {
		// Vendor 40c75256 adds complete_compaction to
		// FREEBUFF_CUSTOM_TOOL_NAMES: no schema to check, counts by name.
		if !IsGenuineSignatureTool("complete_compaction", nil) {
			t.Error("custom complete_compaction has no schema to check and counts by name")
		}
	})
	t.Run("unknown names never count", func(t *testing.T) {
		if IsGenuineSignatureTool("test_tool", map[string]any{
			"type": "object", "properties": map[string]any{"a": map[string]any{}},
		}) {
			t.Error("unknown tool must not be genuine")
		}
	})
	t.Run("combinator branches read", func(t *testing.T) {
		// The shape z.toJSONSchema produces for union schemas: branch
		// properties count toward the subset.
		if !IsGenuineSignatureTool("read_files", map[string]any{
			"anyOf": []any{
				map[string]any{"properties": map[string]any{"paths": map[string]any{}}},
			},
		}) {
			t.Error("anyOf branch keys should count")
		}
		if IsGenuineSignatureTool("read_files", map[string]any{
			"anyOf": []any{
				map[string]any{"properties": map[string]any{"file_path": map[string]any{}}},
			},
		}) {
			t.Error("foreign anyOf branch keys must fail")
		}
	})
}

// Pinned canonical keys for every parameterised official tool (vendor
// 3420c99 toolParams inputSchema literals, mirrored in
// canonicalToolParameterKeys). Each entry duplicates the map literals on
// purpose: a drifted pin or a dropped tool fails here before it can weaken
// the foreign_toolset read.
var pinnedCanonicalKeys = map[string][]string{
	"read_files":           {"paths"},
	"write_file":           {"path", "instructions", "content"},
	"str_replace":          {"path", "replacements"},
	"run_terminal_command": {"command", "process_type", "cwd", "timeout_seconds"},
	"list_directory":       {"path"},
	"code_search":          {"pattern", "flags", "cwd", "maxResults"},
	"write_todos":          {"todos"},
	"apply_patch":          {"operation"},
	"read_url":             {"url", "max_chars"},
	"web_search":           {"query", "depth"},
	"glob":                 {"pattern", "cwd", "max_results"},
	"find_files":           {"prompt"},
	"skill":                {"name"},
	"read_subtree":         {"paths", "maxTokens"},
}

func schemaWithKeys(keys ...string) map[string]any {
	props := map[string]any{}
	for _, k := range keys {
		props[k] = map[string]any{"type": "string"}
	}
	return map[string]any{"type": "object", "properties": props}
}

func TestCanonicalKeysCoverAllOfficialTools(t *testing.T) {
	// Every official target is either parameterised (pinned keys) or
	// zero-param (never genuine by design). Nothing falls through to
	// unknown fail-closed by accident.
	for name := range officialTools {
		_, param := canonicalToolParameterKeys[name]
		_, zero := ZeroParamSignatureTools[name]
		if !param && !zero {
			t.Errorf("official tool %q has no canonical keys and is not zero-param", name)
		}
	}
	if len(pinnedCanonicalKeys) != len(canonicalToolParameterKeys) {
		t.Errorf("pinned table = %d tools, map = %d; keep them in sync",
			len(pinnedCanonicalKeys), len(canonicalToolParameterKeys))
	}
	for name, keys := range pinnedCanonicalKeys {
		// Full canonical set reads genuine ...
		if !IsGenuineSignatureTool(name, schemaWithKeys(keys...)) {
			t.Errorf("%s: full canonical schema should be genuine", name)
		}
		// ... a one-key subset still reads genuine (a client one
		// release behind an added optional field still clears) ...
		if !IsGenuineSignatureTool(name, schemaWithKeys(keys[0])) {
			t.Errorf("%s: single-key subset should be genuine", name)
		}
		// ... but one extra unknown key fails closed (and reads hollow).
		extra := append(append([]string{}, keys...), "client_only_key")
		if IsGenuineSignatureTool(name, schemaWithKeys(extra...)) {
			t.Errorf("%s: schema with an extra unknown key must not be genuine", name)
		}
		if !IsHollowSignatureTool(name, schemaWithKeys(extra...), "Some client description.") {
			t.Errorf("%s: schema with an extra unknown key should read hollow", name)
		}
	}
}

func TestIsHollowSignatureTool(t *testing.T) {
	t.Run("injected end_turn reads hollow", func(t *testing.T) {
		// The exact shape injectEndTurnTool appends (schemacache_endturn.go):
		// the hollow definition the vendor comment names — one hollow
		// definition with an empty schema and a one-line description never
		// shipped. Pinned so any future injection edit re-proves its
		// standing instead of silently re-laundering.
		if !IsHollowSignatureTool("end_turn",
			map[string]any{"type": "object", "properties": map[string]any{}},
			"Signal the end of the current task.") {
			t.Error("injected end_turn must read hollow under the vendor rule")
		}
	})
	t.Run("shipped lead reads clean", func(t *testing.T) {
		if IsHollowSignatureTool("end_turn",
			map[string]any{"type": "object", "properties": map[string]any{}},
			"Only use this tool to hand control back to the user.\n\n- When to use: ...") {
			t.Error("shipped description lead must not read hollow")
		}
	})
	t.Run("parameterised mismatch is hollow", func(t *testing.T) {
		if !IsHollowSignatureTool("read_files",
			map[string]any{
				"type":       "object",
				"properties": map[string]any{"file_path": map[string]any{}},
			},
			"whatever") {
			t.Error("wrong-schema signature name must read hollow")
		}
		if IsHollowSignatureTool("read_files",
			map[string]any{
				"type":       "object",
				"properties": map[string]any{"paths": map[string]any{}},
			},
			"whatever") {
			t.Error("genuine schema must not read hollow")
		}
	})
	t.Run("custom and unknown never hollow", func(t *testing.T) {
		if IsHollowSignatureTool("decide", nil, "") {
			t.Error("custom decide is taken at face value, never hollow")
		}
		if IsHollowSignatureTool("test_tool", map[string]any{}, "A test tool") {
			t.Error("unknown names are unrecognised, not hollow")
		}
		if IsHollowSignatureTool("Bash", map[string]any{}, "x") {
			t.Error("harness names are foreign, not hollow")
		}
	})
}

func TestEnforcedSignals(t *testing.T) {
	for _, s := range []ForeignSignal{ForeignToolset, ForeignToolNames, ForeignSystemPrompt} {
		if !EnforcedSignals[s] {
			t.Errorf("%q must be enforced", s)
		}
	}
	for _, s := range []ForeignSignal{RootAgentNoTools, SamplingParams} {
		if EnforcedSignals[s] {
			t.Errorf("%q must stay report-only", s)
		}
	}
}

func TestForeignHarnessNames(t *testing.T) {
	// Spot pins across the four harnesses, including the case split the
	// detector relies on: PascalCase Claude tools vs lowercase opencode
	// ones. The full list is verified by count against the vendor set.
	for _, n := range []string{"Bash", "AskUserQuestion", "exec_command", "todowrite", "computer_use", "Shell"} {
		if !ForeignHarnessToolNames[n] {
			t.Errorf("%q must be a foreign harness name", n)
		}
	}
	if len(ForeignHarnessToolNames) != 49 {
		t.Errorf("harness names = %d, want 49 (vendor set at 3420c99)", len(ForeignHarnessToolNames))
	}
	// Deliberately absent upstream: generic lowercase names a local agent
	// could take are caught by the schema rule instead.
	for _, n := range []string{"clarify", "question", "update_plan", "read_file", "search_files"} {
		if ForeignHarnessToolNames[n] {
			t.Errorf("%q must stay off the harness list", n)
		}
	}
}

func TestListUnrecognisedToolNames(t *testing.T) {
	tools := []any{
		wireTool("test_tool", nil, "A test tool"),
		wireTool("read_files", map[string]any{"paths": true}, "ours"),
		wireTool("decide", nil, "custom"),
		wireTool("Bash", nil, "harness"),
		map[string]any{"type": "function", "function": map[string]any{"name": "server__tool"}},
		"not-a-map",
	}
	got := ListUnrecognisedToolNames(tools)
	if len(got) != 1 || got[0] != "test_tool" {
		t.Errorf("unrecognised = %v, want [test_tool]", got)
	}
}

func TestWireForeignSignal(t *testing.T) {
	t.Run("harness name settles first", func(t *testing.T) {
		v := ClassifyWireTools([]any{
			wireTool("Bash", nil, "harness"),
			wireTool("read_files", map[string]any{"paths": true}, "ours"),
		})
		if WireForeignSignal(v) != ForeignToolNames {
			t.Errorf("signal = %q, want foreign_tool_names despite the genuine tool", WireForeignSignal(v))
		}
	})
	t.Run("genuine clears toolset", func(t *testing.T) {
		v := ClassifyWireTools([]any{
			wireTool("test_tool", nil, "A test tool"),
			wireTool("run_terminal_command", map[string]any{"command": true}, "renamed"),
		})
		if WireForeignSignal(v) != "" {
			t.Errorf("signal = %q, want clear (genuine tool present)", WireForeignSignal(v))
		}
	})
	t.Run("genuine-less toolset is foreign", func(t *testing.T) {
		v := ClassifyWireTools([]any{
			wireTool("test_tool", nil, "A test tool"),
		})
		if WireForeignSignal(v) != ForeignToolset {
			t.Errorf("signal = %q, want foreign_toolset", WireForeignSignal(v))
		}
	})
	t.Run("empty clears", func(t *testing.T) {
		if s := WireForeignSignal(ClassifyWireTools(nil)); s != "" {
			t.Errorf("signal = %q, want clear for no tools", s)
		}
	})
}

// TestIsNowRecognisedToolset mirrors upstream isNowRecognisedToolset (vendor
// 40c75256, the 2026-09-23 complete_compaction beat detector): a stored
// foreign_toolset observe row carrying a custom tool clears unless a harness
// name convicts it. Guards the sweep class the detector exists for: the
// stale build banned observe rows the new one would have cleared.
func TestIsNowRecognisedToolset(t *testing.T) {
	cases := []struct {
		name  string
		names []string
		want  bool
	}{
		{name: "complete_compaction alone clears", names: []string{"complete_compaction"}, want: true},
		{name: "decide alone clears", names: []string{"decide"}, want: true},
		{name: "custom plus official clears", names: []string{"complete_compaction", "read_files"}, want: true},
		{name: "harness name convicts", names: []string{"complete_compaction", "Task"}, want: false},
		{name: "no custom tool never clears", names: []string{"read_files"}, want: false},
		{name: "empty never clears", names: nil, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsNowRecognisedToolset(tc.names); got != tc.want {
				t.Errorf("IsNowRecognisedToolset(%q) = %v, want %v", tc.names, got, tc.want)
			}
		})
	}
}
