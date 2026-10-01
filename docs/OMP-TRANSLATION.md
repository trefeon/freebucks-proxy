# OMP / pi-family translation

Single canonical page for the OMP-family request/response translation layer,
plus the pi family (§6) — pi is the project OMP forked from and is a separate,
non-floor family. Truth date: 2026-10-01. Every behavior below names its code
path.

## 1. What the gate checks: definitions only

Live bisect 2026-09-30 (`convert/tools_floor.go:7-11`):

| Wire toolset | System prompt | Result |
|---|---|---|
| OMP tool defs (foreign schemas riding) | neutral | 503 `The model is temporarily unavailable` |
| Official 16 canonical defs | full 77 KB OMP system message | 200 |

System-prompt content is exonerated. The gate keys on tool **definitions**:
any foreign-schema definition riding alongside the floor (`mcp__`
virtualizations, `find_files` under a non-floor name) trips it. Substitution
alone is insufficient — riders must not ride at all. Hence floor-only.

## 2. Request leg (`convert/convert_request.go:165-197` `NormalizeRequestMappedOpts`)

Order: `NewToolMapper` → normalize → `ToUpstream` (rename) →
`SubstituteCanonicalDefinitions` → `floorOnlyOMP` (OMP only) →
`RenameRequestToolChoice`.

### Detection (`convert/tools_floor.go:47-78`)

OMP-family iff **any** tool schema carries the injected intent-`i` property
(`parameters.properties.i`), **or** 2+ of the signature names appear:

- `eval`, `learn`, `manage_skill`, `context_notes` (`ompSignatureNames`, `:28-33`)

Invalid bodies report false — never floor-only on doubt. OMP is checked FIRST,
so a stripped OMP toolset cannot read as pi (the two families restore different
arg shapes). pi is a separate family with its own detection and NO floor — see
§6.

### Rename: client → wire (`convert/toolmap_request.go:41-179`)

OMP-declared names that rename (keys lowercase):

| Client | Wire |
|---|---|
| `bash`, `powershell` | `run_terminal_command` |
| `read` | `read_files` |
| `edit` | `str_replace` |
| `write` | `write_file` |
| `grep` | `code_search` |
| `todo` | `write_todos` |
| `find` | `glob` (file-pattern search; restores shape-aware to `find`, §3) |
| `edit-diff` | `apply_patch` |
`glob`, `web_search` are already official (identity). `wait`,
`context_notes`, `new_context`, `eval`, `learn`, `manage_skill`, `task` have
no official equivalent and never map (kept drops, §4).

### Canonical substitution (`convert/tools_normalize.go:67-117`)

Renamed entries are replaced with the byte-canonical CLI definitions
(`canonicalToolDefs`, fixture `cliToolsFixture`) for every wire name in
`substitutedWireNames` (`:53-65`: the 8 above plus `glob`, `list_directory`,
`web_search`, `read_url`). `end_turn`/`decide` pins and `mcp__`
virtualizations are never substituted — they keep riding verbatim into the
floor pass.

### Floor-only (`convert/tools_floor.go:87-132`)

`payload["tools"]` becomes exactly the 16 canonical CLI definitions in
fixture order plus preserved `end_turn`/`decide` pins. Everything else is
dropped. A `tool_choice` pin naming a dropped wire name is deleted (default
`auto`); pins on floor names survive for `RenameRequestToolChoice`.
Upstream `topUpCliTools` drops `decide` (`upstream/clitools.go:28-30`), so
the wire the gate sees is **16 + `end_turn`**.

### Fallback routes (`convert/tools_floor.go` `floorFallbacks`)

Floor tools OMP never declares still ride (gate requirement) and restore to
the OMP equivalent via `RegisterFloorFallbacks` (first claim wins, never
overrides a real mapping). The table is registered UNCONDITIONALLY for every
floor wire name that has an OMP target, not only when the client declared
that tool: resolution is total, so a trimmed toolset (`--tools`,
`PI_NO_INTENT`) can never leak a wire-only name back to the client.

| Wire (model sees) | Client (OMP dispatches) | Args |
|---|---|---|
| `run_terminal_command` | `bash` | reshaped (extras dropped) |
| `read_files` | `read` | reshaped (fan-out per path) |
| `str_replace` | `edit` | reshaped (fan-out per replacement) |
| `write_file` | `write` | reshaped (nested content preserved) |
| `code_search` | `grep` | reshaped (`cwd`→`path`) |
| `glob` | `glob` / `find` | origin-aware (ex-`find` → `find {pattern}`) |
| `write_todos` | `todo` | reshaped (phase form) |
| `web_search` | `web_search` | reshaped (`depth` dropped) |
| `ask_user` | `ask` | reshaped (ids synthesized) |
| `read_url` | `read` | reshaped (`url` → `path`) |
| `list_directory` | `read` | verbatim (`{path}` is already valid OMP `read` shape) |
| `skill` | `read` | reshaped (`skill://` URI) |
| `gravity_index` | `web_search` | reshaped (`query`/`category`/`slug` → `query`) |

Unrouted — no OMP equivalent (verified against OMP's builtin registry,
`pi-coding-agent/src/tools/builtin-names.ts`): `suggest_followups`,
`render_ui`, `report_project_profile`. They ride the wire
because the gate requires all 16 canonical CLI definitions, so the model can
always call them; the response leg never relays them as tool calls
(§3a text fallback).

## 3. Response leg

Names restore via `RestoreName`/`FromUpstreamChunk`
(`convert/toolmap_response.go:10-87`; map-first, identity when unmapped).
Args reshape via `ReshapeArgsFor` / `ReshapeMessageCalls`
(`convert/tools_reshape.go:23-87`), gated on `floorOnly` — no other client's
relay reaches these paths. Reshape runs **before** restore, keyed by wire
name (non-streaming: `server/openai_stream.go:275-292`).

| Wire | Client | Rule (`tools_reshape.go`) — lossy notes |
|---|---|---|
| `run_terminal_command` | `bash` | single: `{command, cwd?, timeout≤timeout_seconds}`; extras dropped, `i` omitted |
| `read_files` | `read` | **fan-out**: one `{path}` per `paths` entry, order preserved; single stays 1:1; object entries keep `path`, `offset`/`limit` dropped |
| `str_replace` | `edit` | **fan-out**: one flat `{path, old_string, new_string}` per `replacements` entry, order preserved; single stays 1:1; `allowMultiple` dropped |
| `write_file` | `write` | `{path, content}`; `instructions` dropped |
| `code_search` | `grep` | `{pattern, path≤cwd}`; flags/`maxResults` dropped |
| `glob` | `glob`/`find` | origin-aware: ex-`find` origin → OMP `find {pattern}` (source: first present of `pattern`/`query`; `cwd`/`max_results`/`grep_keywords` dropped); native → OMP `glob {path}`; already-OMP-shaped emissions pass through |
| `write_todos` | `todo` | **fan-out**: `op:init` with STRING items (`list:[{phase:"Tasks", items:[string]}]` — OMP's `InitListEntry.items` is `string[]`, so the CLI `{task, completed}` objects are rejected), then one trailing `op:done {task}` per completed entry (init has no per-item status); empty dump → `op:view`; an already-OMP-shaped emission (no `todos` key) passes through |
| `web_search` | `web_search` | `{query}` only; `depth` dropped |
| `ask_user` | `ask` | ids synthesized `q0…` (OMP requires them); labels/descriptions verbatim (probe UI: `ask` without per-question `id` fails validation) |
| `read_url` | `read` | `{path≤url}`; `max_chars` dropped |
| `skill` | `read` | `{path: "skill://"+name}` |
| `gravity_index` | `web_search` | `{query: query || q || category+" developer services" || slug+" developer documentation"}` |

All rules are total: unknown shapes / invalid JSON pass through verbatim
rather than failing the turn. The three unrouted names have no reshape rule
because they have no client tool to reshape for — they are suppressed by the
text fallback below, never passed through.

### 3a. Text fallback for unroutable floor calls (`convert/tools_textfallback.go`)

A floor tool with no OMP equivalent cannot be dispatched by the client, so its
call must not be relayed as a tool call at all. `TextFallback` classifies the
four names; `ApplyTextFallbacks` (non-streaming: chat, Anthropic, Responses)
and `stripTextFallbackCalls`/`flushTextFallbacks` (`server/openai_chunk_pipeline.go`,
streaming) remove the call and render its payload as assistant text:

| Wire | Handling | Rendered |
|---|---|---|
| `suggest_followups` | render | markdown list of the followup prompts |
| `render_ui` | render | markdown link (plain string links only; a `gravity_index` link ref has no resolvable URL) |
| `gravity_index` | render | one line naming the discovery request it cannot service |
| `report_project_profile` | absorb | nothing (internal telemetry) |

Invariants: the call is dropped with its name (streaming strips the whole
entry, including nameless continuation fragments, like `end_turn`), the
rendered text is appended to any content already produced, and when the turn
delivered no dispatchable call the terminal `finish_reason` flips
`tool_calls`→`stop` so no client waits on a call it never received. An
ordinary call in the same turn keeps the turn a `tool_calls` turn. Gated on
floor-only: every other client is untouched.

### 3b. The rest of the OMP surface round-trips verbatim

OMP capabilities that have no official CLI equivalent are deliberately absent
from the wire (the gate rejects any foreign-schema rider), but they are NOT
errors: OMP's own system prompt ships a `# Tool Inventory` naming them, so the
model can call them from prompt vocabulary and the relay passes the name
through untouched.

| Group | Names | Path |
|---|---|---|
| Loop / subagent | `wait`, `hub` | verbatim both ways |
| Delegation | `task` | name verbatim; args gain synthesized `tasks[]` batch (singular-to-batch normalization — the harness validates non-empty `tasks[]`, valid batches pass through) |
| OMP-only builtins | `eval`, `learn`, `manage_skill`, `context_notes`, `new_context`, `debug`, `ida`, `security_scan`, `checkpoint`, `rewind`, `github`, `lsp`, `ast_grep`, `ast_edit` | verbatim |
| Hidden | `yield`, `goal`, `think` | verbatim |
| `xd://` devices | ast_grep/ast_edit/lsp/github/debug/checkpoint/rewind/mem_*/security_scan/… | ride through the model's `write`/`read` calls (reshaped to OMP shape, `xd://` path preserved) |
| External | `mcp__<server>_<tool>` | verbatim, namespace preserved |
Regression proof: `server/floor_omp_surface_test.go`
(`TestFloorOMPToolSurfaceRoundTrip` drives every name in the OMP registry and
asserts the name restores). Delegation args specifically:
`TestFloorOmpDelegationStreamsVerbatim` (fragmented `task` + whole `hub`, turn
stays `tool_calls`, task args carry the synthesized `tasks[]`) and
`TestFloorOmpDelegationAcrossSurfaces` (Anthropic + Responses);
`convert/tools_reshape_test.go` (`TestReshapeTaskSingularToTasksBatch`:
singular → batch, valid-batch and empty passthrough, pi untouched).

### Streaming: withhold + inject (`server/openai_chunk_pipeline.go`)

CLI-shaped arg fragments cannot reshape incrementally, so `reshapeBuffer` withholds `arguments` bytes per tool-call index (id and name keep flowing; continuations inherit the recorded wire name) and `reshapeFlush` injects the reshaped whole per index onto the terminal chunk with the client name restored inline. Fan-out expands at flush: the first call keeps its index, extras take fresh indexes above every observed upstream index. The client SDK concatenates fragments, so withheld-empties + wholes assemble exactly the OMP-shaped calls. Unreshapable buffers flush verbatim — no call is ever swallowed.
Emission to the client is OMP vocabulary throughout on both legs.
Non-streaming chat, Anthropic and Responses relays share `ReshapeCompletionCalls` (reshape + fan-out before the name restore). Streaming Anthropic/Responses fan-out stays deferred: those translators relay delta events live with no withhold architecture (noted at both call sites); OMP speaks the chat surface, where both legs fan out.

## 4. Degraded by design

- **Dropped defs never ride**: `eval`, `task`, `wait`, `learn`,
  `manage_skill`, `context_notes`, `new_context`, `mcp__` virtualizations and
  unmapped customs are cut from the WIRE by `floorOnlyOMP` — the model is
  never shown them (the gate rejects any foreign-schema rider). This is a
  wire-shape constraint, not a capability loss: the model still calls them
  from OMP's `# Tool Inventory` prompt vocabulary and they round-trip (§3b) —
  verbatim, except `task`, whose args gain the synthesized `tasks[]` batch.
- **Routed, not dropped**: OMP `find` rides as floor `glob` and restores
  shape-aware to `find` (§3) — the only OMP-only tool with a CLI equivalent.
- **Unroutable floor calls degrade, never fail**: a model call to
  `suggest_followups`, `gravity_index`, `render_ui` or
  `report_project_profile` is suppressed and rendered as assistant text (or
  absorbed), so the OMP dispatcher never answers `Tool <name> not found`
  (§3a).

## 5. Account discipline + live proof

Single spaced probes are fine; retry/churn storms get accounts banned
(`toolmap_request.go:349-353`: verbatim riders 503'd every request and the
retry churn banned the account). Keep `CHAT_AUTO_RETRY=false` while probing
new envelopes.

Live proof: image rel post-`82d896bb` — 5/5 same-session triple (chat,
reasoning, harness-tools normalizer) plus streaming and non-streaming tool
turns (`bash` → `run_terminal_command` → restore → local exec → turn-2 relay
→ final answer).

## 6. pi family (the project OMP forked from)

pi (`reference/harnesses/pi`, `packages/coding-agent/src/core/tools/index.ts`
`allToolNames`) is a *second, distinct* family, and unlike OMP it is **not**
floor-only: every core name has an official wire target, so nothing has to be
dropped for the gate. Detection lives in `convert/tools_floor.go`
(`clientFamily`, `piToolVocabulary`, `isPiToolset`, `hasPiEditFingerprint`,
`detectFamilyBody`).

| Signal | Rule |
|---|---|
| name subset | every declared name is in `piToolVocabulary` (`read`, `bash`, `powershell`, `edit`, `write`, `grep`, `find`, `ls`) and at least two appear |
| edit fingerprint | an `edit` tool whose `parameters.properties.edits.items.properties` carries `oldText` + `newText` (`edit.ts:34-54`) — catches pi sessions carrying extension tools (subagent spawners, MCP bridges), whose extra names leave the vocabulary |

### Request leg

No floor: the eight core names rename to official wire names, canonical
definitions are substituted, and extension tools keep riding virtualized
(`mcp__<name>`).

| Client | Wire |
|---|---|
| `read` | `read_files` |
| `bash`, `powershell` | `run_terminal_command` |
| `edit` | `str_replace` |
| `write` | `write_file` |
| `grep` | `code_search` |
| `find` | `glob` |
| `ls` | `list_directory` |

### Response leg

`piReshapeWires` scopes the rules to the seven wire names pi's core toolset
maps onto — an extension declared under any other canonical name keeps its own
args. Rules live in `piReadArgs` / `piEditArgs` (`convert/tools_reshape.go`).

| Wire | pi shape | Note |
|---|---|---|
| `read_files` | `{path[, offset, limit]}` | fan-out per path; **offset/limit KEPT** (OMP drops them) |
| `str_replace` | `{path, edits:[{oldText,newText}, …]}` | ONE batch call, not one per replacement |
| `run_terminal_command` | `{command, timeout}` | not `timeout_seconds` |
| `write_file` | `{path, content}` | |
| `code_search` | `{pattern, path?}` | |
| `glob` | `{pattern, path?}` | |
| `list_directory` | `{path}` | |

### Text fallback

pi declares no todo/web_search/ask/skill tool, so a model call to
`write_todos`, `web_search`, `read_url`, `ask_user`, `skill`,
`suggest_followups`, `render_ui` or `gravity_index` would reach pi's
dispatcher as `Tool <name> not found`. `piFloorRenderers` renders each payload
as assistant text (todo checklist, query note, URL note, question list) or
absorbs it (`report_project_profile`). `piRoutable` keeps the REAL call when the
wire name restores to a pi core tool, or *is* one of the client's own declared
tools — a pi extension named `web_search` still receives `web_search`.

### Gate

`ToolMapper.ResponseRewrite()` (family != none) gates the streaming withhold
buffers and the unroutable-call strip; `FloorOnly()` still means "the client
defs were replaced" and stays OMP-only.

### Delegation

A pi extension registering `spawn_agent` rides the wire virtualized as
`mcp__spawn_agent`; its calls restore to `spawn_agent` with pi's own args.
Spawning is client-side (the extension runs the subagent), so a verbatim call
is the whole requirement — a rename, virtualization or drop is what turns it
into "Tool spawn_agent not found". Pinned by
`server/conformance_pi_omp_test.go` (`TestConformancePiChatToolRenameRestore`,
`TestConformancePiArgReshapeE2E`, `TestConformancePiStreamArgReshape`,
`TestConformancePiDelegationAcrossSurfaces`) and
`convert/tools_pifamily_test.go`.
