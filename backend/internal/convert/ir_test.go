package convert

import (
	"strings"
	"testing"
)

// Unit tests for the IR core (ir.go): surface detection, Decode/Own,
// choice decoding, kind classification, loss policy, and registries.

func TestDetectSurface(t *testing.T) {
	chat := `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"bash"}}]}`
	if got := DetectSurface([]byte(chat)); got != SurfaceChat {
		t.Errorf("chat = %q, want chat", got)
	}
	resp := `{"input":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"bash"}}]}`
	if got := DetectSurface([]byte(resp)); got != SurfaceResponses {
		t.Errorf("responses = %q, want responses", got)
	}
	anth := `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"tools":[{"name":"bash","input_schema":{"type":"object"}}]}`
	if got := DetectSurface([]byte(anth)); got != SurfaceAnthropic {
		t.Errorf("anthropic = %q, want anthropic", got)
	}
	if got := DetectSurface([]byte(`not json`)); got != SurfaceUnknown {
		t.Errorf("garbage = %q, want unknown", got)
	}
	if got := DetectSurface(nil); got != SurfaceUnknown {
		t.Errorf("nil = %q, want unknown", got)
	}
}

func irFuncTool(name string) string {
	return `{"type":"function","function":{"name":"` + name + `","parameters":{"type":"object"}}}`
}

func TestDecodeOwnKinds(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"hi"}],"tools":[`
	body += irFuncTool("bash") + `,` + irFuncTool("test_tool") + `,` + irFuncTool("Task") + `]}`
	r := DecodeRequestIR([]byte(body))
	if r.Surface != SurfaceChat {
		t.Errorf("surface = %q, want chat", r.Surface)
	}
	if len(r.Tools) != 3 {
		t.Fatalf("decoded %d tools, want 3", len(r.Tools))
	}
	if r.MsgCount != 1 || r.ToolCount != 3 {
		t.Errorf("counts = %d/%d, want 1/3", r.MsgCount, r.ToolCount)
	}
	r.Own()
	byClient := make(map[string]IRToolDef)
	for _, d := range r.Set.Defs {
		byClient[d.ClientName] = d
	}
	if d := byClient["bash"]; d.WireName != "run_terminal_command" || d.Kind != KindMapped {
		t.Errorf("bash = %+v, want wire run_terminal_command kind mapped", d)
	}
	if d := byClient["test_tool"]; d.WireName != "mcp__test_tool" || d.Kind != KindVirtual {
		t.Errorf("test_tool = %+v, want virtualized mcp__test_tool (unknown-custom policy 2026-09-30)", d)
	}
	// Task is a harness spawner: it must never claim a wire name.
	if d := byClient["Task"]; d.Kind == KindDirect || d.WireName == "Task" {
		t.Errorf("Task = %+v, want virtualized/delegated off the wire", d)
	}
	if got := r.Set.RestoreName("run_terminal_command"); got != "bash" {
		t.Errorf("RestoreName = %q, want bash", got)
	}
	// Map-first restore matches ToolMapper semantics.
	mapper := NewToolMapper([]byte(body))
	for wire, want := range map[string]string{"run_terminal_command": "bash"} {
		if got := mapper.RestoreName(wire); got != want {
			t.Errorf("mapper restores %s to %q, IR to %q", wire, got, want)
		}
	}
}

func TestOwnFirstWinsDedupe(t *testing.T) {
	// Hermes terminal + execute_code both map to run_terminal_command; the
	// first keeps the official name, the later virtualizes.
	body := `{"messages":[],"tools":[` + irFuncTool("terminal") + `,` + irFuncTool("execute_code") + `]}`
	r := DecodeRequestIR([]byte(body))
	r.Own()
	if got := r.Set.Reverse["run_terminal_command"]; got != "terminal" {
		t.Errorf("owner = %q, want terminal", got)
	}
	virt := r.Set.Forward["execute_code"]
	if virt == "run_terminal_command" || virt == "" || !strings.HasPrefix(virt, "mcp__") {
		t.Errorf("execute_code forward = %q, want mcp__ virtualization", virt)
	}
	if got := r.Set.Reverse[virt]; got != "execute_code" {
		t.Errorf("virtual owner = %q, want execute_code", got)
	}
	found := false
	for _, d := range r.Diagnostics {
		if d.Code == DiagDedupeVirtual {
			found = true
		}
	}
	if !found {
		t.Error("no ir.dedupe_virtual diagnostic")
	}
}

func TestOwnFirstWinsOwnership(t *testing.T) {
	// Roo-Code offers write_file and write_to_file→write_file. First claimer
	// wins unconditionally (ToUpstream dedupe): native-first keeps identity
	// and the mapped tool virtualizes; mapped-first keeps the official name
	// and the NATIVE tool virtualizes to mcp__write_file. Ground truth:
	// TestToolMapperWireNameOwnerRoundTrips.
	orders := []struct {
		client []string
		owners map[string]string
	}{
		{
			[]string{"write_file", "write_to_file"},
			map[string]string{"write_file": "write_file", "mcp__write_to_file": "write_to_file"},
		},
		{
			[]string{"write_to_file", "write_file"},
			map[string]string{"write_file": "write_to_file", "mcp__write_file": "write_file"},
		},
	}
	for _, o := range orders {
		body := `{"messages":[],"tools":[` + irFuncTool(o.client[0]) + `,` + irFuncTool(o.client[1]) + `]}`
		r := DecodeRequestIR([]byte(body))
		r.Own()
		for wire, owner := range o.owners {
			if got := r.Set.RestoreName(wire); got != owner {
				t.Errorf("order %v: restore(%s) = %q, want %q", o.client, wire, got, owner)
			}
		}
	}
}

func TestDecodeChoice(t *testing.T) {
	auto := DecodeRequestIR([]byte(`{"messages":[]}`))
	if auto.Choice.Mode != "auto" || !auto.Set.Parallel {
		// Parallel is set by Own; decode alone reports the mode.
		if auto.Choice.Mode != "auto" {
			t.Errorf("absent choice = %q, want auto", auto.Choice.Mode)
		}
	}
	none := DecodeRequestIR([]byte(`{"messages":[],"tool_choice":"none"}`))
	if none.Choice.Mode != "none" {
		t.Errorf("none choice = %q", none.Choice.Mode)
	}
	pinned := DecodeRequestIR([]byte(`{"messages":[],"tools":[` + irFuncTool("bash") + `],` +
		`"tool_choice":{"type":"function","function":{"name":"bash"}}}`))
	pinned.Own()
	if pinned.Choice.Mode != "pinned" || pinned.Choice.PinnedClient != "bash" {
		t.Fatalf("pinned choice = %+v", pinned.Choice)
	}
	if pinned.Set.Choice.PinnedWire != "run_terminal_command" {
		t.Errorf("pinned wire = %q, want run_terminal_command", pinned.Set.Choice.PinnedWire)
	}
	if pinned.Set.Parallel {
		t.Error("pinned choice must disable parallel")
	}
}

func TestRejectLoss(t *testing.T) {
	warn := []IRDiagnostic{{Code: DiagRename, Severity: SeverityWarning}}
	errDiag := []IRDiagnostic{{Code: DiagFloorPinStrip, Severity: SeverityError}}
	if err := RejectLoss(append(warn, errDiag...), LossAllow); err != nil {
		t.Errorf("allow rejects: %v", err)
	}
	if err := RejectLoss(warn, LossSafe); err != nil {
		t.Errorf("safe rejects warnings: %v", err)
	}
	if err := RejectLoss(errDiag, LossSafe); err == nil {
		t.Error("safe passes error-severity loss")
	} else if !strings.Contains(err.Error(), DiagFloorPinStrip) {
		t.Errorf("rejection omits code: %v", err)
	}
	if err := RejectLoss(warn, LossStrict); err == nil {
		t.Error("strict passes warnings")
	}
	if err := RejectLoss(nil, LossStrict); err != nil {
		t.Errorf("strict rejects clean turn: %v", err)
	}
}

func TestQualityFor(t *testing.T) {
	if q := QualityFor(nil); q != QualityLossless {
		t.Errorf("clean = %q", q)
	}
	if q := QualityFor([]IRDiagnostic{{Severity: SeverityWarning}}); q != QualityLossyDocumented {
		t.Errorf("warnings = %q", q)
	}
	if q := QualityFor([]IRDiagnostic{{Severity: SeverityWarning}, {Severity: SeverityError}}); q != QualityDegraded {
		t.Errorf("error = %q", q)
	}
}

func TestRequestRegistry(t *testing.T) {
	dec := RequestDecoderFor(SurfaceResponses)
	if dec == nil {
		t.Fatal("no responses decoder")
	}
	r := dec([]byte(`{"input":[{"role":"user","content":"hi"}]}`))
	if r.Surface != SurfaceResponses {
		t.Errorf("surface = %q", r.Surface)
	}
	if RequestDecoderFor(SurfaceFormat("nonsense")) == nil {
		t.Fatal("unknown surface has no fallback decoder")
	}
	found := false
	for _, s := range RegisteredSurfaces() {
		if s == SurfaceChat {
			found = true
		}
	}
	if !found {
		t.Error("chat surface not registered")
	}
	// Custom surface onboarding: decoder + renderer pair, never pipeline surgery.
	const custom SurfaceFormat = "custom-test"
	RegisterRequestDecoder(custom, func(body []byte) *IRRequest {
		r := DecodeRequestIR(body)
		r.Surface = custom
		return r
	})
	if RequestDecoderFor(custom) == nil {
		t.Error("custom decoder not registered")
	}
}

func TestResponseRegistryChatDefault(t *testing.T) {
	rend := ResponseRendererFor(SurfaceChat)
	if rend == nil {
		t.Fatal("no chat renderer registered")
	}
	body := `{"messages":[{"role":"user","content":"hi"}],"tools":[` + irFuncTool("bash") + `]}`
	_, mapper, _, err := NormalizeRequestMappedIR([]byte(body), "", DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	calls := AssembleIRCalls(IRToolSet{Reverse: map[string]string{"run_terminal_command": "bash"}}, []any{
		map[string]any{"id": "c1", "function": map[string]any{"name": "run_terminal_command", "arguments": `{"command":"pwd"}`}},
	})
	out := rend.Render(mapper, calls)
	if len(out) != 1 || out[0].ClientName != "bash" {
		t.Fatalf("rendered = %+v", out)
	}
	if out[0].Args != `{"command":"pwd"}` {
		t.Errorf("args = %s (non-family mapper must pass through verbatim)", out[0].Args)
	}
}

func TestAssembleAnthropicBlocks(t *testing.T) {
	set := IRToolSet{Reverse: map[string]string{"run_terminal_command": "bash"}}
	calls := AssembleAnthropicCalls(set, []any{
		map[string]any{"type": "tool_use", "id": "t1", "name": "run_terminal_command", "input": map[string]any{"command": "ls"}},
		map[string]any{"type": "text", "text": "hi"},
	})
	if len(calls) != 1 {
		t.Fatalf("assembled %d calls, want 1 (text block skipped)", len(calls))
	}
	if calls[0].ClientName != "bash" || calls[0].ID != "t1" {
		t.Errorf("call = %+v", calls[0])
	}
	if !strings.Contains(calls[0].Args, `"command"`) {
		t.Errorf("args = %s", calls[0].Args)
	}
}
