# Tool-name translation — one mapper per request, ownership by the ordered pass, universal wire-name legalization

Status: accepted (shipped in #686, #687, #689; corpus coverage in #688).
Scope: `backend/internal/convert` (request rewrite + response restore) and the
three entry handlers in `backend/internal/server`. Goal: any client can point
at the proxy and see its own tool names, whatever they look like, without the
upstream ever seeing a foreign, duplicated or malformed toolset.

## Why this exists

Upstream's free-mode gates classify and penalize requests by the TOOLSET they
offer (`foreign_toolset`, `foreign_tool_names`, and the sticky
`third_party_client` cap; mirror in `wirefacts/testdata/wire/common/src/
constants/foreign-client-signals.ts`). Clients dispatch on the name the model
returns, so the proxy must both (a) send names upstream that look first-party
and (b) hand every call back under the exact name the client offered.

## Non-goals (deliberate non-changes)

- **No argument or schema translation.** The client's parameter schema is
  forwarded untouched; the model fills arguments per the schema it was shown.
  Only names change.
- **Message history is not rewritten.** `messages[].tool_calls[].function.name`
  carries no pattern in the OpenAI schema (only the `tools[]` declaration
  does), and rewriting model-visible history would be a behavior change nothing
  requires. The mapper knows the mapping if a strict upstream ever rejects one.
- **No invented mappings.** `clientToOfficial` only lists names whose behavior
  genuinely matches an official tool; everything else is passed through or
  virtualized, never renamed to a plausible-but-wrong target.
- **No client-native `mcp__` stripping.** `RestoreName` strips the prefix only
  when the request leg added it (map-first, then a strip guarded by
  `clientToUpstream[stripped] == name`), so a client MCP name like
  `mcp__my_tool` passes through byte-identically.

## Rules

1. **One request, one mapper.** The handler normalizes with
   `convert.NormalizeRequestMappedOpts` and passes the mapper it returns into
   `chatCore`, which threads it into `relayStats.toolMap`; every relay surface
   (chat streaming and not, `/v1/messages`, `/v1/responses`) restores with that
   instance. The response mapper is NEVER rebuilt from the request body: the
   request leg's name-uniqueness dedupe exists only on the instance that ran
   `ToUpstream`, and a rebuild parses `tools[]` only, so it cannot see the
   legacy `functions` shape either. Rebuilding is what leaked `mcp__execute`
   to opencode v2 (#685).
2. **Ownership belongs to the ordered pass.** `ToUpstream` walks `tools[]` in
   order and decides who keeps a shared wire name: the first claimer keeps it,
   later tools resolving to the same name virtualize to `mcp__<client name>`,
   and the same pass writes the reverse (restore) map — a name no longer needed
   by its tool is cleared. `NewToolMapper`'s reverse slots are provisional
   only, for a mapper used without `ToUpstream`. Two maps disagreeing about
   ownership is the defect fixed in #687 (Roo-Code offers `write_file` and
   `write_to_file -> write_file`; the legacy `functions` shape bound
   `run_terminal_command` to `execute` instead of `bash`).
3. **Virtual names are legalized and collision-proof.** `wireVirtualName`
   emits `mcp__<client name>` when that is wire-legal and fits, otherwise
   `mcp__<ascii base>_<8 hex of sha256(client name)>` bounded to 64 chars.
   The hash tail is mandatory: substitution and truncation are not injective
   (`a.b`, `a b`, `a/b` all sanitize to `a_b`), so a base-only name would
   deliver one client's call to another client tool. The dedupe path reuses the
   same helper plus a counter, covering a client that offers the same illegal
   name twice.
4. **Grammar gate before every pass-through.** The upstream is an
   OpenAI-shaped tools endpoint, so names must match `^[A-Za-z0-9_-]{1,64}$`
   (official wording and the pinned local copy:
   `reference/protocols/openai-openapi/openapi.json`, `ChatCompletionFunctions`;
   Anthropic allows 128, so 64 is the strict rule) and ONE illegal name fails
   the whole request. Order in `resolveUpstreamTool`: official/custom signature
   name → known mapping → **grammar gate** (illegal → `wireVirtualName`) →
   client `__`/MCP passthrough → foreign harness name (→ `wireVirtualName`) →
   verbatim. Codewhale's dotted `web.run`
   (`crates/tui/src/tools/web_run.rs:356`) is the live case this closes; the
   same clamp-with-hash appears in hermes-agent's own client code
   (`tools/mcp_tool_schema.py:137-173`).
5. **`tool_choice` follows the same decision.** A pin on a mapped, foreign or
   illegal name is rewritten to that tool's wire name, or upstream sees a
   choice for a tool it was not offered.

## Invariants (asserted by the corpus sweep)

For every client toolset: wire names are unique; every wire name matches the
grammar; no wire name is in `ForeignHarnessToolNames`; every wire name restores
to the EXACT client name (case preserved) through `RestoreName` and both
`FromUpstreamChunk` shapes; and `ClassifyWireTools` still reads first-party.

## Verification

- `backend/internal/convert/toolmap_corpus_test.go` — 21 harnesses / 637
  registry-declared names from the (gitignored) `reference/` corpus, each with
  `file:line` provenance; regenerate with `scripts/extract-tool-calls.sh`
  (byte-identical on re-run; refuses to emit an empty harness).
- `backend/internal/convert/toolmap_wirename_test.go` — legalizer properties,
  base collisions, mixed toolsets, shadowed official/foreign names, repeated
  illegal names, pinned `tool_choice`.
- `backend/internal/server/tool_dedupe_restore_test.go` — the #685 case
  end-to-end on all three surfaces plus the legacy shape.
- `backend/internal/convert/toolmap_dedupe_test.go` — dedupe and reverse-slot
  ownership, both orders.
- Every one of these was proven RED before its fix (production hunks stashed);
  a regression test that never failed pins nothing.

## Amendment 2026-09-30 — OMP/pi-family floor path, gate correction

Status: accepted · 2026-09-30 · amends (does not rewrite) the above; live
image `rel` (post-`82d896bb`; PRs #34/#35/#36 floor+restore, #38/#39
fallback routes + OMP-vocabulary reshape). Pins: vendor `0.0.204` /
UpstreamSHA `775383b3` (`scripts/vendor-version.txt`,
`backend/internal/wirefacts/wirefacts_gen.go:7`).

1. **The tools-keyed foreign gate is a historical mirror.** The "Why this
   exists" paragraph above cites `foreign_toolset`, `foreign_tool_names`
   and the sticky `third_party_client` cap via
   `wirefacts/testdata/wire/common/src/constants/foreign-client-signals.ts`.
   Upstream has since deleted that file; the live detector is the
   edge-stamped CF-Worker, which is tools-blind
   (`backend/internal/convert/cf_worker_signals.go`). The LIVE chat 503 gate
   keys on tool DEFINITIONS ONLY (bisect 2026-09-30: OMP tool names on a
   neutral system prompt still 503; all-16-official tools under the full
   77KB OMP system prompt still 200). System prompt content is exonerated.
   Envelope extras (`store`, `stream_options`, `max_completion_tokens`,
   `reasoning_effort`) are deliberate contracted passthroughs for real
   client fleets + spend metering, not gate material.
2. **Correction to the "No argument or schema translation" non-goal
   (L20-22): names-only holds for other clients only.** For OMP/pi-family,
   detected by an intent-`i` schema prop OR 2+ of
   {`eval`, `learn`, `manage_skill`, `context_notes`}, schemas ARE
   substituted: the wire carries exactly the 16 canonical CLI tool defs +
   the `end_turn` pin (`backend/internal/convert/tools_floor.go`,
   `backend/internal/convert/convert_request.go:182-187`,
   `backend/internal/upstream/clitools.go:28-30`; upstream `topUp` drops
   `decide`). Zero foreign riders. On the response leg, client names are
   restored AND CLI args are reshaped to OMP shape (`bash`/`read`/`edit`/
   `write`/`grep`/`glob`/todo-phase-form/`web_search`, plus fallbacks
   `ask_user`→`ask`, `read_url`→`read`, `list_directory`→`read`,
   `skill`→read-skill-URI; OMP-vocabulary emission reshaped likewise).
3. **OMP floor-only path + dropped tools (degraded loop by design).**
   OMP-only tools with no CLI equivalent (`eval`, `task`, `learn`,
   `manage_skill`, …) never ride the wire — they are dropped, not
   virtualized, so a model turn that needs one cannot dispatch it and the
   agentic loop degrades client-side. Unrouted first-party emissions
   (`suggest_followups`, `gravity_index`, `render_ui`,
   `report_project_profile`) likewise fail client-side. Floor, not mapping,
   is what keeps the OMP gate green.
