# Universal tool translation — reference audit + OpenAI/Anthropic design

Status: proposed · 2026-10-01 · this is the **Phase 0 deliverable** named in
[`UNIVERSAL-TOOLS-PLAN.md`](UNIVERSAL-TOOLS-PLAN.md) §3, extended with the
**surface** dimension the plan left implicit: the same translated toolset must
be produced and restored on *three* wire surfaces (OpenAI Chat Completions,
OpenAI Responses, Anthropic Messages), not just Chat.

It extends, and does not supersede, [`OMP-TRANSLATION.md`](OMP-TRANSLATION.md)
and [`decisions/tool-name-translation.md`](decisions/tool-name-translation.md).

## 1. Method

Sources, all local and gitignored except the generated fixture:

| Source | What it gives us | Regenerate / verify |
|---|---|---|
| `backend/internal/convert/testdata/tool_calls_corpus.json` | 21 harnesses, 637 registry rows, 481 distinct names, per-row `file:line` | `scripts/extract-tool-calls.sh` |
| `reference/**/WIRE-NOTES.md` (12 files) | per-harness protocol surface, auth, tool-def shape, proxy-hostile rules | hand-written, per repo |
| `reference/protocols/openai-openapi`, `reference/protocols/anthropic-sdk-typescript` | canonical wire grammars | upstream SDKs |
| `backend/internal/convert/toolmap_request.go`, `tools_reshape.go`, `tools_floor.go`, `tools_textfallback.go` | the live name map + family reshape rules | unit + corpus tests |

The name classification in §4 is measured against the live maps, not estimated.

## 2. Protocol matrix — which surface each client speaks

Three request surfaces exist and all three are already served by the proxy
(`/v1/chat/completions`, `/v1/responses`, `/v1/messages`; `README.md:3-6`).
A universal translation layer must therefore terminate on all three.

| Harness | Chat Completions | Responses | Anthropic Messages | Evidence |
|---|---|---|---|---|
| codex | ✗ (config-error) | **only** | ✗ | `reference/agents/codex/WIRE-NOTES.md:5,9-23` |
| opencode | ✓ (openai-compatible) | ✓ (openai) | ✓ (anthropic) | `opencode/WIRE-NOTES.md:36-59` |
| cline | ✓ (openai-compatible) | ✓ (openai, default) | ✓ (anthropic) | `cline/WIRE-NOTES.md:9-18` |
| Roo-Code | ✓ | ✓ | **default** | `Roo-Code/WIRE-NOTES.md:7-13` |
| continue | **default** | ✓ (o/gpt-5+ on `api.openai.com` only) | ✓ (separate provider) | `continue/WIRE-NOTES.md:8-15` |
| goose | ✓ (default + declarative) | ✓ (gpt-5*/o*) | ✓ (Claude models) | `goose/WIRE-NOTES.md:11-47` |
| kilocode | ✓ | ✓ | ✓ | `kilocode/WIRE-NOTES.md:16-40` |
| qwen-code | ✓ (default) | ✗ | ✓ (claude-code-compat) | `qwen-code/WIRE-NOTES.md:10-16,37-41` |
| aider | ✓ (via LiteLLM) | ✗ | ✓ (`anthropic/` prefix) | `aider/WIRE-NOTES.md:24-58` |
| pi | ✓ | ✓ | ✓ (per-model `api`) | `pi/WIRE-NOTES.md:5-6` |
| oh-my-pi (OMP) | ✓ | ✓ | ✓ | `oh-my-pi/WIRE-NOTES.md:5-8` |
| gemini-cli | ✗ | ✗ | ✗ (native Gemini REST) | `gemini-cli/WIRE-NOTES.md:3` |
| claude-code | ✗ | ✗ | ✓ | `docs/UNIVERSAL-CLIENTS.md:36` |
| kimi-cli | ? | ? | ? (PascalCase toolset ⇒ Claude-style) | corpus names only; **no WIRE-NOTES** |

Consequence: **the universal registry cannot be a chat-only abstraction.** The
Requestify/Restore/Reshape glue is surface-specific even when the rule table is
shared.

### Surface encodings (tool call leg)

| Surface | Tool definition | Tool call (server→client) | Tool result (client→server) |
|---|---|---|---|
| Chat | `{type:"function",function:{name,description,parameters}}` | `choices[].delta.tool_calls[]`/`message.tool_calls[]`, `function.arguments` = **JSON string** | `role:"tool"`,`tool_call_id` |
| Responses | `{type:"function",name,description,parameters,strict}` | `response.output_item.done` → `FunctionCall` item, `arguments` = **JSON string** | `function_call_output` item |
| Anthropic | `{name,description,input_schema}` | `content_block_start`→`tool_use`; `input_json_delta` partial JSON | `tool_result` block |

Sources: `opencode/WIRE-NOTES.md:255-289`, `codex/WIRE-NOTES.md:126-148`.

### Auth/headers the layer must tolerate per surface

- Chat/Responses: `Authorization: Bearer` (codex `WIRE-NOTES.md:39`; opencode
  `:132`). Codex custom providers may send **no** auth header (`codex:45`).
- Anthropic: `x-api-key` + `anthropic-version: 2023-06-01`
  (opencode `:140`). **Exception:** qwen-code in claude-code-compat mode sends
  `Authorization: Bearer` with `x-app: cli` instead of `x-api-key`
  (`qwen-code:37-41`) — the universal layer must not assume Anthropic means
  `x-api-key`.

## 3. Where the translation layer stands today

| Layer | Coverage | Evidence |
|---|---|---|
| Name map (request) | 103 entries → official signature names | `convert/toolmap_request.go:41-184` |
| Name restore (response) | all mapped + passthrough, exact casing, all three prefixes | `convert/toolmap_response.go`; `toolmap_corpus_test.go` |
| Wire-name legalization | universal (`mcp__`/hashed) | `toolmap_request.go:313-330` |
| Canonical def substitution | renamed entries only | `tools_normalize.go` |
| **Arg-shape reshape** | **OMP (floor) + pi only** | `tools_reshape.go`, `tools_floor.go`; `toolmap_request.go:250` |
| Floor-only substitution | OMP only | `tools_floor.go:floorOnlyOMP` |
| Unroutable-call text fallback | OMP + pi families | `tools_textfallback.go` |
| Response-leg gate | `family != none` | `toolmap_request.go:250` |

The asymmetry is exactly as the plan states: **names are universal; argument
shapes are family-scoped.** A non-OMP/pi client today is assumed to fill
arguments in the canonical shape — true only because its own schema rode the
wire unchanged. On the **Anthropic** surface that assumption is weaker still:
the client sends `input_schema` and the model fills `tool_use.input` per that
schema, so the reshape must read/write the Anthropic input object, not a chat
`arguments` string.

## 4. Name classification (measured 2026-10-01)

Across the 637 per-harness registry rows / 481 distinct names:

| Class | Rows | Meaning |
|---|---|---|
| mapped | 142 | has a `clientToOfficial` entry |
| official | 36 | already an official wire name (identity) |
| foreign-harness | ≥3 (regex undercount; set has 49) | virtualized to `mcp__*` |
| **unmapped** | ~456 rows / ~402 distinct | **Phase 0 work queue** |

The unmapped rows by harness (triage input, grounded in the fixture):

- **Family-private (keep passthrough; no official equivalent)** — the bulk.
  Spawners/subagents: `task`, `new_task`, `spawn_agent`, `new_context`,
  `delegate`, `parallel_tasks`, `create_sub_session`, `team_*` (cline ≈ 17
  names). Browser/desktop/computer-use: SWE-agent's whole
  `click_mouse`/`goto`/`screenshot_site` set, `browser_*` (hermes), `computer*`.
  Memory/recall: `memory`, `remember`, `recall`, `retain`, `kilo_memory_*`.
  Planning: `create_goal`, `update_plan`, `submit_plan`, `enter_plan_mode`.
  These correctly never map; the layer's only obligation is verbatim
  round-trip (already satisfied by identity restore).
- **Mappable (same intent, different name)** — candidates for new
  `clientToOfficial` rows: e.g. `run_command`/`run_commands` (cline,SWE-agent),
  `execute` (kilocode→`run_terminal_command`), `read_media`/`ReadMediaFile`,
  `list_code_definition_names` (→`code_search`?), `git_diff`/`git_status`
  (Codewhale — no official equivalent, likely passthrough), `view_diff`,
  `create`/`insert` (SWE-agent edit family), `move_file`, `delete_range`,
  `notebook_edit`, `glob`-equivalents (`file_glob_search`, `file_search`),
  `grep`-equivalents (`grep_search`, `semantic_search`, `codebase_search`).
- **UI-hook / unroutable (text-fallback class)** — `ask_*`,
  `ask_user_question`, `question`, `suggest`, `suggest_task`, `clarify`,
  `show_tip`, `render_*`, `display_image`, `structured_output`,
  `complete_task`/`attempt_completion`. On a non-OMP client these have no
  dispatcher; if they ride the wire they must be rendered as text on the
  response leg exactly as the OMP fallback already does.

**Fixture gap to close first:** `claude-code` and `aider` exist under
`reference/` but have **no corpus row** — the extractor found no registry for
them. `claude-code` is the flagship Anthropic client; its `Bash`/`Read`/`Edit`/
`Write`/`TodoWrite` PascalCase set only survives today via
`ForeignHarnessToolNames` virtualization. Adding a `claude-code` row is a
Phase-0 prerequisite for "Anthropic-compatible" to mean anything.

## 5. Argument-shape work per official wire tool

For each canonical/floor wire name, the client shapes that need reshape on the
response leg. Verified rows cite the client; `?` marks audit-pending (schema
diff against the client repo not yet done).

| Wire (official) | OMP shape | pi shape | Other families needing reshape |
|---|---|---|---|
| `read_files{paths[]}` | `read{path}` fan-out, offset/limit dropped | `read{path[,offset,limit]}` | claude-code `Read{file_path,offset,limit}` ? |
| `str_replace{replacements[]}` | `edit{old_string,new_string}` fan-out | `edit{edits[{oldText,newText}]}` batch | Roo/cline `apply_diff`/`search_replace` shape ?; SWE-agent `str_replace_editor` |
| `write_file{path,content}` | `write{path,content}` | `write{path,content}` | claude-code `Write{file_path,content}` ? |
| `run_terminal_command` | `bash{command}` | `bash{command,timeout}` | cline `execute_command{command,requires_approval}` ?; SWE-agent `str_replace_editor`/`bash` |
| `code_search` | `grep{pattern,path}` | `grep{pattern,path?}` | continue `grep_search`, kilocode `grep`, gemini `grep_search` |
| `glob` | origin-aware `glob`/`find` | `glob{pattern,path?}` | continue `file_glob_search`, codex `list` |
| `write_todos{todos[]}` | `todo` phase-form | — (pi has none → text) | Claude `TodoWrite` shape ? (Claude Code not yet in the corpus; official is `{task,completed}`); Anthropic `input` object |
| `apply_patch` | — | — | codex `apply_patch` (freeform `input` string per `codex:137`); cline `apply_patch` |
| `web_search`, `read_url`, `skill`, `list_directory`, `ask_user` | reshaped | text/reshaped | pi/others per OMP table |

Two structural consequences:

1. **Fidelity is per-family, not per-name.** `edit` means fan-out for OMP, one
   batch for pi, and a unified-diff for Roo (`apply_diff`) — a single
   `reshapeRules[wire]` keyed on the wire name cannot serve all three. The
   plan's `{wire, family, reshape}` table shape is required.
2. **The Anthropic leg reshapes an object, not a JSON string.** `tool_use.input`
   is already a parsed object; chat/Responses carry `arguments` as a JSON
   string. Reshape must run before the surface encoder in every case, then be
   re-serialized per surface (`arguments` string) or passed as `input`
   (Anthropic).

## 6. Design: one registry, three surfaces, four tables

Per `UNIVERSAL-TOOLS-PLAN.md` §2, with the surface dimension made explicit:

```
reference/ corpus ─► convert/registry.go
    names:   client → wire            (clientToOfficial, exists)
    reverse: wire   → client          (ToUpstream ownership, exists)
    args:    (family, wire, in) → out (generalize reshapeRules; OMP+pi exist)
    floor:   family → substitution?   (generalize floorOnlyOMP; OMP exists)
    route:   (family, wire) → routable (generalize piRoutable/text fallback)
```

Surface adapters (thin, one per wire) feed and drain the shared registry:

| Adapter | Request in | Response out |
|---|---|---|
| chat (`server/openai*.go`) | `tools[]`/`tool_choice` | `delta.tool_calls`/`message.tool_calls` (withhold+inject) |
| responses (`server/responses*.go`) | `tools[]`/`input` items | `function_call` items |
| anthropic (`server/anthropic*.go`) | `tools[]`/`input_schema` | `tool_use`/`input_json_delta` |

Family detection generalizes `detectFamilyBody` (OMP → pi → claude-code/codex/
generic) under the existing "never floor on doubt" rule
(`tools_floor.go:detectFamilyBody`).

Invariants (assert by sweep, per `tool-name-translation.md` §Invariants):
wire names unique + grammar-legal; no foreign schema rides; every wire name
restores to the exact client name on **all three** surfaces; no call is ever
dropped unless a text fallback renders it; a `tool_choice`/`tool` pin follows
the rename.

## 7. Phases

- [x] **Phase 0** — this audit. Remaining sub-tasks: add `claude-code` +
      `aider` corpus rows; finish the `?` rows in §5 with `file:line`.
- [ ] **Phase 1** — registry extraction (`convert/registry.go`), behavior locked
      by the unchanged corpus/reshape tests (plan §3 Phase 1).
- [ ] **Phase 2** — arg rules, one family per commit, RED→GREEN, ordered by
      corpus frequency (plan §3 Phase 2).
- [ ] **Phase 3** — family detection + the family-parameterized surface sweep.
- [ ] **Phase 4** — **surface parity**: port withhold/inject + fan-out to
      streaming Anthropic/Responses (today deferred — `tool-name-translation.md`
      §4, `OMP-TRANSLATION.md:186`). This is the phase that makes the layer
      actually universal for Anthropic-compatible and Responses clients.
- [ ] **Phase 5** — Anthropic object-input reshape wired into the Anthropic
      state machine for non-OMP/pi families.

## 8. Risks

| Risk | Mitigation |
|---|---|
| Mis-detecting a family and reshaping a client that needed its own schema | never floor/reshape on doubt; family must be proven by name-set or schema fingerprint |
| Per-family reshape tables drifting from client schemas | each rule cites `reference/...:line`; corpus regeneration scripted and byte-identical |
| Streaming Anthropic/Responses fan-out breaks the existing delta state machines | Phase 4 is a dedicated withhold-buffer redesign, not a bolt-on (already the documented deferral) |
| Anthropic `input` vs chat `arguments` string divergence | reshape runs before the surface encoder; one shared rule, two encoders |
| Adding mappings that rename to a plausible-but-wrong target | keep "no invented mappings"; only genuinely-equivalent behavior maps |

## 9. Out of scope

- gemini-cli (native Gemini REST; would need a Google API shim — see
  `UNIVERSAL-CLIENTS.md:42-50`).
- New wire tools (the gate forbids them).
- BYOK / non-free lanes.
- Upstream server behavior (`reference/gateways/`, `routers/` own no tools).
