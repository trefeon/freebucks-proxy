# Universal Gateway Plan

Base: `origin/main @ 04564290`. Consolidates four read-only slice reports
(TransIR, 7× GoSteals scouts, ClientMatrix, FrontWins) + live repo facts into
one phased master plan. No code changed by this doc.

Slices: TransIR deep-read (`reference/new-api/relaykit/...`,
`reference/llmrelay-relay/internal/...`, `reference/one-api/relay/...`,
`reference/roo-code/src/api/...`,
`reference/vercel-ai/packages/provider/src/...`,
`reference/llm-api-translator/translator-proxy.mjs`,
`reference/litellm/litellm/llms/anthropic/...`); GoSteals NewApiRelay,
OneApiRelay, LlmrelayCore, JdanzigGw, InstaworkProxy, OpenzitiGw, OurBackend;
ClientMatrix (surfaces in `backend/internal/server/openai.go`,
`anthropic.go`, `middleware_auth.go`); FrontWins (Svelte SPA vs new-api React
admin).

## 1. Vision

freebuff-proxy becomes a **universal gateway**: any OMP-family or third-party
agent client speaks one of three surfaces — chat completions, Responses,
Anthropic Messages — and the proxy translates to exactly one upstream wire
(the freebuff CLI defs) without ever changing wire bytes. Families are
onboarded as **decoder + renderer pairs over a canonical IR**, never as
pipeline surgery. Every translation loss is a machine-readable diagnostic,
not a silent drop. Pool health (acquire → session → upstream → settle) is
observable per turn via trace phases, and the dashboard renders any client
state honestly.

Non-vision: we do NOT become a multi-upstream router (one wire target), a
model host, a BYOK lane, or a Google-API shim (gemini-cli stays out per
`docs/UNIVERSAL-TOOLS-AUDIT.md:224-230` and `docs/UNIVERSAL-CLIENTS.md:42-50`).

## 2. Non-negotiable invariants

These hold before, during, and after every phase. The IR changes decision
structure, never wire bytes.

1. **Floor-only gate.** The wire stays exactly today's 16 canonical CLI defs
   + pins (`backend/internal/upstream/clitools.go:12-17`,
   `backend/internal/convert/tools_floor.go:20-26`). Foreign riders trip the
   503; floor-only passes 200 (`docs/OMP-TRANSLATION.md:8-20`). `topUpCliTools`
   (`backend/internal/upstream/clitools.go:5-50`) remains an assertion, and
   `floorOnlyOMP` (`backend/internal/convert/tools_floor.go:204-256`) remains
   the OMP gate.
2. **First-wins ordered ownership.** `ToUpstream`'s ordered ownership pass
   (`backend/internal/convert/toolmap_request.go:462-539`) is the answer to
   "who owns this wire name". litellm's order-independent reservation
   (`reference/litellm/litellm/llms/anthropic/chat/transformation.py:205-265`)
   is NOT adopted for ownership — only its "only rewrites enter the maps"
   property is asserted.
3. **64-char grammar.** Tool-name grammar stays 64 chars
   (`backend/internal/convert/toolmap_request.go:265-282`); litellm's 128-char
   rule (`transformation.py:193-202`) is explicitly rejected.
4. **Rename / reshape / restore contract.** Rename
   (`toolmap_request.go:467-539` + `RenameRequestToolChoice :544-570`) →
   substitute (`tools_normalize.go:77`) + floor (`tools_floor.go:204-256`) →
   reshape (`tools_reshape.go:45-102`, rules `:543-776`) → restore
   (`toolmap_response.go:10-87`, map-first `:69-87`). Pipeline order
   `NewToolMapper → normalize → ToUpstream → SubstituteCanonicalDefinitions →
   floorOnlyOMP → RenameRequestToolChoice`
   (`backend/internal/convert/convert_request.go:165-205`,
   `docs/OMP-TRANSLATION.md:22-26`).
5. **One request, one mapper.** `NewToolMapper`
   (`toolmap_request.go:401`) per request; `relayStats.toolMap` threading
   untouched (`docs/decisions/tool-name-translation.md:37-45`).
6. **Client-side delegation.** Harness spawner names (`task`, `hub`,
   `spawn_agent`) never enter wire claims; they restore by identity
   (`docs/decisions/tool-name-translation.md:297-320`). The IR records this as
   `Kind`/`ExecBy` data, not folklore.
7. **Single-decode streaming.** `SanitizeChunkMapped` per chunk
   (`backend/internal/convert/sse.go:157`) with `float64` invariant; withhold
   buffers (`reshapeBuffer`/`reshapeFlush`) stay the only streaming state
   owners per surface (roo precedent:
   `reference/roo-code/src/api/transform/stream.ts:97-101`).
8. **Pool-health honesty.** `pool.Acquire` spill-order lanes with FIFO
   park/QUEUE_WAIT scale-out and ban-first tail; `session.Manager` owns
   create/poll/invalidate + claim UUID + `EndSession` refund receipts; `store`
   stays SQLite history/settings/sessions/`pool_state` under one `writeMu`.
   Spend stays record-only (relay usage → pool ledger); settle stays
   `DELETE freebucksRefund` replay. No shadow billing ledgers.

### OMP-family prompt-vocabulary vs schema risk (standing note)

The OMP floor-capability reminder is a **prompt-vocabulary** nudge injected in
the chat envelope (`appendOMPFloorReminder` in `backend/internal/upstream/chat.go`,
`docs/OMP-TRANSLATION.md:195-207`), while the gate enforces **schema**
(canonical defs). These can drift apart: the model may emit a tool the prompt
named but the schema forbids (or vice versa). Every phase that touches floor
wording or canonical defs MUST update both together and keep the
Anthropic/Responses reminder equivalent (Phase 3 item 3.1) in sync. Schema is
authoritative; prompt is advisory — never gate on prompt compliance.

## 3. Phase 0 — Doc / decision scaffolding (5 items)

Goal: answer the questions that Phases 1–3 depend on, with zero prod risk.

### 0.1 Freeze the invariants as an ADR (value M / effort S)

- Target: `docs/decisions/universal-gateway-invariants.md` (new), link from
  `docs/OMP-TRANSLATION.md` and `docs/decisions/tool-name-translation.md`.
- Content: §2 above verbatim + the four explicit non-changes from TransIR
  (one-mapper, first-wins, 16-def floor, client-side delegation).
- Acceptance: every Phase 1–4 item cites which invariant it preserves; review
  rejects any item that can't.
- Evidence: `backend/internal/convert/convert_request.go:165-205`,
  `toolmap_request.go:462-539`, `tools_floor.go:20-26,204-256`,
  `upstream/clitools.go:12-17`.

### 0.2 Definition-vs-gate probe matrix (value H / effort S)

- Target: `docs/UNIVERSAL-GATEWAY-PROBES.md` (new) + throwaway probe script
  (deleted after; per throwaway-script rule).
- Content: for each foreign client (Codex `apply_patch`, Hermes-native names,
  OpenClaw route tools, generic MCP names): send defs through the gate,
  record 200-vs-503 per shape. Codex defs "ride the wire unchanged
  (names-only) so gate risk is unbisected" (ClientMatrix §3 gap).
- Acceptance: a table of (client, tool, shape) → gate verdict; Phase 3 arg-rule
  work is ordered by 503 frequency.
- Evidence: `backend/internal/server/middleware_auth.go:89-103`,
  `backend/internal/convert/tools_floor.go:204-256`,
  `docs/UNIVERSAL-TOOLS-AUDIT.md:143-153`.

### 0.3 OpenClaw conformance-first spec (value M / effort S)

- Target: section in `docs/UNIVERSAL-GATEWAY-PROBES.md`; test lands in Phase 3.
- Content: translate OpenClaw's `compat` table (`supportsStore`,
  `supportsStrictMode`, `requiresStringContent`, `strictMessageKeys`,
  `toolCallArgumentsEncoding`, `requiresOpenAiAnthropicToolPayload` —
  custom-providers doc, Custom-route-key table) into required proxy
  behaviors: `instructions`-embedding when `supportsInstructions=false`,
  strict-keys safety, replay shapes (`requiresAssistantAfterToolResult`,
  `requiresReasoningContentOnAssistantMessages`).
- Acceptance: checklist where each `compat` key maps to ≥1 future assertion.
- Evidence: ClientMatrix §6; `backend/internal/server/conformance_codex_test.go:1-17`
  as the pattern to clone (no `conformance_openclaw_*` exists today).

### 0.4 Prompt-vocabulary vs schema risk register (value M / effort S)

- Target: `docs/decisions/universal-gateway-invariants.md` appendix (with 0.1).
- Content: enumerate every place prompt wording names tools
  (`appendOMPFloorReminder`, floor fallbacks `tools_floor.go:266-287`,
  `piFloorRenderers`) beside the schema that gates them; state the
  "schema authoritative, prompt advisory" rule.
- Acceptance: any floor-wording PR must tick the register.
- Evidence: `backend/internal/upstream/chat.go` (`appendOMPFloorReminder`),
  `backend/internal/upstream/clitools.go:5-50`,
  `docs/OMP-TRANSLATION.md:195-207`.

### 0.5 Surface-provenance convention (value S / effort S)

- Target: one paragraph in the invariants ADR.
- Content: adopt llmrelay's `LegacyMaxTokens`/`Inbound` provenance idea
  (`reference/llmrelay-relay/internal/translate/openai_request.go:27-35`):
  record which spelling/dialect each field came from so emitters round-trip
  faithfully (kills repeat `wire_time_unify`-class bugs).
- Acceptance: convention stated; enforced when the IR lands (Phase 1).
- Evidence: `openai_request.go:29-35`; our `wire_time_unify` history.

## 4. Phase 1 — Translation IR remake (8 items)

Goal: relaykit-style registries + canonical Tool Set adapted to the floor
contract. Same bytes out; every loss becomes a diagnostic; every family
renders from one call list.

Design (from TransIR §2): new `backend/internal/convert/ir.go` with
`SurfaceFormat` (chat|anthropic|responses), `IRToolDef`
(ClientName/WireName/Kind/ExecBy/Params/Origin),
`IRToolChoice`, `IRToolSet` (Defs/Choice/Parallel/Forward/Reverse),
`IRPart`/`IRMessage`, `IRReasoning`, `IRDiagnostic`, `LossPolicy`
(allow|safe|strict), `IRRequest`, `IRCall`. Request leg:
Decode → Detect (unchanged `detectFamilyBody`,
`tools_floor.go:161-169`, OMP-first) → Own (move `ToUpstream` ownership off
JSON onto the IR) → Substitute+Floor (emit `Origin` + diagnostics) → Emit
(single emitter; `topUpCliTools` becomes an assertion) → Gate (`RejectLoss`,
default allow = byte-identical). Response leg:
Assemble → IRCalls → Render → Restore → Encode; `FromUpstreamChunk`
(`toolmap_response.go:10-63`) parses wire calls into `IRCall`, family
renderers map `[]IRCall` to client dispatches (1:1, 1:N fan-out, text
fallback), `Reverse`-map restore, surface encode.

### 1.1 `IRToolSet` core from `toolconv.Set` (value H / effort M) — biggest bet

- Target: `backend/internal/convert/ir.go` (new); adapt, don't copy.
- Precedent: relaykit `toolconv.Set{Source, Definitions, Choice,
  ParallelAllowed, NativeToolConfig, History}` with `Set.Empty()`
  (`reference/new-api/relaykit/relayconvert/internal/toolconv/model.go:88-99`),
  `Definition{Kind, Execution, Function}` (`:57-66`), `Kind`
  (`:11-22`), `Execution` (`:24-29`); symmetric
  `ExtractRequest`/`AttachRequest`
  (`decode.go:16-29`, `encode.go:16-48`); keep legacy-`functions` handling
  (`decode.go:43-55`).
- Acceptance: implicit `ToUpstream → SubstituteCanonicalDefinitions →
  floorOnlyOMP` JSON-mutation contracts become explicit `IRToolSet`
  construction; `ToolMapper` keeps a compat view so `relayStats.toolMap`
  threading is untouched; golden corpus byte-identical (see 1.7).
- Invariants carried: §2.1–2.6 (floor gate, first-wins, 64-char, contract,
  one-mapper, delegation-as-`Kind`/`ExecBy`).
- Evidence: `backend/internal/convert/toolmap_request.go:401,462-539`,
  `tools_normalize.go:77`, `tools_floor.go:204-256`.

### 1.2 Loss diagnostics + policy gate (value H / effort M) — first steal

- Target: `backend/internal/convert/ir.go` (`IRDiagnostic`, `RejectLoss`);
  thread diagnostics into trace/debug surfaces.
- Precedent: `ConversionDiagnostic{Code, Path, Message, Severity, From, To}`
  (`reference/new-api/relaykit/relayconvert/types/conversion.go:15-22`),
  `Severity = warning|error` (`:8-13`), `ConversionLossPolicy =
  safe|strict|allow`, default allow (`:24-37`),
  `RejectConversionLoss` (`:61-75`); partial result still returned alongside
  the error (`tool_loss_policy_test.go:75-82`); known codes
  (`tool_loss_policy_test.go:30,56`).
- Acceptance: every silent drop (OMP kept-drops
  `docs/decisions/tool-name-translation.md:177-187`, `decide` strip
  `backend/internal/upstream/clitools.go:27-30`, scalar drops
  `tools_reshape.go:183-187`, riders) emits
  `IRDiagnostic{Code,Path,Severity,From,To}`; default allow = today's behavior
  byte-identical; safe/strict available per-caller later.
- Evidence: `backend/internal/convert/tools_reshape.go:31-38`,
  `backend/internal/upstream/clitools.go:27-30`.

### 1.3 Decode → Own → Substitute+Floor → Emit restructure (value H / effort M)

- Target: `backend/internal/convert/convert_request.go:165-205`
  (`NormalizeRequestMappedOpts`), `toolmap_request.go`,
  `tools_normalize.go`, `tools_floor.go`.
- Precedent: llmrelay "two codecs per dialect"
  (`reference/llmrelay-relay/internal/translate/openai_request.go:1-4`); one
  decoder per surface (chat/anthropic/responses); single IR→wire emitter.
- Acceptance: family detection unchanged (`detectFamilyBody`,
  `tools_floor.go:161-169`; `isOMPToolset :171-202`; pi
  `isPiToolset :82-103`, `hasPiEditFingerprint :105-141`,
  `detectClientFamily :143-156`); ownership logic moved off JSON onto the IR;
  `topUpCliTools` asserts emit output ⋃ floor.
- Evidence: `convert_request.go:70-150,165-205`; `tools_floor.go:161-287`.

### 1.4 Family renderers over `[]IRCall` (value H / effort M)

- Target: `backend/internal/convert/tools_reshape.go:45-102,121-288,543-776`,
  `tools_textfallback.go`, `piFloorRenderers`.
- Precedent: vercel part renderers; roo `processAiSdkStreamPart`
  (`reference/roo-code/src/api/transform/ai-sdk.ts:196-282`); relaykit "add a
  From/To spec" (`request_registry.go:89-142`); llmrelay "add two codecs".
- Acceptance: one `[]IRCall`, three renderers — OMP (fan-out
  `read_files{paths}`→1×`read`, `str_replace{replacements}`→N×`edit`,
  offset/limit dropped, `find`/`glob` shape-aware + text fallback for the 4
  unroutable floor names) and pi (per-path `read` keeping offset/limit, ONE
  batch `edit{edits[]}`, `bash{command,timeout}`) behave byte-identically;
  Advisor (Codex/Hermes/OpenClaw shapes) arrives as
  `detectAdvisorToolset` + `advisorReshapeWires` + renderer, zero new wire
  names, gate green.
- Evidence: `tools_reshape.go:104-117,121-288,371-461,543-776`;
  `docs/OMP-TRANSLATION.md:117-158,244-305`.

### 1.5 Stream state machine for Anthropic/Responses (value H / effort L)

- Target: `backend/internal/convert/sse.go:157`, `accumulator.go:33-98`,
  `options.go:8-41,60-69`, `foreign_signals.go:332`;
  `reshapeBuffer`/`reshapeFlush` generalized.
- Precedent: `ResponseStreamState{specs, stepStates, usage, diagnostics,
  pendingDiagnostics, seenDiagnostics, fallbackInfo}`
  (`reference/new-api/relaykit/relayconvert/response_registry.go:95-109`),
  chunk/finalize fns (`:28,30`), `NewResponseStreamState(ByID)`
  (`:297,318`), options (`:76-85`); roo "exactly one state owner per stream"
  (`transform/stream.ts:97-101`; name-required-to-start
  `providers/openai-codex.ts:949-951`, `providers/openai-native.ts:1229-1231`).
- Acceptance: closes the explicitly deferred gap
  (`docs/decisions/tool-name-translation.md:194,238`,
  `docs/UNIVERSAL-TOOLS-AUDIT.md:207-210`): Anthropic/Responses streaming gets
  the same withhold→reshape→fanout→fallback machine the chat relay has; chat
  bytes unchanged.
- Evidence: `backend/internal/convert/sse.go:157`;
  `backend/internal/server/openai_chunk_pipeline.go`;
  `docs/OMP-TRANSLATION.md:209-213`.

### 1.6 Portable reasoning Intent (value H/M / effort M)

- Target: `backend/internal/convert/effort.go` (augment, don't delete ladders).
- Precedent: `Intent` (`reference/new-api/relaykit/relayconvert/reasoning/intent.go:75-84`),
  `From*`/`ApplyTo*` per format (`:361-652`), `normalizeIntent`
  (`:149-209`), `MergeExplicitAndSuffix` model-wins (`:211-281`),
  `MergeExplicit` budget+effort coexist (`:283-352`), carried state
  `dto/reasoning_state.go:1-14`, `EffectiveEffort`/`EffortFromBudget`
  (`:654-688`), `ClientError` 4xx class (`:29-53`); litellm per-model
  capability gates (`transformation.py:399-447`, `:268-275`).
- Acceptance: budget+effort coexist through pivots; model-name-wins merge;
  per-model effort ladders move behind capability checks; 4xx-class bad
  controls stay client errors.
- Evidence: `backend/internal/convert/effort.go`;
  `backend/internal/server/conformance_cline_test.go:209-278` (thinking replay).

### 1.7 Golden snapshots + round-trip / cross-dialect suites (value M / effort M)

- Target: `backend/internal/convert/toolmap_corpus_test.go` (extend) +
  `testdata/`.
- Precedent: golden snapshots with `-update` + volatile normalization
  (`reference/new-api/relaykit/relayconvert/golden_test.go:1-73`,
  `boundary_test.go`, `terminal_stream_test.go`); llmrelay
  `roundtrip_test.go`, `cross_dialect_test.go`, `recorded_test.go`,
  `anthropic_stream_test.go`, `anthropic_roundtrip_test.go` + `testdata/`.
- Acceptance: IR→wire→IR per surface + chat→IR→anthropic/responses suites;
  byte-drift detection for all three surfaces (we have one-directional
  client→wire today, `docs/decisions/tool-name-translation.md:88-91`).
- Evidence: `backend/internal/convert/toolmap_corpus_test.go`.

### 1.8 Drop-vs-raise contract + warnings + filters + provenance (value M / effort S)

- Target: convert options + Anthropic/Responses encoders + trace surfaces.
- Precedent: litellm `drop_params` gate + `UnsupportedParamsError` +
  `DROP_*_WARNING` (`transformation.py:277-290,447-471`); vercel
  `stream-start` warnings (`language-model-v4-stream-part.ts:82-90`) + error
  taxonomy module (`errors/` — 17 typed errors); roo `filterNonAnthropicBlocks`
  over `VALID_ANTHROPIC_BLOCK_TYPES` (`anthropic-filter.ts:8-51`); llmrelay
  `LegacyMaxTokens`/`Inbound` (`openai_request.go:27-35`).
- Acceptance: drops get a caller-visible form (start with warning diagnostics
  inside 1.2); per-surface allowlist block filter strips internal blocks
  (reasoning/thought signatures) before encode; IR records field provenance
  per 0.5.
- Evidence: `backend/internal/convert/tools_reshape.go:31-38`,
  `backend/internal/upstream/clitools.go:27-30`,
  `backend/internal/convert/options.go:8-41`.

## 5. Phase 2 — Backend wins (8 items)

Goal: steal the best gateway package patterns into our
`backend/internal/{server,pool,session,store,upstream,registry,modelcat,config,dashboard}`
seams. Our survey: stdlib-mux server (gzip>access+ratelimit>cors>mux),
handlers converge on `chatCore` single-attempt + relay; `pool.Acquire`
spill-order lanes; `session.Manager`; SQLite `store` under one `writeMu`;
typed-refusal `upstream.Client`; `registry` model→agent; `modelcat` Served
gate; `config` `KeyDef` catalog; `dashboard` `AdminRoutes`+manifest
(OurBackend scout).

### 2.1 Adapter unify: one surface per family (value H / effort M)

- Target: `backend/internal/server/` (chat/anthropic/responses relays),
  `backend/internal/upstream/`.
- Precedent: one-api `Adaptor` per channel
  (`reference/one-api/relay/adaptor/interface.go:11-21`, ~40 channel dirs,
  routing via `relaymode/` + `channeltype/`) — take interface discipline, NOT
  the N×direct-mapping sprawl (no IR there; TransIR verdict: anti-pattern);
  llmrelay dumb-adapter/smart-executor split (adapters stay dumb, executor
  injects key via `WithAPIKey` ctx,
  `reference/llmrelay-relay/internal/provider/provider.go:22-32`).
- Acceptance: each client family has one surface (Init/Convert/Do/DoResponse
  shape); cross-family behavior composes through the Phase-1 IR, never
  family-to-family mappings.
- Evidence: `backend/internal/server/openai.go:19-32`,
  `anthropic.go:29-41`.

### 2.2 Breaker + executor with pre-commit stream failover (value H / effort M)

- Target: `backend/internal/pool/` (acquire path), `backend/internal/server/`
  (chatCore + stream relays).
- Precedent: llmrelay per-(provider,model) Breaker (30s window/min-5/0.5
  threshold/30s open/single-probe half-open,
  `internal/reliability/breaker.go:11-16,26-38,42-63,66-94`); `Executor.attempt`
  breaker-gate → key-draw → retry with exp-backoff+jitter/`Retry-After`, 429
  key-rotate-no-backoff (`executor.go:88-144`); `Complete` chain-walk
  (`:159-181`); `Stream` pre-first-content TTFT failover with prefix replay,
  no inbound byte until content (`:183-219,241-300`); jdanzig pre-commit
  failover — commit flag defers `WriteHeader` to first delta
  (`reference/llm-gateway-jdanzig/internal/gateway/server.go:306-315`), error
  before commit → next hop, after commit → terminate honestly (`:332-340`),
  `Streamer` error-before-first-emit contract (`provider.go:47-53`,
  enforced `openai.go:110-142`), per-provider breaker registry gating each hop
  (`server.go:184-189,294-298`, `breaker.go:101-110`).
- Acceptance: breaker gates each attempt; 502s before first byte stay
  retryable onto the next lane/account; streams never cache (jdanzig
  `:135-139` singleflight + `WithoutCancel` on cache miss collapses stampedes
  without leader-disconnect kills); consecutive-failure counter (jdanzig
  `breaker.go:18-77`) vs rolling window chosen explicitly and documented.
- Evidence: `backend/internal/pool/` acquire path; stream relays in
  `backend/internal/server/`.

### 2.3 Multi-key rotation pool (value H / effort M)

- Target: `backend/internal/pool/` (account draw), `backend/internal/session/`.
- Precedent: llmrelay `KeyPool` round-robin skipping cooldown, 30s default,
  nil for <2 keys (`internal/reliability/keypool.go:15,19-25,30-35,39-52,73-80`)
  with 429 rotate-no-backoff (`executor.go:88-144`); jdanzig holds ONE apiKey
  per provider (`openai.go:17-22`, `anthropic.go:17-32`) — multi-key there
  "would be new work".
- Acceptance: per-(model, account) cooldown + round-robin draw skipping
  cooling keys; 429 rotates without backoff; pool exhaustion surfaces as
  today's 429/waiting-room semantics, never a new error shape.
- Evidence: pool spill-order lanes; `session.Manager` claim UUID.

### 2.4 Billing settle/refund machine (value H / effort M)

- Target: `backend/internal/pool/` ledger writes, `backend/internal/store/`,
  `backend/internal/session/` (`EndSession` receipts).
- Precedent: new-api `EstimateRequestToken`
  (`relay/token_counter.go:176`) → `ModelPriceHelper`
  (`relay/helper/price.go`) → `PreConsumeBilling`
  (`service/billing.go:20`) → `NewBillingSession`
  (`billing_session.go:379`) on `RelayInfo.Billing`; success settles via
  `SettleBilling` (`billing.go:51`) → `session.Settle`
  (`billing_session.go:45`, delta=actual−preConsumed) else legacy
  `PostConsumeQuota` (`quota.go:420`); failure refunds via
  `RefundFailedRequestBilling` (`request_billing.go:71`) → `session.Refund`
  (`billing_session.go:86`, idempotent async, no-op if settled) +
  `ChargeViolationFeeIfNeeded` (`violation_fee.go:103`); wrappers
  `PrepareRequestBilling` (`:24`); pipeline `GetAndValidateRequest(:110)` →
  `GenRelayInfo(:121)` → `PrepareBilling(:141)` → retry
  `getChannel(:163)` → Helper by format (`:187-196`) → `DecideRetry(:207)` →
  deferred Refund (`:145`); one-api reserve-then-settle with >100x rich-user
  skip (`relay/controller/helper.go:60-95`, `model/token.go:217,282`,
  `billing.go:11-52`, refund `text.go:73-84`); instawork reserve/reconcile/
  release closing the check-then-charge race (`cost_limit.go:157-257`),
  `CheckAndReserve`/`Adjust`/`Cancel` with `reservedAt` window-attribution
  (`types.go:71-101`).
- Acceptance: EITHER adopt reserve→settle/refund with idempotent guards (settle
  no-op after settle; refund no-op after settle) OR record an ADR keeping
  record-only spend + `DELETE freebucksRefund` replay and stating why
  (invariant §2.8 demands no shadow ledgers either way).
- Evidence: `backend/internal/pool/` ledger; `backend/internal/session/`
  `EndSession`.

### 2.5 Routing policies (value M / effort M)

- Target: `backend/internal/pool/` lane choice, `backend/internal/registry/`,
  `backend/internal/modelcat/`.
- Precedent: llmrelay `router/policies.go + alias.go` — fallback literal chain
  (`:40-59`), cheapest blended-cost/unknown-last (`:64-131`), fastest
  rolling-TTFT/cold-first (`:135-180`), weighted FNV-sticky + fallback tail
  (`:186-254`), `CompileAliases` nesting/cycle rules (`alias.go:22-91`),
  virtual-model `Aliases`+`Inner` (`:137-160`), `Eligibility`
  degraded-tool-replay filter fail-open (`:167-196`); openziti capability-alias
  bypass + construction-time `validateRoutes` failing fast on dead
  rules/phantom fallbacks (`routing.go:79-136,360-406`,
  `handler.go:110-128,204-244`); jdanzig cost metering from canonical counts ×
  price table at record time (`server.go:368-384`).
- Acceptance: lane choice is a named policy (fallback default; cheapest/
  fastest opt-in) with verbatim `Reason` explainability
  (router-returns-chain/executor-walks-chain); alias cycles rejected at load;
  cold-first TTFT + cheapest-unknown-last ranking use stable sorts.
- Evidence: pool `spillOrder`; `registry` model→agent; `modelcat` Served gate.

### 2.6 Store layering: async log + GROUP BY stats (value M / effort M)

- Target: `backend/internal/store/`, `backend/internal/dashboard/`.
- Precedent: llmrelay `store/*.go` — async SQLite log
  (`store.go:82-112,196-223`) → SQL `GROUP BY` stats (`stats.go:27-36`) +
  `SpendByDay`/`LatencyStats`/`RecentDecisions` dashboard queries
  (`dashboard.go:24-32,63-109,136-169`) + Go percentiles
  (`stats.go:65-100`, `dashboard.go:111-117`); async-drop-counted log with
  NULL-cost unpriced flag + reserved training columns; instawork single
  Delta→Lua flush shared via embedded `RecorderBinding`
  (`aggregate.go:26-72`, `binding.go:29`).
- Acceptance: hot path never blocks on stats writes (async + drop counter);
  dashboard cards read pre-aggregated queries, not full scans; unpriced rows
  flagged, not zero-filled.
- Evidence: `backend/internal/store/` (one `writeMu` today);
  `backend/internal/dashboard/dashboard_cards.go` (`sessionQuotaFor`).

### 2.7 Admin rollups (value M / effort S)

- Target: `backend/internal/dashboard/`.
- Precedent: instawork `adminrollup` Store (Redis DB6 `llm:admin:<metric>:today:<day>`
  totals+dims hashes, 48h/26h/35d TTLs, Lua `HINCRBYFLOAT`, TopN caps
  ByKey/ByUser=100, snapmerge max-merge); every stats pkg = in-process RWMutex
  maps + 50-entry recent ring + UTC-day rollover; jdanzig `MemoryRateLimiter`
  windows map / `MemoryCache` TTL map / `MemoryMeter` append+aggregate
  (`memory.go:12-120`), `redis.go`/`postgres.go` behind the same interfaces
  (`store.go:13-49,1-5`).
- Acceptance: per-day/per-hour rollups with TopN caps back the activity views;
  interfaces stay swappable (memory today, same seams later).
- Evidence: `backend/internal/dashboard/` cards + manifest.

### 2.8 Middleware-order audit + transport guards (value M / effort S)

- Target: `backend/internal/server/` mux + middleware chain.
- Precedent: instawork order — AbortLogging → MetaURLRewrite → ClientGzip? →
  VendorPathPolicy → APIKeyValidation → ModelStatus → CostLimit(reserve) →
  IDGate → PIIRedact → TestMode?(triple-guarded) → Logging → RateLimit →
  RedactRateLimit? → CORS → TokenParsing(+cost callback, +unmetered) →
  PIIResponseRestore → Streaming → provider Proxy (`main.go`); jdanzig
  header-override-after-`SetupRequestHeader` so user wins
  (`api_request.go:196-330`); jdanzig mode-switched `DoResponse` on
  RelayMode/IsStream incl Rerank union branch; openziti gateway-binding model
  normalization (overwrite upstream resp/Chunk.Model, log divergence,
  `handler.go:238-244,286-293`) + SSE terminal-event enforcement (missing
  Done/Err = truncation error, `handler.go:301-304`); one-api zero-copy
  OpenAI passthrough when no mapping/prompt override
  (`controller/text.go:91-98`) + shared `DoRequestHelper` + per-vendor
  `SetupRequestHeader` override (`adaptor/common.go:21-52`) + channel-scoped
  ratio keys `name(id)` with global fallback + fail-open defaults
  (`ratio/model.go:695-710,729-741`).
- Acceptance: documented chain order with auth→model→cost→limit→parse→stream
  rationale; user headers win over defaults; terminal SSE events enforced;
  zero-copy fast path where no mapping applies.
- Evidence: `backend/internal/server/` handler chain (gzip>access+ratelimit>
  cors>mux today).

## 6. Phase 3 — Client surfaces (6 items)

Priority (ClientMatrix): OMP + pi to perfect first (P0), then Codex CLI +
ChatGPT-desktop-via-Codex-config (P1), OpenClaw (P2), Hermes/agent clients
(P3), Claude Code opportunistic (P4). Transport/auth/tool-format evidence per
client is in ClientMatrix §§1–6; only gaps + acceptance repeat here.

### 3.1 OMP to perfect (value H / effort M) — P0

- Gaps: (1) streaming Anthropic/Responses fan-out + reshape deferred
  (`docs/decisions/tool-name-translation.md:§4`,
  `docs/UNIVERSAL-TOOLS-AUDIT.md:207-210`) — OMP speaks chat so unblocking,
  but OMP-via-Anthropic/Responses adapters hit it; (2) floor-capability
  reminder is chat-envelope only — Anthropic/Responses relays lack the prompt
  nudge.
- Target: Phase-1 state machine (1.5) + `backend/internal/upstream/chat.go`
  reminder ported per surface.
- Acceptance: OMP tool turns green on all three surfaces, streaming and not;
  strictness pinned: zero riders, byte-canonical defs, `ask` per-question ids,
  `todo` `string[]`, non-empty `tasks[]`, opencode-go item order
  (`docs/OMP-TRANSLATION.md:125,127,171-185`;
  `reference/harnesses/oh-my-pi/WIRE-NOTES.md:28-31`).
- Evidence: `docs/OMP-TRANSLATION.md:239-242` (5/5 live proof baseline);
  `backend/internal/server/openai_chunk_pipeline.go`;
  `backend/internal/server/conformance_pi_omp_test.go`.

### 3.2 pi to perfect (value H / effort S) — P0

- Gaps: same streaming deferral as 3.1; `spawn_agent`-style extensions restore
  verbatim by design (no work).
- Target: pi renderer from 1.4 + withhold buffers keyed on `ResponseRewrite()`
  (`docs/OMP-TRANSLATION.md:301-305`).
- Acceptance: pi core 8 map to wire names (NOT floor-only), extensions ride
  virtualized `mcp__<name>`; read keeps offset/limit, edit is ONE batch
  `{edits:[{oldText,newText}]}`, `bash{command,timeout}`
  (`docs/OMP-TRANSLATION.md:274-288`); unroutable floor → text via
  `piFloorRenderers` + `piRoutable`; completions adapter `tools`-present
  invariant holds (`reference/harnesses/pi/WIRE-NOTES.md:34-36`).
- Evidence: `backend/internal/server/conformance_pi_omp_test.go`,
  `backend/internal/convert/tools_pifamily_test.go`,
  `docs/OMP-TRANSLATION.md:244-305`.

### 3.3 Codex CLI (value H / effort M) — P1

- Gaps: (1) no-auth story — `requireAuth` 401s bare requests
  (`backend/internal/server/middleware_auth.go:89-103`) but tokenless custom
  providers send NO auth header
  (`reference/agents/codex/WIRE-NOTES.md:37-47`); (2) `apply_patch`
  freeform-`input` vs `str_replace` shape unhandled
  (`docs/UNIVERSAL-TOOLS-AUDIT.md:143-153` `?` rows); (3) Codex defs gate risk
  unbisected (needs 0.2 probe).
- Target: auth config guidance or explicit allowlist; Codex arg-shape rules
  (Phase-2 queue); `docs/UNIVERSAL-CLIENTS.md:37` recipe
  (`base_url=HOST:3457/v1`, `wire_api=responses`).
- Acceptance: conformance stays green
  (`backend/internal/server/conformance_codex_test.go:1-17,28-60,89-106`:
  `type` on every SSE JSON, output-item ordering, `response.completed` with
  `response.id` + usage triple, EOF-without-completed = error, `store:false` +
  `include:["reasoning.encrypted_content"]` + `reasoning.effort` round-trip,
  item order + `call_id` pairing); tokenless story documented or allowlisted.
- Evidence: `backend/internal/server/openai.go:21` (`/v1/responses` exists);
  `reference/agents/codex/WIRE-NOTES.md:11-23,61,75,83,126-148`.

### 3.4 ChatGPT desktop via Codex config (value M / effort S) — P1

- Fact: no custom base-URL field in-app; routable surface is the shared Codex
  config (`openai_base_url` / `[model_providers.<id>]`,
  `learn.chatgpt.com` advanced-config docs). "Compat" == Codex-provider
  compat (§3.3) in `~/.codex/config.toml`.
- Target: docs recipe only (no in-app shim possible).
- Acceptance: (1) recipe verified live against custom-provider path; (2) WS
  transport question closed — if the route selects
  `openCodexWebSocketTransport`
  (`reference/harnesses/oh-my-pi/WIRE-NOTES.md:22-23`) we serve SSE only;
  confirm `supports_websockets=false` default negotiates SSE.
- Evidence: ClientMatrix §4; `docs/UNIVERSAL-CLIENTS.md:26-28`.

### 3.5 OpenClaw (value M / effort M) — P2

- Gaps: no OpenClaw conformance exists (conformance covers codex/goose/cline/
  continue only); Responses-route item-ordering needs extension for the
  OpenClaw field set; OpenClaw-native arg shapes sit in the Phase-2 queue.
- Target: `backend/internal/server/conformance_openclaw_test.go` (new, from 0.3
  spec) asserting `instructions`-embedding + strict-keys safety on all three
  surfaces; transport fits 1:1 (`openai-completions|openai-responses|
  anthropic-messages` — custom-providers doc).
- Acceptance: its `compat` table is the test plan — each declared key pinned;
  Bearer auth (`middleware_auth.go:108-143`); trailing usage on chat streams +
  Responses `response.completed` usage (Codex §3 guarantees reused).
- Evidence: `https://docs.openclaw.ai/gateway/config-tools/custom-providers.md`;
  `backend/internal/server/openai.go:23` + `models.go` (`/v1/models` served).

### 3.6 Hermes + Claude Code opportunistic (value M / effort S-M) — P3/P4

- Hermes (P3): generic names-only chat path already serves
  (`docs/decisions/tool-name-translation.md` invariants); remaining:
  `/v1/models` auto-detect live check (`/model custom` queries `/models`),
  Hermes-native arg rules by corpus frequency
  (`docs/UNIVERSAL-TOOLS-AUDIT.md:115-129` triage (a)), reasoning-effort
  passthrough per endpoint. Tolerant client (falls back/wraps/degrades).
- Claude Code via `ANTHROPIC_BASE_URL` (P4, only after P0+P1 green): must
  guarantee every streamed `tool_use` has an `id`, history round-trips
  byte-clean, thinking-signature replay accepted (cf.
  `backend/internal/server/conformance_cline_test.go:209-278`); evidence it
  tolerates third-party gateways poorly
  (`reference/agents/claude-code/CHANGELOG.md:433,436,1838,4192`). Verify with
  a live session, not just conformance.
- Evidence: `reference/agents/hermes-agent/agent/auxiliary_client.py:4858-4860`,
  `agent_init.py:466-478`, `agent_runtime_helpers.py:1279`;
  `backend/internal/server/openai.go:23`.

## 7. Phase 4 — Frontend steals R1–R5 (5 items)

All against the Svelte 5 SPA (hash-routed, embedded dist). Keeps (no action):
query-store layer (`frontend/src/lib/stores/query.js:26-132`) over new-api's
plain QueryClient (`reference/new-api/web/src/lib/query-client.ts:36-65`);
server-persisted page state (`stores/pageState.js:1-145`) over localStorage-only
(`notification-store.ts:42-87`, `status-query.ts:106-126`); session-dead latch
(`stores/session.js:32-47`) over 401-redirect
(`http-client.ts:86-145`); dist flow stays exactly
`frontend/vite.config.js:47-54` → `backend/internal/dashboard/assets_embed.go:11-12`
→ `ServeSPA :29-84` → `Taskfile.yml:9-18` → `verify:full:95-101` freshness gate.

### 4.1 R1 URL-shareable table state (value H / effort S)

- Steal: `reference/new-api/web/src/hooks/use-table-url-state.ts:107-266` +
  zod search schema per route (`routes/_authenticated/keys/index.tsx:22-34`).
- Target: `frontend/src/lib/nav.js:38-76` (hash routing) + one small util +
  call sites in `LiveConsole.svelte:55-86` and `TokenTable` (today: server
  pageState per-operator, not shareable; `Activity.svelte:14-35` in-memory
  cursor + sessionStorage one-shot `:52-59`).
- Acceptance: filterLevel/filterMsg/page/viewMode + Tokens search encoded in
  `location.hash` query on debounce, read on mount; pasted URLs reproduce the
  filtered error view; hash-only, no server route changes (keep
  `vite.config.js:59-91` loopback-only proxy allowlist).

### 4.2 R2 Global page-size memory (value H / effort S — trivial)

- Steal: `PAGE_SIZE_STORAGE_KEY='page-size'`
  (`use-table-url-state.ts:28-47`).
- Target: single `localStorage 'fp-page-size'` read in LiveConsole +
  TokenTable (today: `LiveConsole.svelte:59 pageSize=$state(10)` per-mount,
  forgotten on nav).
- Acceptance: one page-size remembered across all tables (~10 lines).

### 4.3 R3 Stale-status cold render (value M / effort S)

- Steal: `reference/new-api/web/src/lib/status-query.ts:43-126`
  (`STATUS_QUERY_KEY`, `readCachedStatus`/`writeCachedStatus`, first paint
  from cache while fetching).
- Target: `query.js` `remember()`/`ensure()` + `Overview.svelte:30-32`
  (today: `data=$state(null)`/blank skeleton until first 15s poll;
  `App.svelte:248-273 {#key activeTab}` remounts re-skeleton per tab).
- Acceptance: seed overview/tokens from last pageState snapshot (or tiny
  localStorage cache), render immediately with a `stale` badge, replace on
  first poll.

### 4.4 R5 Mutation-toast discipline (value M / effort S)

- Steal: `query-client.ts:50-64` MutationCache/QueryCache `onError` with
  `meta.errorToast` opt-out.
- Target: one `silent` flag on `fetchAPI`/`postAPI` error path
  (`frontend/src/lib/api/client.js:68-115`) or a `toast.js` `notifyOnce`
  helper, adopted uniformly (today: per-component `notifyError` —
  `TracesPanel.svelte:27-36`, `LiveConsole.svelte:92-100` with `lastErrorMsg`
  dedupe — no shared convention; 1s auto-poll double-reports toast + inline).
- Acceptance: panels showing inline errors suppress the global toast
  per-mutation; no double-reporting on auto-poll.

### 4.5 R4 Density + split LiveConsole opportunistically (value M / effort S+M)

- Steal: per-category column-visibility key (`usage-logs-table.tsx:66-71`) +
  `use-table-compact-mode` / `use-media-query` hooks + mobile-card fallback
  (`usage-logs-mobile-card.tsx`) vs their per-feature split
  (`features/usage-logs/components/`, `features/channels/index.tsx:36-39`).
- Target: (a) compact-density toggle on LiveConsole/TokenTable persisted in
  pageState (small, do soon); (b) split 1550-line `LiveConsole.svelte`
  (table+console+filters+pagination) into filter-bar/pagination/row components
  (larger — do only when touching that file anyway; ours already has
  `TokenCard.svelte:1-83` desktop `<tr>` + `TokenCardMobile` separation).
- Acceptance: (a) density toggle survives nav; (b) no big-bang rewrite.
- Note R6 (notification-read persistence): nothing to do —
  `AnnouncementsBanner` + `pageStateNotice` + per-account toast guard
  (`Overview.svelte:186-220`) already cover server-side what new-api's
  `notification-store.ts:22-36` does in localStorage.

## 8. What NOT to do

1. **new-api is AGPL-3.0: study, never copy.** Relaykit patterns
   (registries, `toolconv.Set`, diagnostics, stream state, Intent) are
   re-expressed in our types and our floor contract. No new-api file, struct
   layout, or code block enters this tree. one-api/llmrelay/jdanzig/instawork/
   openziti carry their own licenses — same rule: patterns in, code not.
2. **Roo-Code repo is archived.** `reference/roo-code/src/api/...` is a
   frozen snapshot: take the canonical chunk union (`transform/stream.ts:3-14`)
   and single-parser ownership (`:97-101`) as design precedent only. Do not
   chase its provider-per-file fan-out (~30 handlers) and do not treat it as a
   living API contract.
3. **`reference/responses-proxy/` is empty** (only `.git` at read time) — no
   findings cited, none planned. Re-evaluate only if populated.
4. **No second upstream dialect.** relaykit multi-hop `StepConverters` /
   `ConvertRequestVia` (`request_registry.go:54,178-216`) is rejected: one wire
   target means hops add failure modes for no benefit. Revisit only if a
   second upstream dialect appears.
5. **Withdrawn / refused models are upstream verdicts, not proxy bugs.**
   `fable-5.1` trial-off (400), `gpt-6.1-sol` promo spent (429 reset 04:00Z),
   `glm-5.3-flash` 502 `upstream_unavailable` on empty stream, `bunny`
   superseded seat (409 mid-bench, retry re-admits) — no proxy fix exists or is
   planned. Only fresh official-client seats restore refunded purchases
   (PR #77 catalog verdict `1f686bd3`: handle-mode admission does NOT restore
   refunded purchases; pool-wide wipe stands at 8/9 models released).
6. **one-api Adaptor sprawl is the anti-pattern.** ~40 channel dirs with no IR
   is the exact N×direct-mapping trap Phase 1 avoids. Interface discipline
   only.
7. **No RBAC import.** new-api auth-store roles / `ROLE.SUPER_ADMIN` gating
   (`auth-store.ts:24-58`, `channels/index.tsx:43-51`) stays out — our
   single-admin-token model (`session.js` + `client.js` csrfHeader) is
   intentionally simpler.
8. **No new wire tools, no BYOK lanes, no gemini shim** (reaffirmed §1).
9. **Speed truth stays wall tok/s.** Report wall tok/s out/(TTFT+decode), not
   drain; OMP tok/s mixes TTFT+render on short outputs. Baselines in memory
   are volatile — re-measure when asked.

## 9. Sequencing

1. **Nearest-term lane (docs + zero-risk UI):** Phase 0 probes (0.2, 0.3) +
   R1/R2 (4.1, 4.2). Unblocks Phase 1 renderers and Phase 3 arg rules with
   evidence; no wire risk.
2. **Biggest bet:** Phase 1 IR core (1.1–1.4) + diagnostics (1.2) + stream
   parity (1.5). Everything else composes through it.
3. **Then:** P0 clients (3.1, 3.2) → backend hardening in pool order
   (2.2 breaker → 2.3 rotation → 2.4 billing decision → 2.5 policies → 2.6/2.7
   store+rollups → 2.8 middleware) → P1/P2 clients (3.3–3.5) → R3–R5
   (4.3–4.5) → opportunistic P3/P4 (3.6).
4. **Gating:** each phase's acceptance is observable (bytes, verdicts, or
   green suites) — no phase starts on its successor's speculation. Backend
   serves committed `dist` — rebuild after any `frontend/src` edit
   (`Taskfile.yml:9-18`, `verify:full:95-101`).
