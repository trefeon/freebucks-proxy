package upstream

import (
	_ "embed"
	"encoding/json"
	"sync"
)

// cliToolsFixture is the exact 16-tool coding-agent toolset captured from the
// official Freebuff CLI (read_files, str_replace, write_file,
// run_terminal_command, code_search, glob, list_directory, write_todos,
// web_search, read_url, ask_user, suggest_followups, gravity_index,
// render_ui, skill, report_project_profile). The JSON lives in
// testdata/cli-tools.json (byte-identical to the live capture) and is
// embedded so the wire floor is always the captured shape, never a
// hand-maintained copy. Upstream's free-mode traffic gate requires these
// full-schema declarations on the wire (minimal skeletons get 503).
//
//go:embed testdata/cli-tools.json
var cliToolsFixture []byte

var (
	defaultToolsOnce sync.Once
	defaultToolsList []any
)

// defaultCliTools returns a copy of the official coding agent tool declarations.
func defaultCliTools() []any {
	defaultToolsOnce.Do(func() {
		var tools []any
		if err := json.Unmarshal(cliToolsFixture, &tools); err == nil {
			defaultToolsList = tools
		}
	})
	out := make([]any, len(defaultToolsList))
	copy(out, defaultToolsList)
	return out
}
