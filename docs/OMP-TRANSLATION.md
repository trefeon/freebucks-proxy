# OMP / pi-family translation

Single canonical page for the OMP-family request/response translation layer.
Truth date: 2026-09-30. Every behavior below names its code path.

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

Invalid bodies report false — never floor-only on doubt.

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

### Fallback routes (`convert/tools_floor.go:140-162`)

Floor tools OMP never declares still ride (gate requirement) and restore to
the OMP equivalent via `RegisterFloorFallbacks` (first claim wins, never
overrides a real mapping):

| Wire (model sees) | Client (OMP dispatches) | Args |
|---|---|---|
| `ask_user` | `ask` | reshaped (ids synthesized) |
| `read_url` | `read` | reshaped (`url` → `path`) |
| `list_directory` | `read` | verbatim (`{path}` is already valid OMP `read` shape) |
| `skill` | `read` | reshaped (`skill://` URI) |

Unrouted — no OMP equivalent, still fail client-side with `not found`
(probe §A): `suggest_followups`, `gravity_index`, `render_ui`,
`report_project_profile`.

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
| `write_todos` | `todo` | `op:init` + phase-form `list:[{phase, items}]`; empty dump → `op:view` (probe §B: bare `init` errors `Missing list`; Runtime-broken #3: bridge stringifies nested `list`, so `init` cannot round-trip — `view` works) |
| `web_search` | `web_search` | `{query}` only; `depth` dropped |
| `ask_user` | `ask` | ids synthesized `q0…` (OMP requires them); labels/descriptions verbatim (probe UI: `ask` without per-question `id` fails validation) |
| `read_url` | `read` | `{path≤url}`; `max_chars` dropped |
| `skill` | `read` | `{path: "skill://"+name}` |

All rules are total: unknown shapes / invalid JSON pass through verbatim
rather than failing the turn. No rule exists for unrouted names
(`gravity_index` etc. pass through byte-identical).

### Streaming: withhold + inject (`server/openai_chunk_pipeline.go`)

CLI-shaped arg fragments cannot reshape incrementally, so `reshapeBuffer` withholds `arguments` bytes per tool-call index (id and name keep flowing; continuations inherit the recorded wire name) and `reshapeFlush` injects the reshaped whole per index onto the terminal chunk with the client name restored inline. Fan-out expands at flush: the first call keeps its index, extras take fresh indexes above every observed upstream index. The client SDK concatenates fragments, so withheld-empties + wholes assemble exactly the OMP-shaped calls. Unreshapable buffers flush verbatim — no call is ever swallowed.
Emission to the client is OMP vocabulary throughout on both legs.
Non-streaming chat, Anthropic and Responses relays share `ReshapeCompletionCalls` (reshape + fan-out before the name restore). Streaming Anthropic/Responses fan-out stays deferred: those translators relay delta events live with no withhold architecture (noted at both call sites); OMP speaks the chat surface, where both legs fan out.

## 4. Degraded by design

- **Dropped tools never ride**: `eval`, `task`, `wait`, `learn`,
  `manage_skill`, `context_notes`, `new_context`, `mcp__` virtualizations and
  unmapped customs are cut by `floorOnlyOMP` — the model is never shown them,
  so their calls never need restoring. The agentic loop is degraded for these
  by design, not by bug (probe Runtime-broken items are third-party bridge
  behavior, not proxy bugs).
- **Routed, not dropped**: OMP `find` rides as floor `glob` and restores
  shape-aware to `find` (§3) — the only OMP-only tool with a CLI equivalent.
- **Undeclared floor tools fail client-side**: if the model calls
  `suggest_followups`, `gravity_index`, `render_ui` or
  `report_project_profile`, `RestoreName` identity passes the CLI name through
  and the OMP dispatcher rejects it with `not found`.

## 5. Account discipline + live proof

Single spaced probes are fine; retry/churn storms get accounts banned
(`toolmap_request.go:349-353`: verbatim riders 503'd every request and the
retry churn banned the account). Keep `CHAT_AUTO_RETRY=false` while probing
new envelopes.

Live proof: image rel post-`82d896bb` — 5/5 same-session triple (chat,
reasoning, harness-tools normalizer) plus streaming and non-streaming tool
turns (`bash` → `run_terminal_command` → restore → local exec → turn-2 relay
→ final answer).
