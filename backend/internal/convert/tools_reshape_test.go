package convert

import (
	"encoding/json"
	"testing"
)

// Reshape coverage: every substituted wire name the floor-only model can
// call must reshape to OMP-consumable args; unknown wire names and
// non-floor-only mappers pass through untouched.
func TestReshapeArgsTable(t *testing.T) {
	m := ToolMapper{family: familyOMP}
	cases := []struct {
		wire string
		args string
		want map[string]any
	}{
		{
			"run_terminal_command", `{"command":"ls","cwd":"/tmp","timeout_seconds":30,"process_type":"x"}`,
			map[string]any{"command": "ls", "cwd": "/tmp", "timeout": float64(30)},
		},
		{"read_files", `{"paths":["a.go"]}`, map[string]any{"path": "a.go"}},
		{"read_files", `{"paths":[{"path":"b.go"}]}`, map[string]any{"path": "b.go"}},
		{
			"str_replace", `{"path":"f","replacements":[{"oldString":"a","newString":"b"}]}`,
			map[string]any{"path": "f", "old_string": "a", "new_string": "b"},
		},
		{
			"write_file", `{"path":"f","instructions":"x","content":"hi"}`,
			// A flat string payload (CLI shape) rides through unchanged.
			map[string]any{"path": "f", "content": "hi"},
		},
		{
			"write_file", `{"path":"xd://tts","content":{"text":"hi","voice":"af_sky"}}`,
			// An xd:// device write carries its args as a nested value: a
			// blind string cast would blank content and the device would
			// receive empty args and never execute.
			map[string]any{"path": "xd://tts", "content": `{"text":"hi","voice":"af_sky"}`},
		},
		{
			"write_file", `{"path":"xd://reject","content":["a","b"]}`,
			// Array payloads marshal to their JSON text too.
			map[string]any{"path": "xd://reject", "content": `["a","b"]`},
		},
		{
			"write_file", `{"path":"f"}`,
			// Missing content stays an explicit empty string.
			map[string]any{"path": "f", "content": ""},
		},
		{"code_search", `{"pattern":"foo","cwd":"src"}`, map[string]any{"pattern": "foo", "path": "src"}},
		{"glob", `{"pattern":"*.go"}`, map[string]any{"path": "*.go"}},
		{
			"write_todos", `{"todos":[{"task":"a","completed":false},{"task":"b","completed":true}]}`,
			// OMP init items are STRING-only (InitListEntry.items: string[]);
			// the single entry carries the init half, completion rides as
			// trailing `done` ops (see TestReshapeTodoFanoutCompletion).
			map[string]any{"op": "init", "list": []any{map[string]any{
				"phase": "Tasks",
				"items": []any{"a", "b"},
			}}},
		},
		{
			"write_todos", `{"todos":[{"task":"b","completed":true}]}`,
			map[string]any{"op": "init", "list": []any{map[string]any{
				"phase": "Tasks",
				"items": []any{"b"},
			}}},
		},
		{
			"write_todos", `{"todos":[]}`,
			map[string]any{"op": "view"},
		},
		{"web_search", `{"query":"q","depth":"deep"}`, map[string]any{"query": "q"}},
	}
	for _, tc := range cases {
		got, ok := m.ReshapeArgsFor(tc.wire, tc.args)
		if !ok {
			t.Errorf("ReshapeArgsFor(%q) = not-ok, want reshape", tc.wire)
			continue
		}
		var gotM map[string]any
		if err := json.Unmarshal([]byte(got), &gotM); err != nil {
			t.Errorf("ReshapeArgsFor(%q) invalid JSON %q: %v", tc.wire, got, err)
			continue
		}
		wantB, _ := json.Marshal(tc.want)
		var wantM map[string]any
		if err := json.Unmarshal(wantB, &wantM); err != nil {
			t.Fatalf("want fixture invalid JSON: %v", err)
		}
		gotB, _ := json.Marshal(gotM)
		wantB2, _ := json.Marshal(wantM)
		if string(gotB) != string(wantB2) {
			t.Errorf("ReshapeArgsFor(%q) = %s, want %s", tc.wire, gotB, wantB2)
		}
	}
}

func TestReshapeClientNames(t *testing.T) {
	m := ToolMapper{family: familyOMP}
	// OMP-vocabulary names the model emits instead of the wire schema
	// (live 2026-09-30: turn called "read" with CLI-shaped {paths}):
	// restore is identity, but the args still need the wire rule.
	for _, tc := range [][3]string{
		{"read", `{"paths":["a.go"]}`, "path"},
		{"bash", `{"command":"ls","timeout_seconds":30}`, "command"},
		{"edit", `{"path":"f","replacements":[{"oldString":"a","newString":"b"}]}`, "old_string"},
		{"todo", `{"todos":[{"task":"a","completed":false}]}`, "op"},
	} {
		got, ok := m.ReshapeArgsFor(tc[0], tc[1])
		if !ok {
			t.Errorf("ReshapeArgsFor(%q) = not-ok, want client-name rule", tc[0])
			continue
		}
		var gotM map[string]any
		if err := json.Unmarshal([]byte(got), &gotM); err != nil {
			t.Errorf("ReshapeArgsFor(%q) invalid JSON: %v", tc[0], err)
			continue
		}
		if _, ok := gotM[tc[2]]; !ok {
			t.Errorf("ReshapeArgsFor(%q) = %s, want key %q", tc[0], got, tc[2])
		}
	}
	// OMP-vocabulary emission already in OMP shape passes through
	// untouched (reshaping would corrupt valid calls).
	for _, tc := range [][2]string{
		{"read", `{"path":"a.go"}`},
		{"bash", `{"command":"ls"}`},
		{"edit", `{"path":"f","old_string":"a","new_string":"b"}`},
		{"todo", `{"op":"view"}`},
		{"grep", `{"pattern":"x"}`},
	} {
		if got, ok := m.ReshapeArgsFor(tc[0], tc[1]); ok {
			t.Errorf("ReshapeArgsFor(%q) = %q, want passthrough (already OMP shape)", tc[0], got)
		}
	}
}

func TestReshapeArgsPassthrough(t *testing.T) {
	m := ToolMapper{family: familyOMP}
	if _, ok := m.ReshapeArgsFor("gravity_index", `{"a":1}`); ok {
		t.Error("gravity_index reshaped, want passthrough (no rule)")
	}
	if _, ok := m.ReshapeArgsFor("run_terminal_command", `not json`); ok {
		t.Error("invalid JSON reshaped, want passthrough")
	}
	plain := ToolMapper{}
	if _, ok := plain.ReshapeArgsFor("run_terminal_command", `{"command":"ls"}`); ok {
		t.Error("non-floor-only mapper reshaped, want passthrough")
	}
}

// OMP's init has no per-item status field (InitListEntry.items is string[]),
// so a CLI dump that marks tasks complete must fan out to the init op plus
// one trailing `done` op per completed task, order preserved.
func TestReshapeTodoFanoutCompletion(t *testing.T) {
	m := ToolMapper{family: familyOMP}
	got, ok := m.ReshapeArgsFanout("write_todos",
		`{"todos":[{"task":"a","completed":true},{"task":"b","completed":false},{"task":"c","completed":true}]}`)
	if !ok {
		t.Fatal("write_todos not reshaped")
	}
	if len(got) != 3 {
		t.Fatalf("calls = %d, want 3 (init + done a + done c): %v", len(got), got)
	}
	var init map[string]any
	if err := json.Unmarshal([]byte(got[0]), &init); err != nil {
		t.Fatalf("init args invalid JSON: %v", err)
	}
	if init["op"] != "init" {
		t.Errorf("call[0] op = %v, want init", init["op"])
	}
	list, _ := init["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("init list = %v, want one phase", init["list"])
	}
	phase, _ := list[0].(map[string]any)
	items, _ := phase["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("init items = %v, want 3 strings", phase["items"])
	}
	for i, want := range []string{"a", "b", "c"} {
		if items[i] != want {
			t.Errorf("items[%d] = %v (%T), want string %q", i, items[i], items[i], want)
		}
	}
	for i, want := range []string{"a", "c"} {
		var op map[string]any
		if err := json.Unmarshal([]byte(got[i+1]), &op); err != nil {
			t.Fatalf("done op %d invalid JSON: %v", i, err)
		}
		if op["op"] != "done" || op["task"] != want {
			t.Errorf("call[%d] = %v, want {op:done task:%s}", i+1, op, want)
		}
	}
}

// A model that emits an already-OMP-shaped todo op (no `todos` key) passes
// through untouched on both reshape paths: reshaping it would corrupt a
// valid call.
func TestReshapeTodoOMPShapePassthrough(t *testing.T) {
	m := ToolMapper{family: familyOMP}
	omp := `{"op":"done","task":"a"}`
	if got, ok := m.ReshapeArgsFor("write_todos", omp); ok {
		t.Errorf("ReshapeArgsFor rewrote OMP-shaped todo to %q, want passthrough", got)
	}
	if got, ok := m.ReshapeArgsFanout("write_todos", omp); ok {
		t.Errorf("ReshapeArgsFanout rewrote OMP-shaped todo to %v, want passthrough", got)
	}
}

// OMP delegation (task) normalization: the harness validates `tasks`
// (non-empty array of {name?, agent?, task}), but a model that never saw the
// tool definition (floor-only drops it from the wire) emits the singular
// vocabulary form {agent, task, context} — or a bare {task} — which the
// harness rejects with "Missing `tasks`". The response leg synthesizes
// tasks[] around the singular keys, preserving every original key; a valid
// non-empty tasks[] (or nothing usable at all) passes through untouched.
func TestReshapeTaskSingularToTasksBatch(t *testing.T) {
	m := ToolMapper{family: familyOMP}
	if !m.HasReshapeRule("task") {
		t.Error("HasReshapeRule(task) = false, want true (streaming buffer gate)")
	}
	cases := []struct {
		args string
		want map[string]any
	}{
		{
			`{"agent":"researcher","task":"audit the auth flow","context":"focus on session refresh"}`,
			map[string]any{
				"agent":   "researcher",
				"task":    "audit the auth flow",
				"context": "focus on session refresh",
				"tasks": []any{map[string]any{
					"agent": "researcher",
					"task":  "audit the auth flow",
				}},
			},
		},
		{
			`{"task":"ship it"}`,
			map[string]any{
				"task":  "ship it",
				"tasks": []any{map[string]any{"task": "ship it"}},
			},
		},
		{
			`{"name":"sub","agent":"coder","task":"write tests"}`,
			map[string]any{
				"name":  "sub",
				"agent": "coder",
				"task":  "write tests",
				"tasks": []any{map[string]any{
					"name":  "sub",
					"agent": "coder",
					"task":  "write tests",
				}},
			},
		},
	}
	for _, tc := range cases {
		got, ok := m.ReshapeArgsFor("task", tc.args)
		if !ok {
			t.Errorf("ReshapeArgsFor(task, %s) = not-ok, want tasks[] synthesis", tc.args)
			continue
		}
		var gotM map[string]any
		if err := json.Unmarshal([]byte(got), &gotM); err != nil {
			t.Errorf("ReshapeArgsFor(task) invalid JSON %q: %v", got, err)
			continue
		}
		wantB, _ := json.Marshal(tc.want)
		var wantM map[string]any
		if err := json.Unmarshal(wantB, &wantM); err != nil {
			t.Fatalf("want fixture invalid JSON: %v", err)
		}
		gotB, _ := json.Marshal(gotM)
		wantB2, _ := json.Marshal(wantM)
		if string(gotB) != string(wantB2) {
			t.Errorf("ReshapeArgsFor(task) = %s, want %s", gotB, wantB2)
		}
	}
	// A valid non-empty tasks[] batch passes through untouched.
	valid := `{"tasks":[{"name":"a","task":"one"}],"context":"keep"}`
	if got, ok := m.ReshapeArgsFor("task", valid); ok {
		t.Errorf("ReshapeArgsFor(task, batch) = %q, want passthrough", got)
	}
	// Nothing usable to synthesize from: passthrough, harness errors as today.
	if got, ok := m.ReshapeArgsFor("task", `{}`); ok {
		t.Errorf("ReshapeArgsFor(task, {}) = %q, want passthrough", got)
	}
	// pi has no task rule: extension/pass-through args stay byte-identical.
	pi := ToolMapper{family: familyPi}
	if pi.HasReshapeRule("task") {
		t.Error("pi HasReshapeRule(task) = true, want false")
	}
	if got, ok := pi.ReshapeArgsFor("task", `{"task":"x"}`); ok {
		t.Errorf("pi ReshapeArgsFor(task) = %q, want passthrough", got)
	}
}
