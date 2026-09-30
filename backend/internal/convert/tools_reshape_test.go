package convert

import (
	"encoding/json"
	"testing"
)

// Reshape coverage: every substituted wire name the floor-only model can
// call must reshape to OMP-consumable args; unknown wire names and
// non-floor-only mappers pass through untouched.
func TestReshapeArgsTable(t *testing.T) {
	m := ToolMapper{floorOnly: true}
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
			map[string]any{"path": "f", "content": "hi"},
		},
		{"code_search", `{"pattern":"foo","cwd":"src"}`, map[string]any{"pattern": "foo", "path": "src"}},
		{"glob", `{"pattern":"*.go"}`, map[string]any{"path": "*.go"}},
		{
			"write_todos", `{"todos":[{"task":"a","completed":false},{"task":"b","completed":true}]}`,
			map[string]any{"op": "init", "items": []any{"a"}},
		},
		{
			"write_todos", `{"todos":[{"task":"b","completed":true}]}`,
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

func TestReshapeArgsPassthrough(t *testing.T) {
	m := ToolMapper{floorOnly: true}
	if _, ok := m.ReshapeArgsFor("ask_user", `{"a":1}`); ok {
		t.Error("ask_user reshaped, want passthrough (no rule)")
	}
	if _, ok := m.ReshapeArgsFor("run_terminal_command", `not json`); ok {
		t.Error("invalid JSON reshaped, want passthrough")
	}
	plain := ToolMapper{}
	if _, ok := plain.ReshapeArgsFor("run_terminal_command", `{"command":"ls"}`); ok {
		t.Error("non-floor-only mapper reshaped, want passthrough")
	}
}
