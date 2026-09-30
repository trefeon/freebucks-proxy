package convert

import (
	"encoding/json"
	"os"
	"testing"
)

// Fallback routing for floor tools the OMP family never declares: the model
// sees all 16 floor defs (gate requirement) but OMP dispatches only its own
// names. Routable floor calls are renamed + reshaped to the OMP equivalent
// (ask_user->ask, read_url->read, list_directory->read, skill->read
// skill:// URI); the rest (suggest_followups, gravity_index, render_ui,
// report_project_profile) have no equivalent and keep failing client-side
// with "not found" — documented, not silent.
func TestFloorFallbackRestore(t *testing.T) {
	raw, err := os.ReadFile("D:/tmp/ompcap/001-req.json")
	if err != nil {
		t.Skip("no capture")
	}
	var cap struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(raw, &cap); err != nil {
		t.Fatal(err)
	}
	_, mapper, err := NormalizeRequestMappedOpts([]byte(cap.Body), "", DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !mapper.FloorOnly() {
		t.Fatal("capture did not trigger floor-only")
	}
	for wire, client := range map[string]string{
		"ask_user": "ask", "read_url": "read",
		"list_directory": "read", "skill": "read",
	} {
		if got := mapper.RestoreName(wire); got != client {
			t.Errorf("RestoreName(%q) = %q, want %q (fallback route)", wire, got, client)
		}
	}
}

func TestFloorFallbackReshape(t *testing.T) {
	m := ToolMapper{floorOnly: true}
	m.RegisterFloorFallbacks()
	cases := []struct {
		wire string
		args string
		want map[string]any
	}{
		{
			"ask_user", `{"questions":[{"question":"Pick?","options":[{"label":"A"},{"label":"B","description":"bee"}]}]}`,
			map[string]any{"questions": []any{map[string]any{
				"id": "q0", "question": "Pick?",
				"options": []any{map[string]any{"label": "A"}, map[string]any{"label": "B", "description": "bee"}},
			}}},
		},
		{
			"read_url", `{"url":"https://omp.sh/docs/cli","max_chars":3000}`,
			map[string]any{"path": "https://omp.sh/docs/cli"},
		},
		{
			"skill", `{"name":"find-skills"}`,
			map[string]any{"path": "skill://find-skills"},
		},
	}
	for _, tc := range cases {
		got, ok := m.ReshapeArgsFor(tc.wire, tc.args)
		if !ok {
			t.Errorf("ReshapeArgsFor(%q) = not-ok, want fallback reshape", tc.wire)
			continue
		}
		var gotM map[string]any
		if err := json.Unmarshal([]byte(got), &gotM); err != nil {
			t.Errorf("ReshapeArgsFor(%q) invalid JSON: %v", tc.wire, err)
			continue
		}
		wantB, _ := json.Marshal(tc.want)
		gotB, _ := json.Marshal(gotM)
		var wantM map[string]any
		if err := json.Unmarshal(wantB, &wantM); err != nil {
			t.Fatalf("want fixture invalid: %v", err)
		}
		wantB2, _ := json.Marshal(wantM)
		if string(gotB) != string(wantB2) {
			t.Errorf("ReshapeArgsFor(%q) = %s, want %s", tc.wire, gotB, wantB2)
		}
	}
}

// The todo reshape must emit the verified phase form with STRING items:
// {op:init, list:[{phase, items:[string]}]} — OMP's InitListEntry types items
// as string[], so a per-item {task, completed} object is rejected by the
// dispatcher, and completion rides as trailing `done` ops instead
// (TestReshapeTodoFanoutCompletion).
func TestTodoReshapePhaseForm(t *testing.T) {
	m := ToolMapper{floorOnly: true}
	got, ok := m.ReshapeArgsFor("write_todos",
		`{"todos":[{"task":"a","completed":false},{"task":"b","completed":true}]}`)
	if !ok {
		t.Fatal("write_todos not reshaped")
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatal(err)
	}
	if out["op"] != "init" {
		t.Fatalf("op = %v, want init", out["op"])
	}
	list, _ := out["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("list = %v, want one phase", out["list"])
	}
	phase, _ := list[0].(map[string]any)
	if phase["phase"] == "" {
		t.Errorf("phase name missing: %v", phase)
	}
	items, _ := phase["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %v, want both todos carried", phase["items"])
	}
	if items[0] != "a" || items[1] != "b" {
		t.Errorf("items = %v, want the string task titles in order", items)
	}
}
