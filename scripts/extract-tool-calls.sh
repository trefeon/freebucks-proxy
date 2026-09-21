#!/usr/bin/env bash
#
# scripts/extract-tool-calls.sh
#
# Generates backend/internal/convert/testdata/tool_calls_corpus.json: every
# MODEL-FACING tool name each agent CLI / harness in the local (gitignored,
# CI-absent) reference/ corpus registers with its model, plus the registry
# file:line that declares it.
#
# Why per-harness rules instead of one universal regex: the 23 corpora declare
# tool names in wholly different shapes (TS `export const name = "x"`, Rust
# `impl ToolSpec { fn name(&self) -> &'static str { "x" } }`, Go
# `const XToolName = "x"` / `func (t) Name() string { return "x" }`, registry
# insert calls, Python `name: str = "x"`, …). A single regex across all of them
# matches prose, JSON-schema property names, parameter names and test
# placeholders, which is exactly what the generated fixture must never
# contain. Each rule below was written against the registry source it reads,
# and every rule's glob is narrow enough that its ERE cannot drift into prose.
#
# Requires GNU awk (gawk), for match(string, ere, capture): the tool name is
# the ERE's single capture group, so one awk pass per rule extracts names with
# no per-match subprocess. On Cygwin/Windows a process per match is the
# difference between seconds and minutes, and `sub()`/`gensub()` group capture
# is gawk-only in the first place.
#
# Usage:
#   scripts/extract-tool-calls.sh
#   FREEBUFF_REFERENCE_DIR=/path/to/reference scripts/extract-tool-calls.sh
#
# Re-runnable: names are deduped per harness and sorted (harness, name), so
# running it twice produces a byte-identical fixture.
#
# A harness that yields ZERO names is an extractor failure, never a silent
# empty entry: repos with no model-facing registry are listed in the SKIP
# table with the reason and reported on stderr, and any other repo that yields
# nothing fails the run.
set -euo pipefail

# Byte-order collation everywhere: the fixture's per-harness name order must be
# locale-independent (and identical on re-runs), and the Go test compares names
# with Go's bytewise string ordering.
export LC_ALL=C

if ! awk --version 2>/dev/null | head -1 | grep -q 'GNU Awk'; then
  echo "extract-tool-calls: GNU awk (gawk) is required for ERE capture-group extraction." >&2
  echo "  found: $(awk --version 2>&1 | head -1)" >&2
  exit 1
fi

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
out_file="$repo_root/backend/internal/convert/testdata/tool_calls_corpus.json"

# ---------------------------------------------------------------------------
# Locate the reference corpus.
#
# It lives BESIDE the main checkout (D:/github_repo/freebuff-proxy/reference),
# not beside this worktree, so candidates are tried in order and the run fails
# loudly (listing everything tried) when none of them holds agents/ and
# harnesses/.
# ---------------------------------------------------------------------------
candidate_env=${FREEBUFF_REFERENCE_DIR:-}
candidate_sibling="$repo_root/reference"
candidate_parent="$(dirname -- "$repo_root")/reference"
candidate_checkout="D:/github_repo/freebuff-proxy/reference"

ref_dir=""
for candidate in "$candidate_env" "$candidate_sibling" "$candidate_parent" "$candidate_checkout"; do
  [ -n "$candidate" ] || continue
  if [ -d "$candidate/agents" ] && [ -d "$candidate/harnesses" ]; then
    ref_dir=${candidate%/}
    break
  fi
done

if [ -z "$ref_dir" ]; then
  {
    echo "extract-tool-calls: reference corpus not found (need a directory holding agents/ and harnesses/)."
    echo "  tried:"
    for candidate in "$candidate_env" "$candidate_sibling" "$candidate_parent" "$candidate_checkout"; do
      [ -n "$candidate" ] || continue
      echo "    $candidate"
    done
    echo "  set FREEBUFF_REFERENCE_DIR to the corpus root and re-run."
  } >&2
  exit 1
fi

tmplist=$(mktemp)
rawdata=$(mktemp)
sorted=$(mktemp)
tmp_out=$(mktemp)
trap 'rm -f "$tmplist" "$rawdata" "$sorted" "$tmp_out"' EXIT

shopt -s globstar nullglob

declare -A REPO=()     # harness -> repo path relative to the corpus root
declare -A NOTES=()    # harness -> optional one-liner for the fixture
declare -A SKIP=()     # repo path -> reason it can never produce names

# ---------------------------------------------------------------------------
# emit HARNESS NAME SOURCE
#
# Adds one name. The guards live here, not at rule level, so every rule gets
# the same filters:
#   - must look like a tool identifier (letters/digits/_.:-), never prose;
#   - no placeholder/example names, which only come from READMEs, test modules
#     and fixture blobs;
#   - the source path is de-quoted with pure bash (no subprocess per match).
# ---------------------------------------------------------------------------
emit() {
  local harness=$1 name=$2 source=$3
  name=${name#"${name%%[![:space:]]*}"}
  name=${name%"${name##*[![:space:]]}"}
  [ -n "$name" ] || return 0
  [ "${#name}" -le 64 ] || return 0
  case $name in
    *[!A-Za-z0-9_.:-]*) return 0 ;;   # spaces, quotes, prose
    [A-Za-z0-9]*) ;;                  # must start alphanumeric
    *) return 0 ;;
  esac
  case ${name,,} in
    foo|bar|baz|test|dummy|demo|example|calculator|test_tool|my_tool|example_tool) return 0 ;;
  esac
  source=${source//\"/}
  source=${source//\\/}
  printf '%s\t%s\t%s\n' "$harness" "$name" "$source" >>"$rawdata"
}

# ---------------------------------------------------------------------------
# exclude_path REL
#
# Never read generated, vendored, documentation or test sources: those hold
# prose, schema property names and placeholder fixtures rather than the model
# registry. `*_tests.rs` matters specifically for codex, whose test modules
# re-declare the whole name set.
# ---------------------------------------------------------------------------
exclude_path() {
  case $1 in
    */node_modules/*|node_modules/*) return 0 ;;
    */dist/*|dist/*|*/build/*|build/*|*/out/*|out/*) return 0 ;;
    */vendor/*|vendor/*|*/target/*|target/*) return 0 ;;
    */.git/*|.git/*) return 0 ;;
    *.md|*.mdx|*.md.tpl|*.markdown|*.snap) return 0 ;;
    *_test.go|*_test.rs|*_test.py|*_tests.rs|*_tests.go) return 0 ;;
    */test.py|test.py) return 0 ;;
    */test_sync.rs|test_sync.rs) return 0 ;;   # codex's internal test-sync helper tool
    *.test.ts|*.test.js|*.spec.ts|*.spec.js) return 0 ;;
    */test/*|test/*|*/tests/*|tests/*|*/__tests__/*|__tests__/*) return 0 ;;
    */testdata/*|testdata/*) return 0 ;;
    */examples/*|examples/*|*/example/*|*/snapshots/*|*/__snapshots__/*) return 0 ;;
  esac
  return 1
}

# ---------------------------------------------------------------------------
# rule HARNESS REPO MODE GLOB MATCH_ERE [NAME_ERE]
#
#   MODE        "line"   -> MATCH_ERE decides which lines declare a name, and
#                           NAME_ERE's capture group IS the name.
#               "nextN"  -> MATCH_ERE anchors a line and the name sits on one
#                           of the next N lines, where NAME_ERE's capture group
#                           is the name (required for this mode). Scanning a
#                           window instead of a fixed offset keeps a rule alive
#                           when the declaration wraps at a new depth.
#   GLOB        repo-relative glob (** allowed), expanded against the repo root.
#   MATCH_ERE   POSIX ERE (its own capture group is unused when NAME_ERE is
#               given explicitly).
#   NAME_ERE    Defaults to MATCH_ERE for "line" mode. EVERY NAME_ERE match on
#               the line emits a name, so a one-line array of tool names yields
#               all of them.
#
# One gawk pass per rule does the whole thing and prints "path:line:name", so a
# rule over 150 files still costs exactly one process. A NAME_ERE that does not
# match a target line yields nothing (no junk is ever emitted) — that is the
# mis-anchoring guard.
# ---------------------------------------------------------------------------
rule() {
  local harness=$1 repo=$2 mode=$3 glob=$4 match_ere=$5
  local name_ere=${6:-}
  local dir="$ref_dir/$repo" rec file rest n name delta t0
  REPO[$harness]=$repo

  case $mode in
    line) delta=0 ;;
    next[0-9]*) delta=${mode#next} ;;
    *)
      echo "extract-tool-calls: unknown mode $mode (harness $harness)" >&2
      exit 1
      ;;
  esac

  if [ -z "$name_ere" ]; then
    if [ "$delta" -ne 0 ]; then
      echo "extract-tool-calls: $harness: mode $mode needs an explicit NAME_ERE" >&2
      exit 1
    fi
    name_ere=$match_ere
  fi

  [ -d "$dir" ] || {
    echo "extract-tool-calls: $harness: repo directory missing: $repo" >&2
    return 0
  }

  local -a files=()
  t0=$SECONDS
  (cd "$dir" && for rel in $glob; do printf '%s\n' "$rel"; done) >"$tmplist"
  while IFS= read -r rel; do
    [ -n "$rel" ] || continue
    exclude_path "$rel" && continue
    files+=("$rel")
  done <"$tmplist"
  if [ "${#files[@]}" -eq 0 ]; then
    echo "extract-tool-calls: $harness: glob matched no files: $glob" >&2
    return 0
  fi

  # One awk pass scans the rule's files and prints "path:line:name". The EREs
  # travel through the environment because `awk -v` would eat the backslashes
  # POSIX EREs need (\(, \{) and re-escaping them in every rule is noise.
  while IFS= read -r rec; do
    [ -n "$rec" ] || continue
    file=${rec%%:*}
    rest=${rec#*:}
    n=${rest%%:*}
    name=${rest#*:}
    if [ -n "$name" ]; then
      emit "$harness" "$name" "$file:$n"
    fi
  done < <(
    (cd "$dir" && MATCH_ERE="$match_ere" NAME_ERE="$name_ere" DELTA="$delta" awk '
      # names(line, ln) prints capture group 1 of every ERE match on the line:
      # a registry line may declare several names (arrays, multi-tool specs).
      function names(line, ln,    rest, step) {
        rest = line
        while (match(rest, nre, m)) {
          if (length(m[1]) > 0) print file ":" ln ":" m[1]
          step = RSTART + RLENGTH
          if (RLENGTH <= 0 || step > length(rest)) break
          rest = substr(rest, step)
        }
      }
      BEGIN { mre = ENVIRON["MATCH_ERE"]; nre = ENVIRON["NAME_ERE"]; d = ENVIRON["DELTA"] + 0 }
      FNR == 1 { file = FILENAME; pending = 0 }
      {
        # nextN mode: the name sits on one of the next N lines, so scan them
        # and emit from the first that matches (registry declarations wrap at
        # different depths: generics, chained args, …).
        if (pending > 0) {
          if (match($0, nre, m)) { names($0, FNR); pending = 0 }
          else pending--
          next
        }
        if (!match($0, mre, m)) next
        if (d == 0) names($0, FNR)
        else pending = d
      }
    ' "${files[@]}") 2>/dev/null || true
  )
  echo "extract-tool-calls: $harness $glob (${#files[@]} files, $((SECONDS - t0))s)" >&2
}

# ===========================================================================
# Registry rules, one block per harness. Each comment names the registry
# source the rule reads; harnesses with no model-facing registry live in the
# SKIP table further down instead.
# ===========================================================================

# --- agents/opencode -------------------------------------------------------
# packages/core/src/tool/<tool>.ts: each model-facing built-in declares its
# wire name as `export const name = "<name>"`; builtins.ts composes them.
rule opencode agents/opencode line \
  'packages/core/src/tool/*.ts' \
  'export const name = "([^"]+)"'

# --- harnesses/pi ----------------------------------------------------------
# packages/coding-agent/src/core/tools/<tool>.ts: each tool returns
# `{ name: "<name>", label: "<name>", … }`.
rule pi harnesses/pi line \
  'packages/coding-agent/src/core/tools/*.ts' \
  '^[[:space:]]*name: "([A-Za-z0-9_-]+)",$'

# --- agents/crush ----------------------------------------------------------
# internal/agent/tools/<tool>.go: every built-in passes its `<X>ToolName` const
# to fantasy.NewAgentTool (the Go struct name is NOT the wire name).
rule crush agents/crush line \
  'internal/agent/tools/*.go' \
  'ToolName[[:space:]]*=[[:space:]]*"([a-z_]+)"'
# …and the one tool registered outside the tools package.
rule crush agents/crush line \
  'internal/agent/agent_tool.go' \
  'ToolName[[:space:]]*=[[:space:]]*"([a-z_]+)"'

# --- agents/DeepSeek-Reasonix ---------------------------------------------
# internal/tool/builtin/<tool>.go: every builtin self-registers in init() via
# tool.RegisterBuiltin, which keys on `func (t) Name() string { return "x" }`.
rule DeepSeek-Reasonix agents/DeepSeek-Reasonix line \
  'internal/tool/builtin/*.go' \
  'func \([^)]+\) Name\(\) string \{ return "([a-z_0-9]+)" \}'
# Secondary registries assembled by reg.Add(...) in internal/boot/boot.go —
# the same Name() shape in other packages.
for reasonix_dir in \
  'internal/agent/*.go' \
  'internal/tool/sessiontool/*.go' \
  'internal/memory/*.go' \
  'internal/skill/tools.go' \
  'internal/history/tool.go' \
  'internal/command/slashtool.go' \
  'internal/productdocs/docs.go' \
  'internal/installsource/install_source.go'; do
  rule DeepSeek-Reasonix agents/DeepSeek-Reasonix line \
    "$reasonix_dir" \
    'func \([^)]+\) Name\(\) string \{ return "([a-z_0-9]+)" \}'
done

# --- agents/jcode ----------------------------------------------------------
# crates/jcode-app-core/src/tool/mod.rs: Registry::base_tools inserts every
# model-facing tool under its registry key.
rule jcode agents/jcode line \
  'crates/jcode-app-core/src/tool/mod.rs' \
  'insert_tool_timed\(&mut m, &mut timings, "([^"]+)"'
# …and the same call wrapped across lines (registry key three lines below).
rule jcode agents/jcode next3 \
  'crates/jcode-app-core/src/tool/mod.rs' \
  '^[[:space:]]*Self::insert_tool_timed\($' \
  '^[[:space:]]*"([^"]+)",$'

# --- agents/Codewhale ------------------------------------------------------
# crates/tui/src/tools/*.rs: tools with a literal name implement ToolSpec as
# `fn name(&self) -> &'static str { "<name>" }`.
rule Codewhale agents/Codewhale next1 \
  'crates/tui/src/tools/*.rs' \
  '^[[:space:]]*fn name\(&self\) -> &.static str \{$' \
  '^[[:space:]]*"([^"]+)"[[:space:]]*$'
# crates/tui/src/tools/registry.rs: the shipped registry registers each tool by
# name (canonical name plus the compatibility aliases the harness also knows).
rule Codewhale agents/Codewhale line \
  'crates/tui/src/tools/registry.rs' \
  'Arc::new\([A-Za-z0-9_:]+::[a-z_]+\("([^"]+)"'
# aliases whose arguments wrap onto the next line.
rule Codewhale agents/Codewhale next1 \
  'crates/tui/src/tools/registry.rs' \
  'Arc::new\([A-Za-z0-9_:]+::alias\($' \
  '^[[:space:]]*"([^"]+)",$'

# --- agents/codex ----------------------------------------------------------
# codex-rs/core/src/tools/handlers/**: the runtime tool registry; each handler
# returns ToolName::plain("x") / ToolName::namespaced(NS, "x"). The leading
# character class keeps the match off prefixed wrappers (HookToolName::new,
# codex_tools::ToolName::…) whose literals are hook/test fixtures.
rule codex agents/codex line \
  'codex-rs/core/src/tools/handlers/**/*.rs' \
  '[^A-Za-z_:]ToolName::[A-Za-z]+\([^)]*"([a-z_0-9]+)"\)'
# …plus the const-indirected names in the same tree (`…TOOL_NAME: &str = "x"`,
# i.e. the const name ENDS in TOOL_NAME — _PREFIX/_DESCRIPTION consts are not
# tool names).
rule codex agents/codex line \
  'codex-rs/core/src/tools/handlers/**/*.rs' \
  'const [A-Z_]*TOOL_NAME: &str = "([a-z_0-9]+)"'
# hosted tool names (ToolSpec enum → wire name).
rule codex agents/codex line \
  'codex-rs/tools/src/tool_spec.rs' \
  '\{ \.\. \} => "([a-z_0-9]+)",'
# discovery / plugin-install tool consts.
rule codex agents/codex line \
  'codex-rs/tools/src/tool_discovery.rs' \
  'const [A-Z_]*TOOL_NAME: &str = "([a-z_0-9]+)"'
# extension-crate tools (memories, skills, web search, goals, image gen).
rule codex agents/codex line \
  'codex-rs/ext/*/src/**/*.rs' \
  'const [A-Z_]*TOOL_NAME: &str = "([a-z_0-9]+)"'
# code-mode protocol tool names.
rule codex agents/codex line \
  'codex-rs/code-mode-protocol/src/lib.rs' \
  'pub const [A-Z_]*TOOL_NAME: &str = "([a-z_0-9]+)"'

# --- agents/cline ----------------------------------------------------------
# sdk/packages/core/src/extensions/tools/constants.ts: DefaultToolNames, the
# registry the Cline SDK/CLI path drives.
rule cline agents/cline line \
  'sdk/packages/core/src/extensions/tools/constants.ts' \
  '^[[:space:]]*[A-Z_]+: "([a-z_]+)",$'
# team-tools.ts: TEAM_TOOL_NAMES, the same directory's team surface (one tab
# of indentation: deeper quoted commas are call arguments, not tool names).
rule cline agents/cline line \
  'sdk/packages/core/src/extensions/tools/team/team-tools.ts' \
  '^\t"([a-z_]+)",$'
# provider-executed model tools.
rule cline agents/cline line \
  'sdk/packages/shared/src/llms/model-tools.ts' \
  '_TOOL_NAMES = \["([a-z_]+)"'
# apps/vscode/src/shared/tools.ts: the legacy ClineDefaultTool enum, still the
# extension's subagent-config vocabulary.
rule cline agents/cline line \
  'apps/vscode/src/shared/tools.ts' \
  '^[[:space:]]*[A-Z_]+ = "([a-z_]+)",$'

# --- agents/continue -------------------------------------------------------
# core/tools/builtIn.ts: the BuiltInToolNames enum holds every wire name; the
# per-tool definition modules reference the enum, they never repeat the string.
rule continue agents/continue line \
  'core/tools/builtIn.ts' \
  '^[[:space:]]*[A-Za-z]+ = "([a-z_]+)"'

# --- agents/gemini-cli -----------------------------------------------------
# packages/core/src/tools/definitions/base-declarations.ts declares each tool's
# wire name as `<X>_TOOL_NAME = '…'`, and tool-names.ts holds the registry list
# ALL_BUILTIN_TOOL_NAMES over the same constants.
rule gemini-cli agents/gemini-cli line \
  'packages/core/src/tools/definitions/base-declarations.ts' \
  'export const [A-Za-z0-9_]*TOOL_NAME = '"'"'([a-z0-9_]+)'"'"''
rule gemini-cli agents/gemini-cli line \
  'packages/core/src/tools/tool-names.ts' \
  'export const [A-Za-z0-9_]*TOOL_NAME = '"'"'([a-z0-9_]+)'"'"''

# --- agents/qwen-code ------------------------------------------------------
# packages/core/src/tools/tool-names.ts: the ToolNames map; config.ts registers
# every tool by ToolNames.X and each class binds static readonly Name.
rule qwen-code agents/qwen-code line \
  'packages/core/src/tools/tool-names.ts' \
  '^[[:space:]]*[A-Z_]+: '"'"'([a-z0-9_]+)'"'"',$'

# --- agents/Roo-Code -------------------------------------------------------
# packages/types/src/tool.ts: the canonical `toolNames` registry.
rule Roo-Code agents/Roo-Code line \
  'packages/types/src/tool.ts' \
  '^\t"([a-z_]+)",$'
# src/core/prompts/tools/native-tools/*.ts: the model-facing OpenAI function
# names actually sent (getNativeTools assembles them).
rule Roo-Code agents/Roo-Code line \
  'src/core/prompts/tools/native-tools/*.ts' \
  '^[[:space:]]+name: "([a-z_]+)",$'
# src/shared/tools.ts TOOL_ALIASES is deliberately NOT extracted: those are
# names a MODEL may emit, not entries Roo offers in tools[]. Offering the alias
# alongside the canonical name would synthesize a request no harness ever sends.

# --- agents/kilocode -------------------------------------------------------
# packages/opencode/src/tool/<tool>.ts + kilocode/tool/<tool>.ts: every tool is
# registered as Tool.define("<id>", …); the id sits within the next few lines
# (Tool.define<…>(…) puts it after the type parameters).
rule kilocode agents/kilocode next6 \
  'packages/opencode/src/tool/*.ts' \
  'Tool\.define[<(]' \
  '^[[:space:]]*"([^"]+)",$'
rule kilocode agents/kilocode next6 \
  'packages/opencode/src/kilocode/tool/*.ts' \
  'Tool\.define[<(]' \
  '^[[:space:]]*"([^"]+)",$'
rule kilocode agents/kilocode next6 \
  'packages/opencode/src/kilocode/suggestion/tool.ts' \
  'Tool\.define[<(]' \
  '^[[:space:]]*"([^"]+)",$'
# single-line Tool.define("id", …) form.
rule kilocode agents/kilocode line \
  'packages/opencode/src/tool/*.ts' \
  'Tool\.define\("([^"]+)"'
# the three ids that are constants rather than define() literals.
rule kilocode agents/kilocode line \
  'packages/opencode/src/tool/shell/id.ts' \
  '^export const ToolID = "([^"]+)"'
rule kilocode agents/kilocode line \
  'packages/opencode/src/tool/code-mode.ts' \
  '^export const CODE_MODE_TOOL = "([^"]+)"'
rule kilocode agents/kilocode line \
  'packages/opencode/src/tool/task.ts' \
  '^const id = "([^"]+)"'

# --- agents/kimi-cli -------------------------------------------------------
# src/kimi_cli/tools/**/*.py: each tool class declares `name: str = "…"`, or
# `name: str = NAME` over a module-level `NAME = "…"` constant.
rule kimi-cli agents/kimi-cli line \
  'src/kimi_cli/tools/**/*.py' \
  '^[[:space:]]+name: str = "([^"]+)"'
rule kimi-cli agents/kimi-cli line \
  'src/kimi_cli/tools/**/*.py' \
  '^NAME = "([^"]+)"'

# --- agents/goose ----------------------------------------------------------
# crates/goose/src/agents/platform_extensions/**: each platform extension
# builds its tools with Tool::new("<name>") / McpTool::new("<name>"); the
# literal already carries the extension prefix where one is used.
rule goose agents/goose line \
  'crates/goose/src/agents/platform_extensions/**/*.rs' \
  'Tool::new\("([^"]+)"'
rule goose agents/goose next1 \
  'crates/goose/src/agents/platform_extensions/**/*.rs' \
  'Tool::new\($' \
  '^[[:space:]]*"([^"]+)",$'
rule goose agents/goose line \
  'crates/goose/src/agents/platform_extensions/**/*.rs' \
  'TOOL_NAME_COMPLETE: &str = "([^"]+)"'
# the same shape for every tool registered outside the extensions tree.
rule goose agents/goose line \
  'crates/goose/src/skills/client.rs' \
  'Tool::new\("([^"]+)"'
rule goose agents/goose next1 \
  'crates/goose/src/skills/client.rs' \
  'Tool::new\($' \
  '^[[:space:]]*"([^"]+)",$'
rule goose agents/goose line \
  'crates/goose/src/agents/final_output_tool.rs' \
  'Tool::new\("([^"]+)"'
rule goose agents/goose next1 \
  'crates/goose/src/agents/final_output_tool.rs' \
  'Tool::new\($' \
  '^[[:space:]]*"([^"]+)",$'
# bundled goose-mcp extensions register through the #[tool] macro.
rule goose agents/goose line \
  'crates/goose-mcp/src/**/*.rs' \
  '#\[tool\(name = "([^"]+)"'
rule goose agents/goose next1 \
  'crates/goose-mcp/src/**/*.rs' \
  '^[[:space:]]*#\[tool\($' \
  '^[[:space:]]*name = "([^"]+)",'

# --- agents/hermes-agent ---------------------------------------------------
# tools/*.py: every tool module self-registers at import time with
# registry.register(name="<name>", toolset="…"); toolsets.py only SELECTS
# among those names. Table-driven modules that register inside a loop are
# reported in the fixture notes instead (they never repeat the literal).
rule hermes-agent agents/hermes-agent line \
  'tools/*.py' \
  'registry\.register\(name="([^"]+)"'
# …and the same call with the name on the following line.
rule hermes-agent agents/hermes-agent next2 \
  'tools/*.py' \
  'registry\.register\($' \
  '^[[:space:]]*name="([^"]+)"'
rule hermes-agent agents/hermes-agent line \
  'tools/*/tool.py' \
  'registry\.register\(name="([^"]+)"'

# --- harnesses/oh-my-pi ----------------------------------------------------
# packages/coding-agent/src/tools/builtin-names.ts: BUILTIN_TOOL_NAMES is the
# canonical model-facing set (HIDDEN_TOOL_NAMES on one line is noted instead).
rule oh-my-pi harnesses/oh-my-pi line \
  'packages/coding-agent/src/tools/builtin-names.ts' \
  '^\t"([a-z_]+)",$'

# --- harnesses/openclaw ----------------------------------------------------
# src/agents/tools/*.ts: every tool module declares its wire name in the
# createTool spec object it hands the runtime.
rule openclaw harnesses/openclaw line \
  'src/agents/tools/*.ts' \
  '^[[:space:]]*name: "([a-z_0-9]+)",$'

# --- harnesses/SWE-agent ---------------------------------------------------
# tools/<bundle>/config.yaml: the bundle registry maps each tool name to its
# signature ("goto", "open", "str_replace_editor", …); Bundle commands are
# built from exactly these keys.
rule SWE-agent harnesses/SWE-agent line \
  'tools/*/config.yaml' \
  '^  ([a-z_0-9]+):$'

# --- harnesses/OpenHands ---------------------------------------------------
# This checkout is the OpenHands client/desktop app: the model-facing wire
# names it owns are the agent-server tool selection and the client tools it
# injects. (The agent-server's own tool implementations live outside this
# checkout, so the corpus only carries the names declared here.)
rule OpenHands harnesses/OpenHands line \
  'src/api/agent-server-adapter.ts' \
  '^const DEFAULT_TOOL_NAMES = \[' \
  '"([a-z_0-9]+)"[],]'
rule OpenHands harnesses/OpenHands line \
  'src/constants/canvas-ui.ts' \
  '^export const [A-Z_]*TOOL_NAME = "([a-z_0-9]+)"'
rule OpenHands harnesses/OpenHands line \
  'src/constants/child-conversation.ts' \
  '^export const [A-Z_]*TOOL_NAME = "([a-z_0-9]+)"'
rule OpenHands harnesses/OpenHands line \
  'src/utils/plan-file.ts' \
  '^export const [A-Z_]*TOOL_NAME = "([a-z_0-9]+)"'

# --- agents/plandex --------------------------------------------------------
# app/server/model/prompts/*.go: each model-callable function is an
# openai.FunctionDefinition whose Name is the wire name the planner sends.
rule plandex agents/plandex line \
  'app/server/model/prompts/*.go' \
  '^[[:space:]]+Name: "([A-Za-z_0-9]+)",$'

# ===========================================================================
# Coverage notes carried into the fixture, so a reader knows what each rule
# does and does not capture. Plain text only (the emitter does not escape
# JSON in notes).
# ===========================================================================
NOTES[opencode]='packages/core/src/tool export-const-name declarations'
NOTES[pi]='core/tools name/label pairs'
NOTES[crush]='ToolName consts the constructors pass to fantasy.NewAgentTool (the Go struct names differ)'
NOTES[Codewhale]='ToolSpec literal names plus the shipped-registry names; the registry also registers PascalCase compatibility aliases that model_visible() hides at runtime'
NOTES[codex]='handler registry, const, hosted, discovery and extension names; cfg(test) modules and the internal test-sync helper are excluded'
NOTES[cline]='Cline SDK registry plus the legacy VS Code ClineDefaultTool enum (both are live vocabularies)'
NOTES[continue]='BuiltInToolNames enum values'
NOTES[DeepSeek-Reasonix]='builtin Name() methods plus the secondary reg.Add registries in boot.go'
NOTES[jcode]='Registry::base_tools registry keys'
NOTES[goose]='platform-extension Tool::new literals and bundled goose-mcp macro names; the runtime prefixes bundled-extension tools when it advertises them'
NOTES[hermes-agent]='registry.register name literals; table-driven modules (browser_, kanban_, feishu, homeassistant, yuanbao) register names from in-file tables and are not extracted'
NOTES[kilocode]='Tool.define ids plus the three constant ids; several ids are client, model or flag gated at runtime'
NOTES[kimi-cli]='name: str attributes plus module-level NAME consts'
NOTES[oh-my-pi]='BUILTIN_TOOL_NAMES; HIDDEN_TOOL_NAMES (yield, goal, think) sit in a single-line array and are not extracted'
NOTES[openclaw]='tool spec name fields across src/agents/tools'
NOTES[OpenHands]='this checkout is the OpenHands client/desktop app: the agent-server tool selection plus the client tools it injects'
NOTES[plandex]='openai.FunctionDefinition names under app/server/model/prompts'
NOTES[qwen-code]='ToolNames map; save_memory is declared but has no registering tool class in this checkout'
NOTES[Roo-Code]='canonical toolNames plus the native-tools function names (TOOL_ALIASES keys are excluded: they are names a model may emit, never entries Roo offers in tools[])'
NOTES[SWE-agent]='union of the per-bundle tool registries under tools/<bundle>/config.yaml'

# ===========================================================================
# Corpus repos with no model-facing tool registry, or no readable source for
# one. They are reported as skipped on stderr and never emitted as empty
# harness entries.
# ===========================================================================
SKIP[agents/claude-code]='no readable tool registry in this checkout (plugin-marketplace repo: no src/, no committed bundle; every tool name here is prose in CHANGELOG.md/feed.xml/plugin markdown)'
SKIP[agents/aider]='no reachable function-calling registry: the only functions=[...] coders are deprecated/unreachable (base_coder.py:96 functions = None)'

# ---------------------------------------------------------------------------
# Emit the fixture: dedupe per harness, sort (harness, name), write JSON.
# ---------------------------------------------------------------------------
sort -t$'\t' -k1,1 -k2,2 -k3,3 "$rawdata" \
  | awk -F'\t' '!seen[$1 FS $2]++' >"$sorted"

if [ ! -s "$sorted" ]; then
  echo "extract-tool-calls: extracted no tool names at all from $ref_dir — every registry rule is broken" >&2
  exit 1
fi

{
  printf '{\n'
  printf '  "source": "reference/",\n'
  printf '  "generated_by": "scripts/extract-tool-calls.sh",\n'
  printf '  "harnesses": [\n'
  first=1
  while IFS= read -r harness; do
    [ -n "$harness" ] || continue
    if [ "$first" -eq 0 ]; then
      printf '    },\n'
    fi
    first=0
    printf '    {\n'
    printf '      "harness": "%s",\n' "$harness"
    printf '      "repo": "%s",\n' "${REPO[$harness]}"
    if [ -n "${NOTES[$harness]:-}" ]; then
      printf '      "notes": "%s",\n' "${NOTES[$harness]}"
    fi
    printf '      "tools": [\n'
    awk -F'\t' -v h="$harness" '
      $1 == h { n++; name[n] = $2; src[n] = $3 }
      END {
        for (i = 1; i <= n; i++)
          printf("        {\"name\": \"%s\", \"source\": \"%s\"}%s\n", name[i], src[i], (i < n ? "," : ""))
      }
    ' "$sorted"
    printf '      ]\n'
  done < <(cut -f1 "$sorted" | uniq)
  if [ "$first" -eq 0 ]; then
    printf '    }\n'
  fi
  printf '  ]\n'
  printf '}\n'
} >"$tmp_out"

# ---------------------------------------------------------------------------
# Every corpus repo must be either represented or explicitly skipped.
# ---------------------------------------------------------------------------
declare -A EMITTED=()
while IFS= read -r h; do
  [ -n "$h" ] && EMITTED[$h]=1
done < <(cut -f1 "$sorted" | uniq)

missing=0
for dir in "$ref_dir"/agents/*/ "$ref_dir"/harnesses/*/; do
  [ -d "$dir" ] || continue
  rel=${dir#"$ref_dir"/}
  rel=${rel%/}
  represented=0
  for h in "${!REPO[@]}"; do
    if [ "${REPO[$h]}" = "$rel" ] && [ -n "${EMITTED[$h]:-}" ]; then
      represented=1
    fi
  done
  if [ "$represented" -eq 1 ]; then
    continue
  fi
  if [ -n "${SKIP[$rel]:-}" ]; then
    echo "SKIPPED $rel: ${SKIP[$rel]}" >&2
    continue
  fi
  echo "extract-tool-calls: FAILED to extract any tool name from $rel — add a registry rule, or list it in the SKIP table with a reason" >&2
  missing=1
done

if [ "$missing" -ne 0 ]; then
  exit 1
fi

mkdir -p "$(dirname -- "$out_file")"
mv "$tmp_out" "$out_file"

harness_count=$(cut -f1 "$sorted" | uniq | wc -l | tr -d ' ')
name_count=$(wc -l <"$sorted" | tr -d ' ')
echo "extract-tool-calls: wrote $out_file ($harness_count harnesses, $name_count tool names)" >&2
