package convert

import "strings"

// Foreign-harness signal mirror (vendor 3420c99,
// common/src/constants/foreign-client-signals.ts).
//
// Upstream detects third-party clients offering tools on the free lane and
// downgrades the enforced signals to FREEBUFF_DOWNGRADE_MODEL_ID. This file
// mirrors the enforcement-relevant semantics so the proxy can classify its
// OWN wire the way upstream will — for diagnosis (issue #630), never for
// enforcement: the proxy serves whatever the client declared.
//
// Enforcement order upstream (detectForeignFreebuffClient):
//  0. foreign_tool_names / foreign_system_prompt — a harness tool name or
//     harness system-prompt marker settles it before the signature is
//     consulted. Enforced.
//  1. foreign_toolset — tools offered but NONE genuinely ours
//     (isGenuineSignatureTool). Enforced.
//  2. root_agent_no_tools / sampling_params — report-only, never enforced.
//
// A signature tool is GENUINE only with our parameter schema behind our
// name: custom tools (decide) count by name; parameterised tools count when
// the offered top-level keys are a non-empty subset of the canonical keys;
// zero-parameter tools (end_turn, task_completed) NEVER count — there is
// nothing structural to verify. A signature name without a genuine schema
// is the HOLLOW (laundering) shape: log-only upstream.
//
// Static-mirror limitations (mirror, do not transliterate): upstream derives
// canonical keys at runtime via z.toJSONSchema(toolParams[name]); here they
// are pinned from the toolParams sources at 3420c99. Upstream compares a
// zero-param tool's full shipped description; here only the stable lead
// sentence is pinned (the shipped text carries interpolated examples, so a
// full static copy would rot on the first vendor wording edit). The
// COMPOSIO spread inside toolNames is not statically resolvable and is
// excluded from the known-names set.

// ForeignSignal is one upstream foreign-client signal.
type ForeignSignal string

const (
	ForeignToolset      ForeignSignal = "foreign_toolset"
	ForeignToolNames    ForeignSignal = "foreign_tool_names"
	ForeignSystemPrompt ForeignSignal = "foreign_system_prompt"
	RootAgentNoTools    ForeignSignal = "root_agent_no_tools"
	SamplingParams      ForeignSignal = "sampling_params"
)

// EnforcedSignals mirrors upstream ENFORCED_SIGNALS: the signals that
// change what is served. The other two are measurements.
var EnforcedSignals = map[ForeignSignal]bool{
	ForeignToolset:      true,
	ForeignToolNames:    true,
	ForeignSystemPrompt: true,
}

// ForeignHarnessToolNames mirrors upstream FOREIGN_HARNESS_TOOL_NAMES:
// tool names that belong to a harness upstream does not ship. Offering ANY
// of them marks the request foreign, whatever else it carries. Generic
// lowercase names a user's local agent could plausibly take are
// deliberately absent upstream; those harnesses are caught by the schema
// rule instead.
var ForeignHarnessToolNames = map[string]bool{
	// Claude Code core tools.
	"Agent": true, "AskUserQuestion": true, "Bash": true, "BashOutput": true,
	"KillShell": true, "Edit": true, "MultiEdit": true, "Write": true,
	"Read": true, "Glob": true, "Grep": true, "NotebookEdit": true,
	"WebFetch": true, "WebSearch": true, "TodoWrite": true, "Task": true,
	"Skill": true, "SlashCommand": true, "EnterPlanMode": true,
	"ExitPlanMode": true, "EnterWorktree": true, "ExitWorktree": true,
	"ToolSearch": true, "CronCreate": true, "CronDelete": true,
	"CronList": true, "CronUpdate": true, "SendMessage": true,
	"ListAgents": true, "TaskStop": true, "TaskOutput": true,
	"Monitor": true, "ScheduleWakeup": true, "DesignSync": true,
	"Artifact": true,
	// Cursor.
	"AskQuestion": true, "ReadLints": true, "StrReplace": true,
	"Shell": true, "Delete": true,
	// Codex.
	"exec_command": true, "write_stdin": true, "request_user_input": true,
	// OpenClaw.
	"browser_exec": true, "delegate_task": true, "computer_use": true,
	// opencode.
	"todowrite": true, "todoread": true, "webfetch": true,
}

// ForeignHarnessPromptMarkers mirrors upstream
// FOREIGN_HARNESS_PROMPT_MARKERS: phrases a third-party harness writes into
// its system prompt and none of upstream's ever does.
var ForeignHarnessPromptMarkers = []string{
	"You are Claude Code",
	"Anthropic's official CLI",
	"cc_version=",
	"cc_entrypoint=",
}

// CustomSignatureToolNames mirrors upstream FREEBUFF_CUSTOM_TOOL_NAMES:
// tools defined outside toolNames with no schema to check, taken at face
// value like before. Vendor 40c75256 adds complete_compaction (the
// model-compaction tool shipped in the same commit as the detector): a legal
// wire name, so resolveUpstreamTool already passes it through — the entry
// owns the verbatim path past any future harness collision.
var CustomSignatureToolNames = map[string]bool{
	"decide":              true,
	"complete_compaction": true,
}

// ZeroParamSignatureTools are the signature tools defined WITHOUT
// parameters. They never count as genuine (a copied name plus {} is
// byte-identical to the real thing), so the proxy must never rely on one
// as its first-party marker.
var ZeroParamSignatureTools = map[string]bool{
	"end_turn":       true,
	"task_completed": true,
}

// canonicalToolParameterKeys pins the top-level input keys upstream ships
// per signature tool (toolParams inputSchema at 3420c99). Only the tools
// the proxy maps onto or injects need entries; every other name reads
// unknown (never genuine, never hollow — fail-closed).
var canonicalToolParameterKeys = map[string]map[string]bool{
	"read_files":           {"paths": true},
	"write_file":           {"path": true, "instructions": true, "content": true},
	"str_replace":          {"path": true, "replacements": true},
	"run_terminal_command": {"command": true, "process_type": true, "cwd": true, "timeout_seconds": true},
	"list_directory":       {"path": true},
	"code_search":          {"pattern": true, "flags": true, "cwd": true, "maxResults": true},
	"write_todos":          {"todos": true},
	"apply_patch":          {"operation": true},
	"read_url":             {"url": true, "max_chars": true},
	"web_search":           {"query": true, "depth": true},
	"glob":                 {"pattern": true, "cwd": true, "max_results": true},
	"find_files":           {"prompt": true},
	"skill":                {"name": true},
	"read_subtree":         {"paths": true, "maxTokens": true},
}

// upstreamToolNames pins the toolNames literals (common/src/tools/
// constants.ts at 3420c99) for the unrecognised-names helper. The trailing
// COMPOSIO spread is not statically resolvable and is excluded.
var upstreamToolNames = map[string]bool{
	"apply_patch": true, "add_subgoal": true, "add_message": true,
	"ask_user": true, "browser_logs": true, "code_search": true,
	"cloud_plan_ready": true, "create_plan": true, "end_turn": true,
	"find_files": true, "glob": true, "gravity_index": true,
	"list_directory": true, "lookup_agent_info": true,
	"propose_str_replace": true, "propose_write_file": true,
	"read_docs": true, "read_files": true, "read_subtree": true,
	"read_url": true, "render_ui": true, "run_file_change_hooks": true,
	"run_terminal_command": true, "set_messages": true, "set_output": true,
	"skill": true, "spawn_agents": true, "spawn_agent_inline": true,
	"str_replace": true, "suggest_followups": true, "task_completed": true,
	"think_deeply": true, "update_subgoal": true, "web_search": true,
	"write_file": true, "write_todos": true,
}

// shippedZeroParamLead pins the stable lead sentence of each zero-param
// tool's shipped description (end-turn.ts / task-completed.ts at 3420c99).
// Approximation of upstream's full-text compare (see file header): enough
// to tell the shipped paragraph from the one-line hollow definitions
// proxies inject, without pinning vendor prose that rots on rewording.
var shippedZeroParamLead = map[string]string{
	"end_turn":       "Only use this tool to hand control back to the user.",
	"task_completed": "Use this tool to signal that the task is complete.",
}

// schemaDepthCap bounds schemaPropertyKeys recursion like the normalizer's
// maxSchemaDepth: offered schemas arrive in client request bodies.
const schemaDepthCap = 12

// schemaPropertyKeys returns the top-level property names of a
// JSON-schema-shaped object, following anyOf/oneOf/allOf branches like
// upstream schemaPropertyKeys. Empty when the value is not an object
// schema at all.
func schemaPropertyKeys(params map[string]any) map[string]bool {
	out := map[string]bool{}
	collectSchemaKeys(params, 0, out)
	return out
}

func collectSchemaKeys(node any, depth int, out map[string]bool) {
	if depth > schemaDepthCap {
		return
	}
	m, ok := node.(map[string]any)
	if !ok || m == nil {
		return
	}
	if props, ok := m["properties"].(map[string]any); ok {
		for k := range props {
			out[k] = true
		}
	}
	for _, comb := range []string{"anyOf", "oneOf", "allOf"} {
		branches, ok := m[comb].([]any)
		if !ok {
			continue
		}
		for _, b := range branches {
			collectSchemaKeys(b, depth+1, out)
		}
	}
}

// IsGenuineSignatureTool mirrors upstream isGenuineSignatureTool: whether
// an offered tool is ours in substance, not only in name. Custom tools
// count by name; parameterised tools count when carrying a non-empty
// schema whose top-level names are a subset of ours (so a client one
// release behind an added optional field still clears); zero-parameter
// and unknown tools never count.
func IsGenuineSignatureTool(name string, params map[string]any) bool {
	if CustomSignatureToolNames[name] {
		return true
	}
	ours, ok := canonicalToolParameterKeys[name]
	if !ok || len(ours) == 0 {
		return false
	}
	theirs := schemaPropertyKeys(params)
	if len(theirs) == 0 {
		return false
	}
	for k := range theirs {
		if !ours[k] {
			return false
		}
	}
	return true
}

// IsHollowSignatureTool mirrors upstream isHollowSignatureTool: whether an
// offered tool wears one of our signature names without being ours — the
// laundering shape. For parameterised tools this is simply "not genuine";
// for zero-parameter tools the description lead is compared instead (the
// one field the hollow definitions proxies inject get wrong). Log-only
// upstream; diagnostic-only here.
func IsHollowSignatureTool(name string, params map[string]any, description string) bool {
	if CustomSignatureToolNames[name] {
		return false
	}
	if _, ok := canonicalToolParameterKeys[name]; ok {
		return !IsGenuineSignatureTool(name, params)
	}
	if ZeroParamSignatureTools[name] {
		lead := shippedZeroParamLead[name]
		return lead == "" || !strings.HasPrefix(strings.TrimSpace(description), lead)
	}
	return false
}

// offeredToolParts reads one wire tool entry in OpenAI function-tool
// shape, mirroring upstream readOfferedTools (name plus parameters plus
// description). Non-map entries and entries without a string name are
// skipped.
func offeredToolParts(t any) (name, description string, params map[string]any, ok bool) {
	m, ok := t.(map[string]any)
	if !ok {
		return "", "", nil, false
	}
	fn, ok := m["function"].(map[string]any)
	if !ok {
		return "", "", nil, false
	}
	name, _ = fn["name"].(string)
	if name == "" {
		return "", "", nil, false
	}
	description, _ = fn["description"].(string)
	params, _ = fn["parameters"].(map[string]any)
	return name, description, params, true
}

// isUnrecognisedToolName mirrors upstream isUnrecognisedToolName: neither
// ours, nor MCP-namespaced (server__tool), nor on the harness list.
// Observe-only upstream; local agent ids land here too, which is why
// nothing enforces on it.
func isUnrecognisedToolName(name string) bool {
	return !upstreamToolNames[name] &&
		!CustomSignatureToolNames[name] &&
		!ForeignHarnessToolNames[name] &&
		!strings.Contains(name, "__")
}

// ListUnrecognisedToolNames mirrors upstream listUnrecognisedToolNames: a
// bounded sample of offered names that are neither ours, MCP-namespaced,
// nor on the harness list.
func ListUnrecognisedToolNames(tools []any) []string {
	var out []string
	for _, t := range tools {
		name, _, _, ok := offeredToolParts(t)
		if !ok {
			continue
		}
		if isUnrecognisedToolName(name) {
			out = append(out, name)
			if len(out) == 8 {
				break
			}
		}
	}
	return out
}

// WireToolVerdict is the tool-leg of an upstream foreign-client verdict:
// the evidence fields (toolCount via len(Names)) without the sampling or
// system-prompt arms, which need the full request body.
type WireToolVerdict struct {
	Names          []string
	Genuine        []string
	Hollow         []string
	Unrecognised   []string
	ForeignHarness []string
	Foreign        []string // alias for ForeignHarness
}

// ClassifyWireTools classifies one wire tools array the way upstream's
// detector reads it: genuine vs hollow per offered definition, harness
// names verbatim, unrecognised as the bounded observe-only sample.
func ClassifyWireTools(tools []any) WireToolVerdict {
	var v WireToolVerdict
	for _, t := range tools {
		name, desc, params, ok := offeredToolParts(t)
		if !ok {
			continue
		}
		v.Names = append(v.Names, name)
		if ForeignHarnessToolNames[name] {
			v.ForeignHarness = append(v.ForeignHarness, name)
			v.Foreign = append(v.Foreign, name)
		}
		if IsGenuineSignatureTool(name, params) {
			v.Genuine = append(v.Genuine, name)
		} else if IsHollowSignatureTool(name, params, desc) {
			v.Hollow = append(v.Hollow, name)
		}
	}
	v.Unrecognised = ListUnrecognisedToolNames(tools)
	return v
}

// WireForeignSignal maps a tool-leg verdict onto the enforced signal
// upstream would raise, in detector order: a harness name settles it
// before the signature is consulted, otherwise tools with no genuine
// member read foreign_toolset. Empty when the tool leg clears. (The
// system-prompt and no-tools arms need the full body and live here as
// documentation, not code.)
func WireForeignSignal(v WireToolVerdict) ForeignSignal {
	if len(v.ForeignHarness) > 0 || len(v.Foreign) > 0 {
		return ForeignToolNames
	}
	if len(v.Names) > 0 && len(v.Genuine) == 0 {
		return ForeignToolset
	}
	return ""
}

// IsNowRecognisedToolset mirrors upstream isNowRecognisedToolset (vendor
// 40c75256): whether a toolset an older build flagged foreign_toolset would
// clear on the new one, judged from stored names alone. Names are all a
// ban_event row keeps, so only a custom tool can vouch for a row (face
// value); a harness name still convicts. Pass the full toolset.
// Diagnostic-only here: the proxy keeps no ban_event rows and never
// enforces; it exists so a stored observe row can be re-judged the way the
// new detector judges it.
func IsNowRecognisedToolset(names []string) bool {
	hasCustom := false
	for _, n := range names {
		if CustomSignatureToolNames[n] {
			hasCustom = true
			break
		}
	}
	if !hasCustom {
		return false
	}
	for _, n := range names {
		if ForeignHarnessToolNames[n] {
			return false
		}
	}
	return true
}
