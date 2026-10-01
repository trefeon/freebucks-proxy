package convert

import "testing"

func mkContinueTool(name string) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":       name,
			"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}
}

// Continue ships snake_case BuiltInToolNames (reference/agents/continue
// core/tools/builtIn.ts, carried in the corpus fixture). The mapping table
// once held concatenated keys (searchweb, fetchurlcontent, globsearch) that
// never matched those names, so the tools virtualized (mcp__*) or rode
// unrenamed instead of translating to their official wire names — a silent
// loss of first-party tool surface at the gate. The corpus sweep accepts
// either outcome (it only pins restore + wire legality), so this asserts the
// translation itself, through the mapped rename path.
func TestContinueSnakeCaseNamesTranslate(t *testing.T) {
	out, _, err := NormalizeRequestMapped(mustJSON(t, map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{
			mkContinueTool("search_web"),
			mkContinueTool("fetch_url_content"),
			mkContinueTool("file_glob_search"),
		},
	}), "")
	if err != nil {
		t.Fatalf("NormalizeRequestMapped: %v", err)
	}
	tools, _ := decode(t, out)["tools"].([]any)
	got := map[string]bool{}
	for _, n := range toolNamesOf(tools) {
		got[n] = true
	}
	for _, want := range []string{"web_search", "read_url", "glob"} {
		if !got[want] {
			t.Errorf("wire names %v missing %q (continue name was not translated)", toolNamesOf(tools), want)
		}
	}
}
