package convert

import (
	"encoding/json"
	"testing"
)

// Fan-out: wire shapes that carry N client operations expand to N OMP calls,
// order preserved; single-item shapes stay exactly 1:1.

func fanoutBodies(t *testing.T, m ToolMapper, wire, args string) []map[string]any {
	t.Helper()
	bodies, ok := m.ReshapeArgsFanout(wire, args)
	if !ok {
		t.Fatalf("ReshapeArgsFanout(%q) = passthrough, want reshape", wire)
	}
	var outs []map[string]any
	for _, b := range bodies {
		var o map[string]any
		if err := json.Unmarshal([]byte(b), &o); err != nil {
			t.Fatalf("ReshapeArgsFanout(%q) emitted invalid JSON %q: %v", wire, b, err)
		}
		outs = append(outs, o)
	}
	return outs
}

func TestReshapeFanoutRead(t *testing.T) {
	m := ToolMapper{floorOnly: true}
	// Multi-path string form fans out in order.
	outs := fanoutBodies(t, m, "read_files", `{"paths":["a.go","b.go","c.go"]}`)
	if len(outs) != 3 {
		t.Fatalf("read fan-out count = %d, want 3", len(outs))
	}
	for i, want := range []string{"a.go", "b.go", "c.go"} {
		if outs[i]["path"] != want || len(outs[i]) != 1 {
			t.Errorf("read fan-out[%d] = %v, want {path:%s}", i, outs[i], want)
		}
	}
	// Object form keeps the path; offset/limit have no OMP equivalent.
	outs = fanoutBodies(t, m, "read_files", `{"paths":[{"path":"a.go","offset":10,"limit":5},"b.go"]}`)
	if len(outs) != 2 || outs[0]["path"] != "a.go" || outs[1]["path"] != "b.go" {
		t.Fatalf("read object fan-out = %v, want [{path:a.go} {path:b.go}]", outs)
	}
	if _, ok := outs[0]["offset"]; ok {
		t.Errorf("read fan-out kept offset: %v", outs[0])
	}
	// Single path stays 1:1.
	outs = fanoutBodies(t, m, "read_files", `{"paths":["a.go"]}`)
	if len(outs) != 1 || outs[0]["path"] != "a.go" {
		t.Fatalf("single-path read = %v, want [{path:a.go}]", outs)
	}
	// Already OMP shape passes through (never blanked).
	if _, ok := m.ReshapeArgsFanout("read_files", `{"path":"a.go"}`); ok {
		t.Error("OMP-shaped read reshaped, want passthrough")
	}
	// Non-floor mappers never reshape.
	if _, ok := (ToolMapper{}).ReshapeArgsFanout("read_files", `{"paths":["a.go","b.go"]}`); ok {
		t.Error("non-floor mapper fanned out, want passthrough")
	}
}

func TestReshapeFanoutEdit(t *testing.T) {
	m := ToolMapper{floorOnly: true}
	outs := fanoutBodies(t, m, "str_replace",
		`{"path":"f","replacements":[{"oldString":"a","newString":"b"},{"oldString":"c","newString":"d","allowMultiple":true}]}`)
	if len(outs) != 2 {
		t.Fatalf("edit fan-out count = %d, want 2", len(outs))
	}
	want := []map[string]any{
		{"path": "f", "old_string": "a", "new_string": "b"},
		{"path": "f", "old_string": "c", "new_string": "d"},
	}
	for i := range want {
		gotB, _ := json.Marshal(outs[i])
		wantB, _ := json.Marshal(want[i])
		if string(gotB) != string(wantB) {
			t.Errorf("edit fan-out[%d] = %s, want %s", i, gotB, wantB)
		}
	}
	// Single replacement stays 1:1 and matches the legacy single shape.
	outs = fanoutBodies(t, m, "str_replace", `{"path":"f","replacements":[{"oldString":"a","newString":"b"}]}`)
	if len(outs) != 1 {
		t.Fatalf("single-replacement edit count = %d, want 1", len(outs))
	}
	single, ok := m.ReshapeArgsFor("str_replace", `{"path":"f","replacements":[{"oldString":"a","newString":"b"}]}`)
	if !ok {
		t.Fatal("ReshapeArgsFor(str_replace) = passthrough, want reshape")
	}
	if single != `{"new_string":"b","old_string":"a","path":"f"}` {
		// Compare semantically: key order is not contractual.
		var a, b map[string]any
		_ = json.Unmarshal([]byte(single), &a)
		bb, _ := json.Marshal(outs[0])
		_ = json.Unmarshal(bb, &b)
		ab, _ := json.Marshal(a)
		bbb, _ := json.Marshal(b)
		if string(ab) != string(bbb) {
			t.Errorf("ReshapeArgsFor != fan-out[0]: %s vs %s", ab, bbb)
		}
	}
	// No replacements key: already OMP shape, passthrough.
	if _, ok := m.ReshapeArgsFanout("str_replace", `{"path":"f","old_string":"a","new_string":"b"}`); ok {
		t.Error("OMP-shaped edit reshaped, want passthrough")
	}
}

func TestReshapeMessageCallsFanout(t *testing.T) {
	m := ToolMapper{floorOnly: true}
	mk := func(id, name, args string) any {
		return map[string]any{
			"id":   id,
			"type": "function",
			"function": map[string]any{
				"name":      name,
				"arguments": args,
			},
		}
	}
	tcs := []any{
		mk("call_r1", "read_files", `{"paths":["a.go","b.go"]}`),
		mk("call_b1", "run_terminal_command", `{"command":"ls"}`),
		mk("call_g1", "gravity_index", `{"a":1}`),
	}
	expanded, changed := m.ReshapeMessageCalls(tcs)
	if !changed {
		t.Fatal("ReshapeMessageCalls = unchanged, want fan-out")
	}
	if len(expanded) != 4 {
		t.Fatalf("expanded count = %d, want 4 (read x2 + bash + gravity)", len(expanded))
	}
	nameOf := func(i int) string {
		return expanded[i].(map[string]any)["function"].(map[string]any)["name"].(string)
	}
	idOf := func(i int) string {
		id, _ := expanded[i].(map[string]any)["id"].(string)
		return id
	}
	argsOf := func(i int) map[string]any {
		var o map[string]any
		_ = json.Unmarshal([]byte(expanded[i].(map[string]any)["function"].(map[string]any)["arguments"].(string)), &o)
		return o
	}
	// Order: parent read, extra read, bash, untouched gravity.
	if nameOf(0) != "read_files" || nameOf(1) != "read_files" || nameOf(2) != "run_terminal_command" || nameOf(3) != "gravity_index" {
		t.Errorf("order/names = %q %q %q %q, want read_files read_files run_terminal_command gravity_index",
			nameOf(0), nameOf(1), nameOf(2), nameOf(3))
	}
	if idOf(0) != "call_r1" || idOf(1) != "call_r1-fanout-1" || idOf(2) != "call_b1" {
		t.Errorf("ids = %q %q %q, want call_r1 call_r1-fanout-1 call_b1", idOf(0), idOf(1), idOf(2))
	}
	if argsOf(0)["path"] != "a.go" || argsOf(1)["path"] != "b.go" {
		t.Errorf("fanned args = %v %v, want {path:a.go} {path:b.go}", argsOf(0), argsOf(1))
	}
	if argsOf(2)["command"] != "ls" {
		t.Errorf("bash args = %v, want OMP bash shape", argsOf(2))
	}
	if argsOf(3)["a"] != float64(1) {
		t.Errorf("gravity args = %v, want byte-identical passthrough", argsOf(3))
	}
}

func TestFindGlobRestore(t *testing.T) {
	// A request that claimed wire glob for the client's find tool.
	findMapper := ToolMapper{
		floorOnly:        true,
		upstreamToClient: map[string]string{"glob": "find"},
		clientToUpstream: map[string]string{"find": "glob"},
	}
	// CLI glob shape restores to OMP find {pattern}; extras dropped.
	outs := fanoutBodies(t, findMapper, "glob", `{"pattern":"*.go","cwd":"src","max_results":5}`)
	if len(outs) != 1 || outs[0]["pattern"] != "*.go" || len(outs[0]) != 1 {
		t.Fatalf("ex-find glob = %v, want [{pattern:*.go}]", outs)
	}
	// Live-prompt query vocabulary maps to pattern too.
	outs = fanoutBodies(t, findMapper, "glob", `{"query":"*.ts","grep_keywords":["x"]}`)
	if len(outs) != 1 || outs[0]["pattern"] != "*.ts" {
		t.Fatalf("ex-find query glob = %v, want [{pattern:*.ts}]", outs)
	}
	// Native glob keeps the OMP glob {path} shape (glob→glob identity).
	native := ToolMapper{floorOnly: true}
	outs = fanoutBodies(t, native, "glob", `{"pattern":"*.go"}`)
	if len(outs) != 1 || outs[0]["path"] != "*.go" {
		t.Fatalf("native glob = %v, want [{path:*.go}]", outs)
	}
	// OMP-native glob emission passes through (never blanked to {path:""}).
	if _, ok := native.ReshapeArgsFanout("glob", `{"path":"src/**/*.go"}`); ok {
		t.Error("OMP-native glob reshaped, want passthrough")
	}
	// A model-emitted OMP "find" is already OMP-shaped: no rule, passthrough.
	if _, ok := findMapper.ReshapeArgsFor("find", `{"pattern":"*.go"}`); ok {
		t.Error("OMP-vocabulary find reshaped, want passthrough (already OMP shape)")
	}
	if got := findMapper.RestoreName("glob"); got != "find" {
		t.Errorf("RestoreName(glob) = %q, want find", got)
	}
	if got := native.RestoreName("glob"); got != "glob" {
		t.Errorf("native RestoreName(glob) = %q, want glob identity", got)
	}
}

func TestFindGlobRequestRoute(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[` +
		`{"type":"function","function":{"name":"read","description":"r","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}},` +
		`{"type":"function","function":{"name":"find","description":"f","parameters":{"type":"object","properties":{"pattern":{"type":"string"}},"required":["pattern"]}}},` +
		`{"type":"function","function":{"name":"eval","description":"e","parameters":{"type":"object","properties":{}}}},` +
		`{"type":"function","function":{"name":"learn","description":"l","parameters":{"type":"object","properties":{}}}}]}`
	wire, mapper, err := NormalizeRequestMappedOpts([]byte(body), "", DefaultOptions())
	if err != nil {
		t.Fatalf("NormalizeRequestMappedOpts: %v", err)
	}
	if !mapper.FloorOnly() {
		t.Fatal("eval+learn toolset did not trigger floor-only")
	}
	var decoded map[string]any
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatalf("wire not JSON: %v", err)
	}
	tools, _ := decoded["tools"].([]any)
	names := map[string]bool{}
	for _, rt := range tools {
		fn, _ := rt.(map[string]any)["function"].(map[string]any)
		n, _ := fn["name"].(string)
		names[n] = true
	}
	if !names["glob"] {
		t.Error("floor wire missing glob (find must ride as glob)")
	}
	for _, banned := range []string{"find", "find_files"} {
		if names[banned] {
			t.Errorf("floor wire carries %q (foreign-schema rider risk)", banned)
		}
	}
	if got := mapper.RestoreName("glob"); got != "find" {
		t.Errorf("RestoreName(glob) = %q, want find (find claimed the wire name)", got)
	}
	outs := fanoutBodies(t, mapper, "glob", `{"pattern":"*.go"}`)
	if len(outs) != 1 || outs[0]["pattern"] != "*.go" {
		t.Fatalf("mapped glob reshape = %v, want [{pattern:*.go}]", outs)
	}
}
