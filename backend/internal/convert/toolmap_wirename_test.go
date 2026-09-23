package convert

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// The universal wire-name guarantee, in one place: whatever a client calls its
// tools — known mapping, foreign harness name, client MCP name, dotted
// namespacing, spaces, non-ASCII, or 200 characters of generated name — the
// request must reach the upstream in a shape its tools endpoint accepts, and
// every wire name must come back to the client as the name the client sent.
// A single illegal name used to fail the whole upstream request
// (reference/agents/Codewhale registers `web.run`,
// crates/tui/src/tools/web_run.rs:356), which made the proxy unusable for any
// client outside the mapping table that emitted one.

// wireNameGrammar is the upstream's documented function-name rule
// ("must be a-z, A-Z, 0-9, or contain underscores and dashes, with a maximum
// length of 64").
var wireNameGrammar = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// TestWireVirtualNameGrammar pins the legalizer itself: legal short names keep
// the historic mcp__<name> form (backward compatibility for the names already
// in the wild), everything else comes out legal, bounded and unique.
func TestWireVirtualNameGrammar(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // "" = assert properties only
	}{
		{name: "legal short keeps historic form", in: "bash", want: "mcp__bash"},
		{name: "legal at the prefix budget", in: strings.Repeat("a", 59), want: "mcp__" + strings.Repeat("a", 59)},
		{name: "legal but too long for the prefix", in: strings.Repeat("b", 60)},
		{name: "legal at the wire limit", in: strings.Repeat("c", 64)},
		{name: "dotted namespace", in: "web.run"},
		{name: "space", in: "web fetch"},
		{name: "slash", in: "browser/navigate"},
		{name: "colon", in: "mcp:tool"},
		{name: "unicode", in: "日本語ツール"},
		{name: "emoji", in: "🙂"},
		{name: "non-ascii only", in: "\u00e9"},
		{name: "200 chars", in: strings.Repeat("x", 200)},
		{name: "over-long unicode", in: strings.Repeat("日", 100)},
	}
	seen := map[string]string{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := wireVirtualName(tc.in)
			if !wireNameGrammar.MatchString(got) {
				t.Errorf("wireVirtualName(%q) = %q, which does not match the wire grammar %s", tc.in, got, wireNameGrammar)
			}
			if got != wireVirtualName(tc.in) {
				t.Errorf("wireVirtualName(%q) is not deterministic", tc.in)
			}
			if tc.want != "" && got != tc.want {
				t.Errorf("wireVirtualName(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if ForeignHarnessToolNames[got] {
				t.Errorf("wireVirtualName(%q) = %q, a foreign harness name", tc.in, got)
			}
			if prev, dup := seen[got]; dup {
				t.Errorf("wireVirtualName collision: %q and %q both map to %q", prev, tc.in, got)
			}
			seen[got] = tc.in
		})
	}
}

// TestWireVirtualNameDistinctBases: names that sanitize to the SAME base must
// still get distinct wire names, otherwise a legalized wire would deliver one
// client's tool call to another client tool.
func TestWireVirtualNameDistinctBases(t *testing.T) {
	got := map[string]string{}
	for _, in := range []string{"a.b", "a b", "a/b", "a\tb", "a.b."} {
		w := wireVirtualName(in)
		if !wireNameGrammar.MatchString(w) {
			t.Errorf("wireVirtualName(%q) = %q, illegal for the wire", in, w)
		}
		if prev, dup := got[w]; dup {
			t.Errorf("wireVirtualName(%q) and %q both = %q", in, prev, w)
		}
		got[w] = in
	}
}

// normalizeToolNames runs one request whose tools[] are exactly names, in
// order, through the mapped ingress path and returns the wire tools plus the
// request's mapper.
func normalizeToolNames(t *testing.T, names []string) ([]any, ToolMapper) {
	t.Helper()
	tools := make([]any, 0, len(names))
	for _, name := range names {
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": "tool " + name,
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"input": map[string]any{"type": "string"}},
				},
			},
		})
	}
	body, err := json.Marshal(map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools":    tools,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	norm, mapper, err := NormalizeRequestMapped(body, "")
	if err != nil {
		t.Fatalf("NormalizeRequestMapped: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(norm, &parsed); err != nil {
		t.Fatalf("unmarshal normalized: %v", err)
	}
	wireTools, _ := parsed["tools"].([]any)
	return wireTools, mapper
}

// restoreViaChunk pushes one wire name through FromUpstreamChunk in the given
// shape and returns the name the client sees.
func restoreViaChunk(mapper ToolMapper, shape, wire string) string {
	call := map[string]any{
		"index":    0,
		"id":       "call_universal",
		"type":     "function",
		"function": map[string]any{"name": wire, "arguments": "{}"},
	}
	choice := map[string]any{"index": 0, "finish_reason": "tool_calls"}
	switch shape {
	case "delta":
		choice["delta"] = map[string]any{"tool_calls": []any{call}}
	default:
		choice["message"] = map[string]any{"tool_calls": []any{call}}
	}
	chunk := map[string]any{"choices": []any{choice}}
	mapper.FromUpstreamChunk(chunk)
	tcs, _ := choice[shape].(map[string]any)["tool_calls"].([]any)
	fn, _ := tcs[0].(map[string]any)["function"].(map[string]any)
	name, _ := fn["name"].(string)
	return name
}

// TestUniversalClientToolNamesRoundTrip is the cross-client guarantee: a
// toolset mixing official, mapped, foreign-harness, unknown and wire-illegal
// names must reach the upstream in a legal, unique, non-foreign shape and
// restore byte-exactly to the client's own names on every response path.
func TestUniversalClientToolNamesRoundTrip(t *testing.T) {
	cases := []struct {
		name  string
		tools []string
	}{
		{
			name: "opencode v2 dual shell plus illegal names",
			tools: []string{
				"glob",                  // official, verbatim
				"bash",                  // mapped -> run_terminal_command
				"Bash",                  // foreign harness name -> virtualized
				"hub",                   // unknown but legal -> verbatim
				"complete_compaction",   // new upstream custom tool -> verbatim
				"web.run",               // Codewhale dotted name -> legalized
				"web fetch",             // space -> legalized
				"a/b",                   // slash -> legalized
				"日本語ツール",                // non-ASCII -> legalized
				strings.Repeat("x", 80), // over the wire limit -> legalized
			},
		},
		{
			name:  "same illegal name twice",
			tools: []string{"web.run", "web.run"},
		},
		{
			name:  "sanitization bases collide",
			tools: []string{"a.b", "a b", "a/b"},
		},
		{
			name:  "illegal name shadowing an official name",
			tools: []string{"read files", "read_files"},
		},
		{
			name:  "illegal name shadowing a foreign name",
			tools: []string{"exec command", "exec_command"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wireTools, mapper := normalizeToolNames(t, tc.tools)

			// One wire entry per client tool; the injected signature tools
			// are appended, so wire index i belongs to client tool i.
			if len(wireTools) < len(tc.tools) {
				t.Fatalf("wire tools = %d, want at least the %d client tools", len(wireTools), len(tc.tools))
			}
			seen := map[string]int{}
			for i, client := range tc.tools {
				fn, _ := wireTools[i].(map[string]any)["function"].(map[string]any)
				wire, _ := fn["name"].(string)
				if !wireNameGrammar.MatchString(wire) {
					t.Errorf("client tool %q reached the wire as %q, which the upstream tools grammar rejects", client, wire)
				}
				if ForeignHarnessToolNames[wire] {
					t.Errorf("client tool %q reached the wire as foreign harness name %q", client, wire)
				}
				if prev, dup := seen[wire]; dup {
					t.Errorf("wire name %q used by both %q and %q — strict upstreams reject duplicate tool names", wire, tc.tools[prev], client)
				}
				seen[wire] = i
				if got := mapper.RestoreName(wire); got != client {
					t.Errorf("RestoreName(%q) = %q, want %q (client tool %d)", wire, got, client, i)
				}
				for _, shape := range []string{"delta", "message"} {
					if got := restoreViaChunk(mapper, shape, wire); got != client {
						t.Errorf("choices[].%s.tool_calls[] restore of %q = %q, want %q", shape, wire, got, client)
					}
				}
			}

			if v := ClassifyWireTools(wireTools); len(v.Genuine) == 0 {
				t.Errorf("wire reads with no genuine signature tool (names %v)", v.Names)
			} else if sig := WireForeignSignal(v); sig != "" {
				t.Errorf("wire reads %q, want empty (foreign harness %v)", sig, v.ForeignHarness)
			}
		})
	}
}

// TestIllegalToolNameKeepsItsOwnIdentity: legalizing a name must not change
// which client tool the model's call belongs to, including when the legalized
// form spells an official name (read files -> read_files).
func TestIllegalToolNameKeepsItsOwnIdentity(t *testing.T) {
	wireTools, mapper := normalizeToolNames(t, []string{"read files", "read_files"})
	first, _ := wireTools[0].(map[string]any)["function"].(map[string]any)["name"].(string)
	second, _ := wireTools[1].(map[string]any)["function"].(map[string]any)["name"].(string)
	if first == "read_files" {
		t.Fatalf("the legalized dotted name claimed the official wire name %q — the real read_files tool lost its identity by array order", first)
	}
	if got := mapper.RestoreName(first); got != "read files" {
		t.Errorf("RestoreName(%q) = %q, want \"read files\"", first, got)
	}
	if got := mapper.RestoreName(second); got != "read_files" {
		t.Errorf("RestoreName(%q) = %q, want read_files", second, got)
	}
}

// TestIllegalToolChoicePinRewrite: a tool_choice pinned to a name the wire
// cannot carry must point at the legalized wire name, or the upstream rejects
// the request for an unknown tool.
func TestIllegalToolChoicePinRewrite(t *testing.T) {
	body := `{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"web.run","parameters":{"type":"object"}}}],` +
		`"tool_choice":{"type":"function","function":{"name":"web.run"}}}`
	norm, _, err := NormalizeRequestMapped([]byte(body), "")
	if err != nil {
		t.Fatalf("NormalizeRequestMapped: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(norm, &parsed); err != nil {
		t.Fatalf("unmarshal normalized: %v", err)
	}
	tools, _ := parsed["tools"].([]any)
	fn, _ := tools[0].(map[string]any)["function"].(map[string]any)
	wire, _ := fn["name"].(string)
	tc, _ := parsed["tool_choice"].(map[string]any)
	pinned, _ := tc["function"].(map[string]any)["name"].(string)
	if pinned != wire {
		t.Errorf("tool_choice pins %q but the wire tool is %q — the upstream sees a choice for an unknown tool", pinned, wire)
	}
	if !wireNameGrammar.MatchString(pinned) {
		t.Errorf("pinned tool_choice name %q is not wire-legal", pinned)
	}
}
