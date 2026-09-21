package convert

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
)

// Tool-name tolerance mapping (issue #140).
//
// Upstream's free-mode gate classifies a request offering tools with NO
// signature tool as third-party (foreign_toolset → downgrade to
// ling-3.0-tiny:free, backend/internal/wirefacts/testdata/wire/common/src/
// constants/foreign-client-signals.ts; generated mirror GenericToolNames in
// toolnames_gen.go) and the trust system permanently caps any
// account seen sending a foreign tool schema (third_party_client sticky cap).
// The proxy neutralizes both by renaming common third-party harness tool
// names to the official signature equivalents for the upstream wire and
// renaming them BACK on every response path — the client only ever sees its
// own names.
//
// Design constraint that keeps this safe: the client's PARAMETER SCHEMA is
// forwarded untouched (after structural normalization). The model fills
// arguments per the schema it was shown, so arguments come back in client
// shape and only the NAME needs restoring downstream. No argument translation,
// no schema substitution, no invented structure.

// clientToOfficial maps well-known third-party harness tool names to the
// official codebuff signature tool they behave as. Keys are lowercase.
// Only names whose behavior genuinely matches are mapped; everything else
// passes through untouched (an unknown rename would break the client's
// dispatcher). Behavior table owned by this file; the canonical upstream tool
// names it maps onto are generated in toolnames_gen.go from
// backend/internal/wirefacts/testdata/wire/common/src/tools/constants.ts:
//
//	read_files{paths[]}   write_file{path,instructions,content}
//	run_terminal_command{command,...}   glob{pattern,...}
//	write_todos{todos[{task,completed}]}
var clientToOfficial = map[string]string{
	// Claude Code / generic agentic CLIs
	"read":      "read_files",
	"view":      "read_files",
	"edit":      "str_replace",
	"write":     "write_file",
	"bash":      "run_terminal_command",
	"execute":   "run_terminal_command",
	"ls":        "list_directory",
	"grep":      "code_search",
	"todo":      "write_todos",
	"todowrite": "write_todos",

	// Cline / Roo Code
	"read_file":           "read_files",
	"write_to_file":       "write_file",
	"replace_in_file":     "str_replace",
	"execute_command":     "run_terminal_command",
	"list_files":          "list_directory",
	"search_files":        "code_search",
	"apply_diff":          "apply_patch",
	"edit_file":           "str_replace",
	"search_replace":      "str_replace",
	"search_and_replace":  "str_replace",
	"codebase_search":     "code_search",
	"update_todo_list":    "write_todos",
	"read_command_output": "run_terminal_command",
	"editor":              "str_replace",
	"fetch_web":           "read_url",
	"search":              "code_search",

	// Codex / OpenAI harnesses
	"shell":          "run_terminal_command",
	"shell_command":  "run_terminal_command",
	"local_shell":    "run_terminal_command",
	"container_exec": "run_terminal_command",
	"exec_command":   "run_terminal_command",
	"exec":           "run_terminal_command",

	// Aider
	"command":       "run_terminal_command",
	"replace_lines": "str_replace",

	// Qwen-Code
	"run_shell_command": "run_terminal_command",
	"grep_search":       "code_search",
	"todo_write":        "write_todos",
	"web_fetch":         "read_url",
	"save_memory":       "write_todos",

	// Goose
	"developer__shell":       "run_terminal_command",
	"developer__bash":        "run_terminal_command",
	"developer__text_editor": "str_replace",
	"developer__read":        "read_files",
	"developer__write":       "write_file",
	"developer__edit":        "str_replace",
	"computer__execute":      "run_terminal_command",
	// Goose dot-mangled forms (model rewrites __ to . on the wire)
	"developer.shell":       "run_terminal_command",
	"developer.text_editor": "str_replace",

	// Continue (keys are matched lowercase)
	"readfile":             "read_files",
	"editfile":             "str_replace",
	"createnewfile":        "write_file",
	"runterminalcommand":   "run_terminal_command",
	"grepsearch":           "code_search",
	"globsearch":           "glob",
	"fetchurlcontent":      "read_url",
	"searchweb":            "web_search",
	"viewsubdirectory":     "list_directory",
	"singlefindandreplace": "str_replace",
	// Kimi-CLI (reference/agents/kimi-cli src/kimi_cli/tools/*)
	"writefile":   "write_file",
	"settodolist": "write_todos",
	"fetchurl":    "read_url",
	// Crush (reference/agents/crush internal/agent/tools/*.go)
	"todos": "write_todos",
	"rg":    "code_search",

	// Pi / Oh My Pi (OMP)
	"powershell": "run_terminal_command",
	"find":       "find_files",
	"edit-diff":  "apply_patch",

	// Kilocode / OpenCode
	"execute_bash": "run_terminal_command",
	"fuzzy_search": "code_search",
	"list_dir":     "list_directory",
	"websearch":    "web_search",
	"webfetch":     "read_url",

	// Gemini-CLI (reference/agents/gemini-cli packages/core/src/tools/
	// definitions/base-declarations.ts wire names + tool-names.ts legacy aliases)
	"read_many_files":     "read_files",
	"replace":             "str_replace",
	"google_web_search":   "web_search",
	"activate_skill":      "skill",
	"search_file_content": "code_search",
	// Hermes (reference/agents/hermes-agent toolsets.py + agent/* tool refs)
	"terminal":     "run_terminal_command",
	"execute_code": "run_terminal_command",
	"web_extract":  "read_url",
	"patch":        "str_replace",
	"todo_list":    "write_todos",
	"skills_list":  "skill",
	"skill_view":   "skill",
	"skill_manage": "skill",
	// OpenHands agent-server (reference/harnesses/OpenHands __tests__ tool_call
	// fixtures carry the wire function name; terminal shares the Hermes entry)
	"invoke_skill": "skill",
	"run_ipython":  "run_terminal_command",
	// Crush-additions (reference/agents/crush internal/agent/tools/*.go)
	"fetch":       "read_url",
	"multiedit":   "str_replace",
	"sourcegraph": "code_search",
	// Kimi-additions (reference/agents/kimi-cli src/kimi_cli/tools/file/replace.py)
	"strreplacefile": "str_replace",
	// Codewhale (reference/agents/Codewhale crates/tui/src/tools/*.rs)
	"exec_shell":  "run_terminal_command",
	"fetch_url":   "read_url",
	"web.fetch":   "read_url",
	"grep_files":  "code_search",
	"file_search": "glob",
	// jcode (reference/agents/jcode crates/jcode-tool-types/src/lib.rs aliases +
	// crates/jcode-app-core/src/tool/*.rs; multiedit/patch share entries above)
	"shell_exec": "run_terminal_command",
	"agentgrep":  "code_search",
	"file_grep":  "code_search",
	"todoread":   "write_todos",
	"todo_read":  "write_todos",
	// Reasonix (reference/agents/DeepSeek-Reasonix internal/tool/builtin/*.go)
	"multi_edit":    "str_replace",
	"complete_step": "write_todos",

	// Cursor
	"strreplace": "str_replace",
}

// officialTools is the set of official codebuff signature tool names a
// mapping target must be in. Guards against typos introducing a name
// upstream does not recognize (which would itself be foreign).
var officialTools = map[string]bool{
	"apply_patch":          true,
	"code_search":          true,
	"end_turn":             true,
	"glob":                 true,
	"list_directory":       true,
	"read_files":           true,
	"read_subtree":         true,
	"run_terminal_command": true,
	"skill":                true,
	"str_replace":          true,
	"web_search":           true,
	"write_file":           true,
	"write_todos":          true,
	"find_files":           true,
	"read_url":             true,
}

func init() {
	for _, official := range clientToOfficial {
		if official == "" {
			continue
		}
		if !officialTools[official] {
			panic("convert: clientToOfficial maps to non-official tool " + official)
		}
	}
}

// ToolMapper is one request's bidirectional tool-name map. Compute it from
// the request body BEFORE normalization (ToUpstream rewrites the request's
// tools array), then thread it to the response relays (FromUpstream restores
// client names). Zero value is valid: nothing maps, relays pass through.
type ToolMapper struct {
	upstreamToClient map[string]string // response path: official/MCP → original
	clientToUpstream map[string]string // request path: original → official/MCP
	msgs             int               // len(messages) (or len(input) for Responses) in the scanned body
	tools            int               // len(tools) in the scanned body
}

func isForeignHarness(name string) bool {
	return ForeignHarnessToolNames[name] || strings.HasPrefix(strings.ToLower(name), "cron")
}

// Wire tool-name grammar. The upstream is an OpenAI-shaped tools endpoint, and
// every function name on the wire must match ^[A-Za-z0-9_-]{1,64}$ ("must be
// a-z, A-Z, 0-9, or contain underscores and dashes, with a maximum length of
// 64"). Client harnesses do not all obey it — reference/agents/Codewhale
// registers `web.run` (crates/tui/src/tools/web_run.rs:356) — and ONE illegal
// name fails the whole upstream request, so an illegal client name is
// legalized for the wire and restored to the client's own name downstream.
// This is the universal path for clients whose tools the mapping table does
// not know: unknown-but-legal names already pass through verbatim, and
// unknown-and-illegal names now fit the wire instead of breaking the request.
const (
	wireToolNameMaxLen = 64
	wireVirtualPrefix  = "mcp__"
	// 8 hex chars of sha256 over the client name: two distinct client names
	// can collapse onto one substituted/truncated base, so the tail carries
	// the uniqueness the base cannot.
	wireHashTailLen = 8
)

// isWireToolNameRune reports whether r may appear in a wire tool name.
func isWireToolNameRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') || r == '_' || r == '-'
}

// wireToolNameOK reports whether name already satisfies the wire grammar
// (length is bytes, matching the upstream's own limit check).
func wireToolNameOK(name string) bool {
	if name == "" || len(name) > wireToolNameMaxLen {
		return false
	}
	for _, r := range name {
		if !isWireToolNameRune(r) {
			return false
		}
	}
	return true
}

// sanitizeWireBase maps every non-grammar rune to '_' and guarantees a
// non-empty, ASCII-only base.
func sanitizeWireBase(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		if isWireToolNameRune(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "tool"
	}
	return b.String()
}

// wireVirtualName returns the wire name for a client tool that cannot put its
// own name on the wire (grammar violation, or a name collision). A legal name
// that fits keeps the historic mcp__<client name>; anything else is
// ASCII-legalized, bounded to the wire limit and given a hash-of-the-original
// tail, so distinct client names never collapse onto one wire name.
func wireVirtualName(clientName string) string {
	if wireToolNameOK(clientName) && len(wireVirtualPrefix)+len(clientName) <= wireToolNameMaxLen {
		return wireVirtualPrefix + clientName
	}
	sum := sha256.Sum256([]byte(clientName))
	tail := hex.EncodeToString(sum[:wireHashTailLen/2])
	budget := wireToolNameMaxLen - len(wireVirtualPrefix) - 1 - len(tail)
	base := sanitizeWireBase(clientName)
	if len(base) > budget {
		base = base[:budget]
	}
	return wireVirtualPrefix + base + "_" + tail
}

// resolveUpstreamTool decides the wire name for a client tool.
// Tools mapped in clientToOfficial are mapped to their official codebuff signature tool.
// Unmapped foreign harness tools (matching ForeignHarnessToolNames exact casing)
// are safely virtualized into the MCP namespace (mcp__<tool_name>) to prevent triggering
// foreign_tool_names. Unrecognized custom tools and existing MCP tools pass through verbatim.
func resolveUpstreamTool(origName string, params map[string]any) string {
	if origName == "" {
		return ""
	}
	if officialTools[origName] || CustomSignatureToolNames[origName] {
		return origName
	}

	lower := strings.ToLower(origName)

	if official, ok := clientToOfficial[lower]; ok && official != "" {
		return official
	}

	// Universal grammar gate: names the wire cannot carry are legalized here,
	// before the pass-through rules below — one illegal name fails the whole
	// upstream request, so "pass through verbatim" is not an option for them.
	if !wireToolNameOK(origName) {
		return wireVirtualName(origName)
	}

	if strings.Contains(origName, "__") {
		return origName
	}

	// Check exact case in ForeignHarnessToolNames (e.g. PascalCase "Task", "Agent",
	// "AskUserQuestion" from Claude Code, or "browser_exec" from OpenClaw).
	// Lowercase agentic tools like OMP's "task" are not in ForeignHarnessToolNames.
	if ForeignHarnessToolNames[origName] || strings.HasPrefix(lower, "cron") {
		return wireVirtualName(origName)
	}

	return origName
}

// MsgCount is the client message count retained by NewToolMapper (0 when the
// body had no messages/input array). Feeds the console "N MSG" segment.
func (m ToolMapper) MsgCount() int { return m.msgs }

// ToolCount is the client tool count retained by NewToolMapper (0 when the
// body had no tools array). Feeds the console "N TOOL" segment.
func (m ToolMapper) ToolCount() int { return m.tools }

// NewToolMapper scans a raw request body for function-tool names and returns
// the mapper for it. Names that are already official (or unknown) produce no
// entry — they round-trip unchanged. body may be nil/invalid (returns empty).
func NewToolMapper(body []byte) ToolMapper {
	var payload struct {
		Messages []json.RawMessage `json:"messages"`
		Input    []json.RawMessage `json:"input"`
		Tools    []struct {
			Function struct {
				Name       string         `json:"name"`
				Parameters map[string]any `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ToolMapper{}
	}
	m := ToolMapper{
		upstreamToClient: make(map[string]string),
		clientToUpstream: make(map[string]string),
	}
	m.msgs = len(payload.Messages)
	if m.msgs == 0 {
		m.msgs = len(payload.Input) // Responses API carries input[], not messages[]
	}
	m.tools = len(payload.Tools)
	for _, t := range payload.Tools {
		name := t.Function.Name
		if name == "" {
			continue
		}
		upstreamName := resolveUpstreamTool(name, t.Function.Parameters)
		if upstreamName != "" && upstreamName != name {
			m.clientToUpstream[name] = upstreamName
			// Provisional reverse slot so a mapper used WITHOUT ToUpstream
			// still restores. Ownership is finalized by ToUpstream's ordered
			// pass, which knows which client tool actually claimed the shared
			// wire name: here only the array order is visible, and a client
			// tool whose own name IS the wire name may appear after a mapped
			// one (see TestToolMapperWireNameOwnerRoundTrips).
			if _, taken := m.upstreamToClient[upstreamName]; !taken {
				m.upstreamToClient[upstreamName] = name
			}
		}
		if official, ok := clientToOfficial[strings.ToLower(name)]; ok && official != "" && official != name {
			if _, taken := m.upstreamToClient[official]; !taken {
				m.upstreamToClient[official] = name
			}
		}
	}
	return m
}

// ToUpstream renames mapped client tool entries in the request payload IN
// PLACE (payload["tools"]) so the upstream wire carries official signature
// names. Idempotent. Name-unique: when two client tools resolve to the SAME
// wire name (e.g. Hermes terminal + execute_code both map to
// run_terminal_command), the first keeps the official name and later ones
// virtualize to mcp__<original> — restore already handles the namespace.
// Strict upstreams (DeepSeek, Muse Spark, MiMo) reject duplicate tool names
// outright ("Tool names must be unique"), so dedupe is unconditional.
//
// This pass is also what decides OWNERSHIP of every wire name: the client tool
// that keeps (or claims) a wire name is the one that name restores to, and a
// virtualized tool owns the mcp__<name> it was given. Name uniqueness depends
// on array order, so only this ordered pass can answer that question — a
// reverse map built up front (NewToolMapper) is a provisional guess.
func (m ToolMapper) ToUpstream(payload map[string]any) {
	tools, ok := payload["tools"].([]any)
	if !ok {
		return
	}
	used := make(map[string]bool, len(tools))
	for _, t := range tools {
		tool, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		upstreamName, hit := m.clientToUpstream[name]
		if !hit {
			params, _ := fn["parameters"].(map[string]any)
			upstreamName = resolveUpstreamTool(name, params)
		}
		if upstreamName != "" && upstreamName != name {
			fn["name"] = upstreamName
			// Preserve the original where the model can see it: some models
			// echo the description when choosing between similar tools.
			if desc, _ := fn["description"].(string); desc != "" && !strings.Contains(desc, "(client tool: "+name+")") {
				fn["description"] = strings.TrimSpace(desc) + " (client tool: " + name + ")"
			}
		}
		finalName, _ := fn["name"].(string)
		if finalName == "" {
			continue
		}
		if used[finalName] {
			// Duplicate wire name: virtualize this later occurrence so the
			// wire stays name-unique. Both entries stay callable; restore
			// maps the virtual name back to the client name downstream.
			// wireVirtualName also guarantees the result is a legal wire
			// name; the counter keeps it unique even when the client offered
			// the very same illegal name twice (whose virtualization is
			// idempotent, so the plain form would collide with itself).
			virt := wireVirtualName(name)
			for i := 2; used[virt]; i++ {
				virt = wireVirtualName(name + "#" + strconv.Itoa(i))
			}
			if m.upstreamToClient != nil {
				m.upstreamToClient[virt] = name
			}
			if m.clientToUpstream != nil {
				m.clientToUpstream[name] = virt
			}
			fn["name"] = virt
			finalName = virt
		} else if name == finalName {
			// The client's own name IS the wire name (nothing was renamed),
			// so THIS tool owns the slot: NewToolMapper pre-registers reverse
			// slots in array order but cannot know which client tool claims a
			// shared wire name, so a mapped tool's entry here would hand this
			// tool's calls to the wrong client tool (Roo-Code offers both
			// write_file and write_to_file->write_file).
			delete(m.upstreamToClient, finalName)
		} else {
			// First claimer of a renamed wire name owns the reverse slot.
			if m.upstreamToClient != nil {
				m.upstreamToClient[finalName] = name
			}
		}
		used[finalName] = true
	}
}

// RenameRequestToolChoice points tool_choice at the renamed official tool
// when the client pinned a mapped name. Handles both string form ("auto")
// and structured form {"type":"function","function":{"name":...}}.
func (m ToolMapper) RenameRequestToolChoice(payload map[string]any) {
	tc, ok := payload["tool_choice"]
	if !ok || tc == nil {
		return
	}
	rename := func(name string) string {
		if upstreamName, hit := m.clientToUpstream[name]; hit && upstreamName != "" {
			return upstreamName
		}
		if upstreamName := resolveUpstreamTool(name, nil); upstreamName != "" {
			return upstreamName
		}
		return name
	}
	switch v := tc.(type) {
	case string:
		if v != "none" && v != "auto" && v != "required" {
			payload["tool_choice"] = rename(v)
		}
	case map[string]any:
		if fn, ok := v["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); name != "" {
				fn["name"] = rename(name)
			}
		}
	}
}
