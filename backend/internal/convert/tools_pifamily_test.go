package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

// pi-family response translation (tools_floor.go familyPi): pi's core tools
// map onto official wire names like any client, but the model fills the
// canonical CLI shapes it was shown, so the response leg must reshape them
// back to pi's own shapes — read {path[, offset, limit]}, edit {path,
// edits:[{oldText,newText}]}, bash {command, timeout} — and canonical calls
// pi cannot dispatch (write_todos, web_search, ask_user, …) must degrade to
// assistant text instead of relaying a name pi answers with "Tool not found".

// piFamilyBody renders a chat request whose tool names are exactly names.
func piFamilyBody(names ...string) string {
	var b strings.Builder
	b.WriteString(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[`)
	for i, n := range names {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"type":"function","function":{"name":"` + n + `","parameters":{"type":"object","properties":{"input":{"type":"string"}}}}}`)
	}
	b.WriteString("]}")
	return b.String()
}

func TestPiFamilyDetection(t *testing.T) {
	cases := []struct {
		name  string
		tools []string
		want  clientFamily
	}{
		{"default pi session", []string{"read", "bash", "edit", "write"}, familyPi},
		{"read-only pi session", []string{"read", "grep", "find", "ls"}, familyPi},
		{"full pi session", []string{"read", "bash", "edit", "write", "grep", "find", "ls"}, familyPi},
		{"single generic tool is no family", []string{"read"}, familyNone},
		{"opencode is not pi", []string{"read", "bash", "edit", "write", "grep", "glob", "todowrite", "webfetch", "websearch", "question", "skill", "apply_patch"}, familyNone},
		{"omp wins over the shared vocabulary", []string{"read", "bash", "edit", "write", "eval", "learn"}, familyOMP},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			norm, mapper, err := NormalizeRequestMapped([]byte(piFamilyBody(tc.tools...)), "")
			if err != nil {
				t.Fatalf("NormalizeRequestMapped: %v", err)
			}
			if mapper.family != tc.want {
				t.Fatalf("family = %v, want %v", mapper.family, tc.want)
			}
			if tc.want != familyPi {
				return
			}
			// pi is NOT floored: the mapped core names keep their
			// substituted definitions on the wire, and the response leg
			// is still marked for rewrite.
			if mapper.FloorOnly() {
				t.Error("pi family went floor-only, want substituted wire")
			}
			if !mapper.ResponseRewrite() {
				t.Error("pi family lost its response rewrite flag")
			}
			wire := string(norm)
			// Only the declared tools are mapped; every declared name must
			// reach the wire under its official name, and no bare client
			// name may ride.
			mapped := map[string]string{
				"read": "read_files", "bash": "run_terminal_command", "powershell": "run_terminal_command",
				"edit": "str_replace", "write": "write_file", "grep": "code_search",
				"find": "glob", "ls": "list_directory",
			}
			seen := map[string]bool{}
			for _, name := range tc.tools {
				want := mapped[name]
				if !seen[want] {
					seen[want] = true
					if !strings.Contains(wire, `"name":"`+want+`"`) {
						t.Errorf("wire missing mapped name %s for %s: %s", want, name, clip(wire, 300))
					}
				}
				if strings.Contains(wire, `"name":"`+name+`"`) {
					t.Errorf("wire still carries bare client name %q: %s", name, clip(wire, 300))
				}
			}
		})
	}
}

// clip shortens a wire dump for failure messages (the substituted canonical
// descriptions make the full body unreadable).
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func reshapeOut(t *testing.T, m ToolMapper, wire, args string) []map[string]any {
	t.Helper()
	outs, ok := m.ReshapeArgsFanout(wire, args)
	if !ok {
		t.Fatalf("ReshapeArgsFanout(%s, %s): no reshape", wire, args)
	}
	decoded := make([]map[string]any, 0, len(outs))
	for _, b := range outs {
		var o map[string]any
		if err := json.Unmarshal([]byte(b), &o); err != nil {
			t.Fatalf("ReshapeArgsFanout(%s) emitted invalid JSON %q: %v", wire, b, err)
		}
		decoded = append(decoded, o)
	}
	return decoded
}

func TestPiReadFanoutKeepsOffsetLimit(t *testing.T) {
	m := ToolMapper{family: familyPi}
	outs := reshapeOut(t, m, "read_files", `{"paths":["a.go",{"path":"b.go","offset":10,"limit":5}]}`)
	if len(outs) != 2 {
		t.Fatalf("read fan-out count = %d, want 2 (%v)", len(outs), outs)
	}
	if outs[0]["path"] != "a.go" || len(outs[0]) != 1 {
		t.Errorf("read[0] = %v, want {path:a.go}", outs[0])
	}
	if outs[1]["path"] != "b.go" || outs[1]["offset"] != float64(10) || outs[1]["limit"] != float64(5) {
		t.Errorf("read[1] = %v, want {path:b.go,offset:10,limit:5} (pi read keeps them)", outs[1])
	}
	// Already pi-shaped read passes through, never blanked.
	if _, ok := m.ReshapeArgsFanout("read_files", `{"path":"a.go"}`); ok {
		t.Error("pi-shaped read {path} was reshaped, want passthrough")
	}
}

func TestPiEditIsBatchNative(t *testing.T) {
	m := ToolMapper{family: familyPi}
	outs := reshapeOut(t, m, "str_replace", `{"path":"f","replacements":[{"oldString":"a","newString":"b"},{"oldString":"c","newString":"d"}]}`)
	if len(outs) != 1 {
		t.Fatalf("pi edit fan-out count = %d, want 1 batch call (%v)", len(outs), outs)
	}
	if outs[0]["path"] != "f" {
		t.Errorf("edit path = %v, want f", outs[0]["path"])
	}
	edits, _ := outs[0]["edits"].([]any)
	if len(edits) != 2 {
		t.Fatalf("edits = %v, want 2 entries in ONE call", outs[0]["edits"])
	}
	first, _ := edits[0].(map[string]any)
	second, _ := edits[1].(map[string]any)
	if first["oldText"] != "a" || first["newText"] != "b" || second["oldText"] != "c" || second["newText"] != "d" {
		t.Errorf("edits = %v, want pi {oldText,newText} pairs in order", edits)
	}
	// Native pi edit (edits[]) and legacy flat shape pass through untouched.
	for _, native := range []string{
		`{"path":"f","edits":[{"oldText":"a","newText":"b"}]}`,
		`{"path":"f","oldText":"a","newText":"b"}`,
	} {
		if _, ok := m.ReshapeArgsFanout("edit", native); ok {
			t.Errorf("pi-native edit %s was reshaped, want passthrough", native)
		}
	}
	// Wire names outside pi's core mapping keep their own args even when a
	// reshape rule exists for the OMP vocabulary.
	if _, ok := m.ReshapeArgsFanout("write_todos", `{"todos":[{"task":"a","completed":false}]}`); ok {
		t.Error("pi write_todos was reshaped, want passthrough (not a pi core wire)")
	}
	// Unmapped clients stay byte-identical.
	if _, ok := (ToolMapper{}).ReshapeArgsFanout("str_replace", `{"path":"f","replacements":[{"oldString":"a","newString":"b"}]}`); ok {
		t.Error("unmapped mapper reshaped, want passthrough")
	}
}

func TestPiUnroutableCallsRenderAsText(t *testing.T) {
	m := ToolMapper{
		family:           familyPi,
		upstreamToClient: map[string]string{"read_files": "read", "str_replace": "edit"},
		clientTools:      map[string]bool{"read": true, "edit": true},
	}
	text, kind := m.TextFallback("write_todos", `{"todos":[{"task":"ship","completed":true},{"task":"test","completed":false}]}`)
	if kind != TextFallbackRender || !strings.Contains(text, "- [x] ship") || !strings.Contains(text, "- [ ] test") {
		t.Errorf("write_todos fallback = %q (%v), want rendered checklist", text, kind)
	}
	text, kind = m.TextFallback("web_search", `{"query":"kv store"}`)
	if kind != TextFallbackRender || !strings.Contains(text, "kv store") {
		t.Errorf("web_search fallback = %q (%v), want query note", text, kind)
	}
	text, kind = m.TextFallback("ask_user", `{"questions":[{"question":"Which auth?","options":[{"label":"JWT"},{"label":"Cookies"}]}]}`)
	if kind != TextFallbackRender || !strings.Contains(text, "Which auth?") || !strings.Contains(text, "JWT") {
		t.Errorf("ask_user fallback = %q (%v), want question list", text, kind)
	}
	if _, kind := m.TextFallback("report_project_profile", `{"status":"unchanged"}`); kind != TextFallbackAbsorb {
		t.Errorf("report_project_profile kind = %v, want absorb", kind)
	}
	// A mapped wire name restores to a real pi tool: never rendered.
	if _, kind := m.TextFallback("read_files", `{"paths":["a.go"]}`); kind != TextFallbackNone {
		t.Errorf("mapped read_files kind = %v, want ordinary", kind)
	}
	// A pi extension tool whose own name IS the wire name keeps real calls.
	ext := ToolMapper{family: familyPi, clientTools: map[string]bool{"web_search": true, "read": true}}
	if _, kind := ext.TextFallback("web_search", `{"query":"x"}`); kind != TextFallbackNone {
		t.Errorf("declared web_search kind = %v, want ordinary (extension tool dispatches)", kind)
	}
	// Unmapped and OMP mappers are untouched by the pi table.
	if _, kind := (ToolMapper{}).TextFallback("write_todos", `{}`); kind != TextFallbackNone {
		t.Errorf("unmapped write_todos kind = %v, want ordinary", kind)
	}
	if _, kind := (ToolMapper{family: familyOMP}).TextFallback("write_todos", `{}`); kind != TextFallbackNone {
		t.Errorf("OMP write_todos kind = %v, want ordinary (OMP routes it to todo)", kind)
	}
}
