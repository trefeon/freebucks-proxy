package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

// resumeOMPTools renders an OMP-family toolset (eval+learn are two
// signature names, forcing the floor) around a messages array.
func resumeOMPTools() string {
	return `[{"type":"function","function":{"name":"read","description":"r","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}},` +
		`{"type":"function","function":{"name":"bash","description":"b","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}},` +
		`{"type":"function","function":{"name":"eval","description":"e","parameters":{"type":"object","properties":{}}}},` +
		`{"type":"function","function":{"name":"learn","description":"l","parameters":{"type":"object","properties":{}}}}]`
}

func resumeWire(t *testing.T, body string) (map[string]any, ToolMapper) {
	t.Helper()
	norm, mapper, err := NormalizeRequestMappedOpts([]byte(body), "", DefaultOptions())
	if err != nil {
		t.Fatalf("NormalizeRequestMappedOpts: %v", err)
	}
	if !mapper.FloorOnly() {
		t.Fatal("eval+learn toolset did not trigger floor-only")
	}
	var p map[string]any
	if err := json.Unmarshal(norm, &p); err != nil {
		t.Fatalf("wire not JSON: %v", err)
	}
	return p, mapper
}

func wireToolDefs(p map[string]any) map[string]bool {
	defs := map[string]bool{}
	for _, x := range p["tools"].([]any) {
		fn, _ := x.(map[string]any)["function"].(map[string]any)
		if n, _ := fn["name"].(string); n != "" {
			defs[n] = true
		}
	}
	return defs
}

func wireHistoryCalls(p map[string]any) []string {
	var calls []string
	for _, m := range p["messages"].([]any) {
		mm, _ := m.(map[string]any)
		if mm == nil {
			continue
		}
		tcs, _ := mm["tool_calls"].([]any)
		for _, tc := range tcs {
			fn, _ := tc.(map[string]any)["function"].(map[string]any)
			if name, _ := fn["name"].(string); name != "" {
				calls = append(calls, name)
			}
		}
	}
	return calls
}

func wireToolEchoIDs(p map[string]any) []string {
	var ids []string
	for _, m := range p["messages"].([]any) {
		mm, _ := m.(map[string]any)
		if mm == nil {
			continue
		}
		if role, _ := mm["role"].(string); role != "tool" {
			continue
		}
		if id, _ := mm["tool_call_id"].(string); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func wireAssistantTexts(p map[string]any) string {
	var sb strings.Builder
	for _, m := range p["messages"].([]any) {
		mm, _ := m.(map[string]any)
		if mm == nil {
			continue
		}
		if role, _ := mm["role"].(string); role != "assistant" {
			continue
		}
		if s, ok := mm["content"].(string); ok {
			sb.WriteString(s + "\n")
		}
	}
	return sb.String()
}

// T2 (resume B1): resumed OMP history carrying a dropped-tool call (task)
// plus its role:tool echo must not virtualize into a wire call to an
// undeclared tool. The dropped call folds into assistant text (name, args,
// result preserved for the model); a declared history call (read) still
// rides renamed with its echo intact.
func TestFloorResumeHistoryDroppedToolFoldsToText(t *testing.T) {
	body := `{"model":"m","messages":[
		{"role":"user","content":"delegate this"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_task1","type":"function","function":{"name":"task","arguments":"{\"agent\":\"researcher\",\"task\":\"audit\"}"}}]},
		{"role":"tool","tool_call_id":"call_task1","name":"task","content":"delegated ok"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_read1","type":"function","function":{"name":"read","arguments":"{\"path\":\"a.txt\"}"}}]},
		{"role":"tool","tool_call_id":"call_read1","name":"read","content":"file bytes"},
		{"role":"user","content":"continue"}],"tools":` + resumeOMPTools() + `}`
	p, _ := resumeWire(t, body)
	defs := wireToolDefs(p)
	for _, c := range wireHistoryCalls(p) {
		if !defs[c] {
			t.Errorf("wire history calls undeclared tool %q (no def rides the floor)", c)
		}
		if strings.HasPrefix(c, "mcp__") {
			t.Errorf("wire history carries virtualized call %q with no def", c)
		}
	}
	calls := wireHistoryCalls(p)
	if len(calls) != 1 || calls[0] != "read_files" {
		t.Errorf("wire history calls = %v, want [read_files] (declared call renamed, dropped call folded)", calls)
	}
	ids := wireToolEchoIDs(p)
	if len(ids) != 1 || ids[0] != "call_read1" {
		t.Errorf("wire tool echoes = %v, want [call_read1] (consumed echo dropped, live echo kept)", ids)
	}
	text := wireAssistantTexts(p)
	for _, want := range []string{"task", "researcher", "delegated ok"} {
		if !strings.Contains(text, want) {
			t.Errorf("folded assistant text missing %q:\n%s", want, text)
		}
	}
}

// T6 (compacted-resume orphan): a role:tool echo whose id matches no
// assistant call anywhere must not ride the floor wire — a result with no
// call is meaningless context and risks the same undeclared-tool refusal.
// A matched echo stays. The transform is a fixpoint: a second pass changes
// nothing, so resumed turns cannot re-issue dropped echoes.
func TestFloorResumeOrphanToolEchoDropped(t *testing.T) {
	body := `{"model":"m","messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_live1","type":"function","function":{"name":"read","arguments":"{\"path\":\"a.txt\"}"}}]},
		{"role":"tool","tool_call_id":"call_live1","name":"read","content":"file bytes"},
		{"role":"tool","tool_call_id":"call_orphan9","name":"task","content":"stale compacted result"},
		{"role":"user","content":"continue"}],"tools":` + resumeOMPTools() + `}`
	p, _ := resumeWire(t, body)
	ids := wireToolEchoIDs(p)
	if len(ids) != 1 || ids[0] != "call_live1" {
		t.Errorf("wire tool echoes = %v, want [call_live1] (orphan call_orphan9 dropped)", ids)
	}
	if calls := wireHistoryCalls(p); len(calls) != 1 || calls[0] != "read_files" {
		t.Errorf("wire history calls = %v, want [read_files]", calls)
	}
	// Fixpoint: renormalizing the wire output changes nothing.
	again, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("re-marshal wire: %v", err)
	}
	norm2, _, err := NormalizeRequestMappedOpts(again, "", DefaultOptions())
	if err != nil {
		t.Fatalf("second NormalizeRequestMappedOpts: %v", err)
	}
	var p2 map[string]any
	if err := json.Unmarshal(norm2, &p2); err != nil {
		t.Fatalf("second wire not JSON: %v", err)
	}
	ids2 := wireToolEchoIDs(p2)
	if len(ids2) != 1 || ids2[0] != "call_live1" {
		t.Errorf("second-pass echoes = %v, want [call_live1] (transform must be a fixpoint)", ids2)
	}
}
