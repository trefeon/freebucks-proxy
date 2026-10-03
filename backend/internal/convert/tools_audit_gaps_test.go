package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

// OMP-surface audit gaps G1 (HIGH) + G3/G4/G5 (LOW), pinned hermetically
// against the live OMP 18.5.0 registry (BUILTIN_TOOL_NAMES + tool schemas):
// find is a registered strict {query, grep_keywords} tool; web_search
// requires query; ask requires per-question options; read requires path.

// G1: a model-emitted `find` call carrying CLI pattern vocabulary reshapes
// to the dispatchable live shape; an already-valid emission passes through
// untouched; a shape with neither vocabulary passes through for the harness
// to reject exactly as today (never dropped, never invented).
func TestAuditFindNamedEmissionReshape(t *testing.T) {
	m := ToolMapper{family: familyOMP}
	// CLI pattern vocabulary gains the strict-required fields.
	got, ok := m.ReshapeArgsFor("find", `{"pattern":"auth refresh","cwd":"src"}`)
	if !ok {
		t.Fatal("find {pattern} = passthrough, want reshape to the live shape")
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatalf("find reshape invalid JSON: %v", err)
	}
	if out["query"] != "auth refresh" {
		t.Errorf("query = %v, want the pattern text", out["query"])
	}
	kw, _ := out["grep_keywords"].([]any)
	if len(kw) != 1 || kw[0] != "auth refresh" {
		t.Errorf("grep_keywords = %v, want [query] (strict-required, defaulted)", out["grep_keywords"])
	}
	if out["path"] != "src" {
		t.Errorf("path = %v, want src (cwd scopes)", out["path"])
	}
	if _, banned := out["pattern"]; banned {
		t.Errorf("reshaped find keeps CLI key pattern: %v", out)
	}
	// Already-valid live emission: passthrough (never re-shaped).
	if _, ok := m.ReshapeArgsFor("find", `{"query":"auth refresh","grep_keywords":["refresh"]}`); ok {
		t.Error("valid find reshaped, want passthrough")
	}
	// Nothing usable: passthrough, the harness errors exactly as today.
	if _, ok := m.ReshapeArgsFor("find", `{"cwd":"src"}`); ok {
		t.Error("query-less find reshaped, want passthrough")
	}
	// pi keeps its own find vocabulary (no rule on that family).
	if _, ok := (ToolMapper{family: familyPi}).ReshapeArgsFor("find", `{"pattern":"*.go"}`); ok {
		t.Error("pi find reshaped, want passthrough (pi dispatches its own shape)")
	}
	// Non-family mappers never reshape.
	if _, ok := (ToolMapper{}).ReshapeArgsFor("find", `{"pattern":"*.go"}`); ok {
		t.Error("non-floor find reshaped, want passthrough")
	}
}

// G1: the find rule joins the streaming buffer gate on OMP only.
func TestAuditFindWithholdsOnOMPOnly(t *testing.T) {
	if !(ToolMapper{family: familyOMP}).WithholdStreamArgs("find") {
		t.Error("OMP find does not withhold (rule must buffer fragments)")
	}
	if (ToolMapper{family: familyPi}).WithholdStreamArgs("find") {
		t.Error("pi find withholds (pi has no find rule)")
	}
}

// G3: an empty gravity shape (no query/q/category/slug) must not ride as a
// query-less web_search — it renders the discovery note instead. Query-ful
// shapes stay ordinary (reshape proceeds to web_search).
func TestAuditEmptyGravityRendersNote(t *testing.T) {
	m := ToolMapper{family: familyOMP}
	for _, args := range []string{
		`{}`,
		`{"action":"list_categories"}`,
		`{"action":"search"}`,
		`{"a":1}`,
		`not json`,
		``,
	} {
		text, kind := m.TextFallback("gravity_index", args)
		if kind != TextFallbackRender {
			t.Errorf("gravity %q kind = %v, want render", args, kind)
			continue
		}
		if !strings.Contains(text, "Gravity Index") {
			t.Errorf("gravity %q text = %q, want the discovery note", args, text)
		}
	}
	// Query-ful shapes are ordinary calls (the reshape still routes them).
	for _, args := range []string{
		`{"action":"search","query":"postgres hosting"}`,
		`{"action":"browse","q":"sendgrid"}`,
		`{"action":"browse","category":"database"}`,
		`{"action":"get_service","slug":"supabase"}`,
	} {
		if _, kind := m.TextFallback("gravity_index", args); kind != TextFallbackNone {
			t.Errorf("gravity %q kind = %v, want none (reshape routes it)", args, kind)
		}
	}
	// Other families are untouched by the OMP guard.
	pi := ToolMapper{family: familyPi, clientTools: map[string]bool{}}
	if _, kind := pi.TextFallback("gravity_index", `{}`); kind == TextFallbackNone {
		t.Error("pi empty gravity = none, want the pi renderer to still classify it")
	}
}

// G3: end to end on the whole-args legs — an empty gravity call is
// suppressed into content (never relayed as web_search), a query-ful one
// still routes to web_search with its synthesized query.
func TestAuditEmptyGravityWholeArgsLegs(t *testing.T) {
	m := ToolMapper{family: familyOMP}
	m.RegisterFloorFallbacks()
	comp := map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{map[string]any{
					"id":   "c1",
					"type": "function",
					"function": map[string]any{
						"name":      "gravity_index",
						"arguments": `{"action":"list_categories"}`,
					},
				}},
			},
			"finish_reason": "tool_calls",
		}},
	}
	if !m.ApplyTextFallbacks(comp) {
		t.Fatal("empty gravity not classified, want suppression into text")
	}
	msg := comp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if _, has := msg["tool_calls"]; has {
		t.Errorf("empty gravity relayed as a call: %v", msg["tool_calls"])
	}
	content, _ := msg["content"].(string)
	if !strings.Contains(content, "Gravity Index") {
		t.Errorf("content = %q, want the discovery note", content)
	}
	if fr := comp["choices"].([]any)[0].(map[string]any)["finish_reason"]; fr != "stop" {
		t.Errorf("finish_reason = %v, want stop (no call delivered)", fr)
	}
	// The Anthropic/Responses streaming finalize shares the classifier.
	calls, text, absorb := m.FinalizeStreamedCall("gravity_index", "c9", `{"action":"search"}`)
	if absorb || len(calls) != 0 || !strings.Contains(text, "Gravity Index") {
		t.Errorf("finalize empty gravity = (%v,%q,%v), want (no calls, note, no absorb)", calls, text, absorb)
	}
}

// G4: the live ask schema requires options per question (strict) — a CLI
// question carrying none emits an empty array, never a missing key.
func TestAuditAskOptionlessEmitsEmptyOptions(t *testing.T) {
	m := ToolMapper{family: familyOMP}
	got, ok := m.ReshapeArgsFor("ask_user", `{"questions":[{"question":"Ship it?"},{"question":"Pick?","options":[{"label":"A"}]}]}`)
	if !ok {
		t.Fatal("ask_user = passthrough, want reshape")
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatalf("ask reshape invalid JSON: %v", err)
	}
	qs, _ := out["questions"].([]any)
	if len(qs) != 2 {
		t.Fatalf("questions = %v, want both questions kept", out["questions"])
	}
	q0 := qs[0].(map[string]any)
	opts, present := q0["options"].([]any)
	if !present {
		t.Fatalf("optionless question = %v, want an options key", q0)
	}
	if len(opts) != 0 {
		t.Errorf("optionless options = %v, want []", opts)
	}
	q1 := qs[1].(map[string]any)
	if qopts, _ := q1["options"].([]any); len(qopts) != 1 {
		t.Errorf("optioned question options = %v, want the label kept", q1["options"])
	}
}

// G5: blank read paths (empty or whitespace-only, string or object form)
// are skipped — the live read schema requires path. All-blank (or empty)
// fans out to nothing, which the caller turns into a passthrough.
func TestAuditReadBlankPathsSkipped(t *testing.T) {
	m := ToolMapper{family: familyOMP}
	bodies, ok := m.ReshapeArgsFanout("read_files", `{"paths":["a.go","","b.go",{"path":"  "},{"path":"c.go","offset":3}]}`)
	if !ok || len(bodies) != 3 {
		t.Fatalf("fanout = (%v,%v), want 3 calls (a, b, c)", bodies, ok)
	}
	for i, want := range []string{"a.go", "b.go", "c.go"} {
		var out map[string]any
		if err := json.Unmarshal([]byte(bodies[i]), &out); err != nil {
			t.Fatalf("body %d invalid JSON: %v", i, err)
		}
		if out["path"] != want {
			t.Errorf("body %d = %v, want path %q", i, out, want)
		}
	}
	// All blank: passthrough (never a blanked-out path, never dropped).
	for _, args := range []string{`{"paths":["","  "]}`, `{"paths":[{"path":""}]}`, `{"paths":[]}`} {
		if _, ok := m.ReshapeArgsFanout("read_files", args); ok {
			t.Errorf("ReshapeArgsFanout(%q) reshaped, want passthrough", args)
		}
	}
	if _, ok := m.ReshapeArgsFor("read_files", `{"paths":[""]}`); ok {
		t.Error("single blank read reshaped, want passthrough")
	}
}
