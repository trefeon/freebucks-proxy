# Universal gateway probes (Phase 0: items 0.2 + 0.3)

Truth date: 2026-10-03. Base: `origin/main @ 1cdd3c0a`.
Hermetic pins: `backend/internal/registry/gateway_probes_test.go`
(`TestGatewayProbe*`, package `registry_test` — test-only import of
`convert`, so the archtest matrix and convert's purity guard are untouched).

No live traffic ran for this doc (per lane rules). The upstream free-tier
gate keys on tool DEFINITIONS — live bisect 2026-09-30
(`docs/OMP-TRANSLATION.md §1`): OMP defs + neutral prompt → 503
`The model is temporarily unavailable`; all-16-official defs + the full
77 KB OMP system prompt → 200. System-prompt content is exonerated. The
probes therefore drive the real request leg
(`convert.NormalizeRequestMappedOpts`: rename → substitute → floor) and pin
the wire property each verdict follows. A verdict marked **bisected** was
observed live; **unbisected** is the proxy-side wire outcome plus the
expected gate reading — Phase 3 arg-rule work is ordered by 503 frequency,
unbisected shapes first.

## 0.2 Definition-vs-gate probe matrix

Floor reference: the 16 canonical CLI defs in fixture order —
`read_files, str_replace, write_file, run_terminal_command, code_search,
glob, list_directory, write_todos, web_search, read_url, ask_user,
suggest_followups, gravity_index, render_ui, skill, report_project_profile`
(`backend/internal/upstream/testdata/cli-tools.json`, mirrored at
`backend/internal/convert/testdata/cli-tools.json`) — plus the
`end_turn`/`decide` pins the normalizer appends
(`convert/schemacache_endturn.go`).

| # | Client | Tool (shape) | Wire outcome (pinned) | Gate verdict |
|---|---|---|---|---|
| 1 | OMP family | `bash`/`read` with intent-`i` schemas + `eval` + `task` | Exactly 16 floor defs in fixture order + `end_turn` + `decide` pins; zero riders; zero `i` schemas; zero `(client tool:` annotations (`TestGatewayProbeOMPFloorIsGateGreen`) | **200 bisected** — this is the §1 green shape |
| 2 | Codex CLI | `apply_patch` freeform-`input` string (audit §5; codex WIRE-NOTES §4) | Rides with the client's own schema; no canonical substitution exists for a name outside the 16-def fixture; identity restore (`TestGatewayProbeCodexApplyPatchRidesNamesOnly`) | **Unbisected** — names-only ride, gate risk open. Bisect first in Phase 3 (plan §3.3 gap 3) |
| 3 | OpenClaw class | OpenAI-chat-shaped route tools: plain `read{path}` + `write_file` (no OMP markers) | `read` renames to `read_files` AND is substituted with the canonical def; `write_file` substituted; restores to exact client names; no family detected (`TestGatewayProbeOpenClawClassRenamesToOfficial`) | **200 expected, unbisected** — gate sees canonical bytes. OpenClaw's exact vocabulary is audit-pending (no corpus row); this pins the class its route tools fall into |
| 4 | Generic MCP / Hermes class | Dotted `web.run` (live case: Codewhale `web_run.rs:356`, decision rule 4) | Virtualized to a grammar-legal `mcp__` name (hash tail, collision-proof); never rides verbatim; restores to `web.run` exactly (`TestGatewayProbeDottedMCPVirtualizes`) | **Unbisected** — virtualized defs are foreign-schema riders by the §1 definition, so Hermes-native shapes sit in the same bucket until bisected. Phase 3 orders Hermes arg rules (audit §5 triage a) by this bucket's 503 frequency |
| 5 | OMP family | `tool_choice` pin on dropped `task` | Pin removed; OpenAI default `auto` applies (`TestGatewayProbeToolChoiceFollowsTheFloor`) | **200 bisected pattern** — upstream never sees a pin for an unoffered tool |
| 6 | OMP family | `tool_choice` pin on client name `bash` vs wire name `run_terminal_command` | Observed ordering consequence, pinned not fixed: `floorOnlyOMP` compares the pin against wire names BEFORE `RenameRequestToolChoice` runs, so the client-name pin degrades to `auto`; only the wire-name pin survives (same test) | Behavior pin for Phase 1 (the IR's Emit pass should rename-then-gate, not gate-then-rename) |

What the matrix orders for Phase 3: bisect row 2 (Codex) and row 4
(Hermes/MCP-virtualized) against live upstream first — both ride foreign
schemas today. Row 3 needs only its vocabulary row added to the corpus, not
a bisect. Row 6 is a Phase-1 IR design input, not a gate question.

## 0.3 OpenClaw conformance-first spec

Transport fits 1:1 — OpenClaw custom providers select
`openai-completions | openai-responses | anthropic-messages`
(custom-providers doc), which are exactly our three surfaces
(`/v1/chat/completions`, `/v1/responses`, `/v1/messages`; recipe:
`baseUrl=http://HOST:3457/v1`, `apiKey` → proxy key per
`docs/UNIVERSAL-CLIENTS.md:40`). Auth is Bearer
(`server/middleware_auth.go:108-143`). No conformance exists today
(conformance covers codex/goose/cline/continue only). Phase 3 lands
`backend/internal/server/conformance_openclaw_test.go`, cloned from the
`conformance_codex_test.go:1-17` pattern (live HTTP surface +
`testutil.MockUpstream`, assert the byte shape the client parses). Each
`compat` key maps to ≥1 future assertion:

| `compat` key | Required proxy behavior | Future assertion (Phase 3) |
|---|---|---|
| `supportsStore` | `store:false` accepted and kept client-side — it has no chat-completions analogue and must never surface as a chat field (same contract as Codex Turn-2 `include`, pinned for codex in `conformance_codex_test.go:315-324`) | Responses Turn-1 with `store:false` → upstream chat body carries no `store`; stream still terminates on `response.completed` |
| `supportsStrictMode` | `strict` flags on function tools tolerated end-to-end (accepted, forwarded or safely stripped — never a 4xx) | Chat + Responses turns carrying `strict:true` tools stay green; wire defs still gate-clean |
| `requiresStringContent` | Message `content` encoded as a string on the surfaces that require it | Per-surface encoder test: parts → string where required, no object leak |
| `strictMessageKeys` | Unknown message keys never break the turn (stripped or tolerated per surface) | Turn with extra message keys relays cleanly on all three surfaces |
| `toolCallArgumentsEncoding` | `arguments` as JSON string (chat/Responses) vs `input` object (Anthropic) — reshape runs before the surface encoder (audit §5 consequence 2) | Same wire call renders string-encoded on chat/Responses and object-encoded on Anthropic |
| `requiresOpenAiAnthropicToolPayload` | Tool payload shape matches the active surface's schema (function-tool vs `input_schema`) | Tool-def relay test per surface |
| `requiresAssistantAfterToolResult` | History replay keeps assistant→tool-result pairing intact through translation (the PR #226 contract codex relies on) | Round-trip test: function_call + function_call_output items → upstream assistant `tool_calls` + `role:tool` pair, no re-issued call |
| `requiresReasoningContentOnAssistantMessages` | Thinking/reasoning replays accepted on assistant messages (cf. `conformance_cline_test.go:209-278`) | Thinking-signature replay turn stays green |

Shared guarantees reused from Codex §3 (not re-proven): `type` on every
SSE JSON, output-item ordering, `response.completed` with `response.id` +
usage triple, EOF-without-completed = error, trailing usage on chat
streams. OpenClaw-native arg shapes sit in the Phase-2 queue, ordered by
corpus frequency once its corpus row exists.
