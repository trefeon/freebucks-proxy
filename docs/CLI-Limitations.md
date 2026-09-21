# CLI → Proxy Limitations (CLI-Limitations)

Static audit of the upstream CLI behaviors in scope vs the proxy port.
Audit pin: the citations below were written against `e2b911eca` (= live npm
`0.0.178` at the time). Proxy: `main` @ `e9427683`.

> Pin note (2026-09-21): the current recorded pins are
> `backend/internal/wirefacts/testdata/wire/snapshots.json:2-3`
> (`upstream_sha 2b165f749…`, `vendor_version 0.0.180`) and
> `scripts/vendor-version.txt:1` (`0.0.180`). The local vendor clone has moved
> 15 commits past that pin to `8ed5d3e5e` while the npm wrapper still reads
> `0.0.180` at both ends, so the tag no longer identifies a revision — cite the
> SHA. **Drift exists**: the manifest's `freebuff-model-selector.tsx` hash no
> longer matches the clone tip. The upstream half has **not** been re-run; the
> delta, and which files audited below actually changed, is recorded in
> `UPSTREAM-CLI.md` §14 (`Version delta 0.0.178 → 0.0.180`) and §14.6 (the 15
> commits after it).
>
> Proxy-half re-verify (2026-09-22): every proxy citation below was re-pointed
> against `main` @ `051f303c`. The two P0 arms had landed by then, so rows 1-3
> moved to PORTED and the P1 proxy line numbers are the corrected ones.

- Verdicts: **PORTED** / **GAP-P0** (breaks interop — harness retry-spin or
  wrong-operator billing) / **GAP-P1** (parity gap, degraded UX, no spin) /
  **WONT-PORT-BY-DESIGN** (presentation, timing cosmetics, credential-write,
  owner-liveness mechanism, ads rendering, build recipe).
- **Both former P0 arms have since landed** in the owning lane
  (TransLayerPort: `backend/internal/upstream/*` +
  `backend/internal/server/*`), so P0-1/P0-2 below record the shipped
  implementation instead of a spec. The P1 items are still specifications.
- Counts over table rows 1-28 plus W1-W9 (37 entries): **P0 = 0**,
  **P1 = 5**, P2 = 2, WONT = 9, PORTED = 21 (rows 24/25/28 are PORTED+).

Harness key: aider / cline / codex / continue / goose / kilocode / opencode /
pi / omp / qwen / roo. "ALL" = every harness consuming the OpenAI
(`/v1/chat/completions`, `/v1/responses`) or Anthropic (`/v1/messages`)
error envelopes.

## P0 — exact arm required (TransLayerPort) — both arms have since landed

### P0-1 — `403 free_mode_unavailable` (+ `anonymous_network` variant) — PORTED

- CLI: `cli/src/utils/error-handling.ts:59-65`
  (`isFreeModeUnavailableError`: `statusCode === 403` + `error ===
  'free_mode_unavailable'`); variant message at `:200-211`
  (`getFreeModeUnavailableErrorMessage`: `countryBlockReason ===
  'anonymous_network'` → "cannot be used from VPN/Tor/… traffic" via
  `formatFreebuffHardBlockedPrivacySignals`); block extraction at `:173-198`
  (`getCountryBlockFromFreeModeError`: countryCode / countryBlockReason /
  ipPrivacySignals); chat-gate recovery at
  `cli/src/hooks/use-freebuff-session.ts:411-431`
  (`markFreebuffSessionCountryBlocked`: flip to terminal `country_blocked` +
  best-effort `releaseSlot()` DELETE).
- Proxy: **PORTED**. `backend/internal/upstream/wirecodes_gen.go:100-102`
  defines `WireCodeFreeModeUnavailable = "free_mode_unavailable"`; the
  classifier arm at `backend/internal/upstream/classify.go:131-138` matches
  the 403 marker and builds a typed `FreeModeUnavailableError` whose parser
  extracts `countryCode` / `countryBlockReason` / `ipPrivacySignals`
  best-effort (`classify.go:587-618`, the same shape as
  `countryBlockFromBody`, `classify.go:561-585`). The server surfaces
  `403 free_mode_unavailable` with **no Retry-After**
  (`backend/internal/server/errors.go:392-399`) and a terminal hint covering
  the `anonymous_network` remediation plus `recent_limited_country`
  (`backend/internal/server/error_taxonomy.go:41-42`). The arm is terminal by
  construction — no cooldown is ever scheduled, and the same marker on any
  other status falls through to the generic arms (`classify.go:131-138`).
- Harness impact: **none** — the fence is a terminal `403
  free_mode_unavailable` with no Retry-After to drum, and the hint names the
  `anonymous_network` remediation; no retry-spin.

### P0-2 — provider-billing failure → usage mapping — PORTED

- CLI: `error-handling.ts:164-171` (`isFreebuffProviderUsageError`:
  `statusCode === 402` **or** `FREEBUFF_PROVIDER_USAGE_ERROR_PATTERN` on the
  message); pattern + operator copy at
  `common/src/constants/freebuff-errors.ts:2-7`
  (`/(not enough|insufficient|out of) credits?|(add|refill|top up) …
  credits?/i` → `"Freebuff ran out of provider usage and needs a refill.
  This is on us, not your account."`); chat-gate mapping at
  `cli/src/hooks/helpers/send-message.ts:404-408,530-533` (operator problem,
  never the credit-purchase flow).
- Proxy: **PORTED**. `backend/internal/upstream/classify.go:88-96` matches
  the vendor billing wording on **401 and 402** (upstreams disagree on
  status) with `reProviderUsageBill` (`classify.go:690-694`, the ported
  `FREEBUFF_PROVIDER_USAGE_ERROR_PATTERN`) *before* the blanket 402 arm, and
  builds a distinct `ProviderUsageError` (`classify.go:620-625`). The server
  surfaces `402 provider_usage_exhausted` with the upstream body verbatim, no
  Retry-After, and an operator-side hint that says explicitly not to buy
  credits (`backend/internal/server/errors.go:400-411`,
  `backend/internal/server/error_taxonomy.go:43-44`). A 402 *without* billing
  wording still maps to `CreditsError` → `402 out_of_credits`
  (`classify.go:101-102`, `errors.go:431-437`), and the two codes bucket
  separately in the trace column
  (`error_taxonomy.go:125-128,166-169`).
- Harness impact: **none** — an operator-side billing outage no longer reads
  as the caller's credit problem, so no harness escalates buy-credits UX.

## P1 — parity gaps (no retry-spin, degraded UX)

### P1-1 — setup writers missing for roo / cline / goose / qwen / kilocode / pi / omp

- CLI-side need: per-client config writers so each harness points at the proxy.
- Proxy: `backend/internal/cli/setup/setup.go:19-134` (`Run`) covers only
  **Continue** (`:41-70`), **opencode** (`:72-89`), **aider** (`:91-103`).
  No writers for roo, cline, goose, qwen, kilocode, pi, omp (nor codex);
  `:105-124` is the TODO recording why they stay manual (roo/cline/kilocode
  need a JSONC-preserving merge plus a keychain secrets story, goose/qwen
  schemas are unpinned in-repo, pi/omp live outside this repo).
- Harness impact: **roo / cline / goose / qwen / kilocode / pi / omp degrade**
  (manual config, drift-prone); continue / opencode / aider OK.

### P1-2 — no owner-liveness multi-instance takeover

- CLI: `cli/src/utils/freebuff-instance-owner.ts:45-66`
  (`recordFreebuffInstanceOwner` pid-file + `isFreebuffInstanceOwnedByDeadLocalProcess`
  via `kill(pid, 0)`); takeover UX at `send-message.ts:620-627`
  ("Another freebuff CLI took over this account…") with poll-stop
  (`markFreebuffSessionSuperseded`, `use-freebuff-session.ts:96`).
- Proxy: deliberately surfaces `503 session_superseded` and never
  auto-re-acquires in-request (`classify.go:195-210`, `errors.go:285-294`).
  Correct for a multi-tenant server, but there is no equivalent of
  dead-owner detection → two competing instances 503-loop with no
  take-over-or-stand-down resolution.
- Harness impact: **ALL degrade** when two instances share one token
  (common with omp/pi multi-session use); manual kill-the-other required.
- Note: the pid-*file mechanism itself is WONT (single-user CLI concept);
  only the *takeover resolution* behavior is P1.

### P1-3 — no `model_locked` auto-repick (GET → DELETE → POST)

- CLI: `use-freebuff-session.ts:676-720` — on deliberate-pick `model_locked`,
  GET the held row, DELETE it (`releaseSlot`), re-POST on the requested
  model; background-race locks revert silently instead.
- Proxy: `model_locked` is folded into `ErrSessionInvalid`
  (`classify.go:206-210`), but the server does **not** surface a bare `502
  session_invalid`: it maps the marker to `409 model_locked` with the
  re-pick copy and no Retry-After (`errors.go:325-334`). There is still no
  GET→DELETE→POST repick — the refusal is terminal for the request and only
  the admission path released and retried once (`errors.go:328-331`), so a
  stale row on another model still costs a failed turn.
- Harness impact: **ALL degrade** after a model switch against a stale row
  (extra failed turn + manual retry); aider/continue users see a `409` that
  tells them to end the session instead of a silent switch.

### P1-4 — country-block best-effort DELETE

- CLI: `markFreebuffSessionCountryBlocked`
  (`use-freebuff-session.ts:411-431`): terminal-state flip **plus**
  best-effort `releaseSlot()` DELETE so the server does not hold a row it
  already refuses to serve at chat time.
- Proxy: `country_blocked` → `403 country_blocked` (`classify.go:146-147`,
  `errors.go:383-385`) with cache invalidation, but no best-effort upstream
  DELETE on this path — the row lingers server-side until natural expiry.
- Harness impact: **ALL mildly degrade** (server-side slot held during a
  fence); with the P0-1 403 arm landed, this lingering row is the remaining
  country-block defect — the same best-effort release path should be reused.

### P1-5 — sponsored-run timers

- CLI: `cli/src/utils/sponsored-run.ts:437,1281-1285` (report-retry timer,
  terminal-report flush chain) + `exit-cleanly.ts:60-63,77-80`
  (`settleSponsoredRun` raced into remote cleanup with user-visible notice).
- Proxy: no sponsored-run concept; interrupted runs have no terminal-state
  settlement timer/flush equivalent.
- Harness impact: **none today** (no harness drives sponsored runs through
  the proxy); P1 only if sponsored runs ever route here. Downgrade to WONT
  if the product decision is CLI-only sponsored runs.

## P2 — minor

- **P2-1 — upstream-credits (`freebucks*`) countdown/presentation copy**:
  reset countdown (`freebucks.ts:190-203`), header line (`:122-145`), intro
  (`:175-184`) — Web/Desktop/CLI picker UX. (The picker notice this row cited
  before was deleted upstream at `2b165f749` — `UPSTREAM-CLI.md` §14.5; no such
  notice exists in the current `freebucks.ts`.) Proxy
  correctly derives everything off the wire (no hardcoded prices,
  `freebucks.ts:14-21` constraint honored via `session_parse.go`); only the
  human countdown/copy rendering is absent. Impact: dashboard shows raw
  balances — cosmetic.
- **P2-2 — poll-cadence exactness**: CLI `polling-backoff.ts:15-59` (equal
  jitter lower-half backoff, symmetric success jitter, 20s→300s caps).
  Proxy mirrors it (`pool/pool_lifecycle.go:26-40`, `sessionPoll*`
  constants) with its own jitter spelling — fleet-spread behavior
  equivalent, constants not byte-identical. Impact: none observed.

## WONT-PORT-BY-DESIGN (rationale)

| # | CLI behavior | CLI source | Proxy counterpart / rationale |
|---|--------------|------------|-------------------------------|
| W1 | TUI picker, landing screen, banners, streak line | `freebuff-model-selector.tsx`, `freebuff-landing-screen.tsx:791-801`, `freebuff-streak-line.ts` | Gateway-correct rule: presentation never ports. Dashboard owns its own UX. No harness impact (wire unchanged). |
| W2 | Ads rendering | `lazy-response-ads.ts`, `response-ad-positions.ts` | Ad-chain *gates* port (`waiting_room_required`, `classify.go:129-139`); ad *rendering* is client UX. No harness impact. |
| W3 | Credential **write** to user CLI files (0600/0700) | `auth.ts:198-252` (`saveUserCredentials`, `clearUserCredentials`, `CREDENTIALS_FILE_MODE`) | Proxy never writes `~/.config/manicode/credentials.json`. Read path PORTED (`clicreds.go:57-71`); proxy-owned `.env` writes via `-refresh-token` (`refreshtoken.go:35-66`) are a different trust domain. No harness impact. |
| W4 | Owner-liveness pid-file mechanism | `freebuff-instance-owner.ts:16-43` | Single-user mechanism; meaningless for a multi-token server. Behavior gap tracked as P1-2 instead. |
| W5 | Analytics / engagement / log-shipper flush on exit | `exit-cleanly.ts:74-80` | No telemetry-sidecar concept in the gateway. No harness impact. |
| W6 | Renderer/terminal cleanup, alternate-screen notice | `exit-cleanly.ts:39-41,64-67,104-113` | No terminal in a server process. No harness impact. |
| W7 | First-tab-discount / wallet-consent *UX copy* | `use-freebuff-session.ts:630-651` | Wire headers PORTED (`session.go` POST headers, `freebuff-session-api.ts:163-179`); user-facing consent strings are client UX. No harness impact. |
| W8 | Strict tool-calling UX | n/a (CLI has no strict concept) | Proxy-side harness hardening (`strict_tools.go`, gates in `openai.go:77`, `responses.go:94`, `anthropic.go:98`, `anthropic_json.go:54`) *exceeds* the CLI — PORTED+ by design. Protects Hermes/OpenClaw loose schemas. |
| W9 | Build recipe / release plumbing | CLI package scripts | Out of scope for wire behavior. No harness impact. |

## Verdict table (full)

| # | CLI behavior | CLI source | Proxy source | Verdict | Harness impact |
|---|--------------|------------|--------------|---------|----------------|
| 1 | `403 free_mode_unavailable` gate incl. country fields | `error-handling.ts:59-65,173-198` | `wirecodes_gen.go:100-102`, `classify.go:131-138,587-618`, `errors.go:392-399` | PORTED | — |
| 2 | `anonymous_network` variant message (disable VPN/Tor) | `error-handling.ts:200-211` | `classify.go:591-610` (`countryBlockReason`/`ipPrivacySignals`), `error_taxonomy.go:41-42` (VPN/Tor remediation) | PORTED (variant of 1) | — |
| 3 | Provider-billing → operator-usage mapping | `error-handling.ts:164-171`, `freebuff-errors.ts:2-7`, `send-message.ts:404-408,530-533` | `classify.go:88-96,690-694`, `errors.go:400-411` (`provider_usage_exhausted`) | PORTED | — |
| 4 | Setup writers roo/cline/goose/qwen/kilocode/pi/omp | — (absent) | `setup.go:41-103` (only continue/opencode/aider), `:105-124` (TODO: rest stay manual) | **GAP-P1** | roo/cline/goose/qwen/kilocode/pi/omp manual-config |
| 5 | Dead-owner takeover resolution | `freebuff-instance-owner.ts:45-66`, `send-message.ts:620-627` | `classify.go:195-210`, `errors.go:285-294` (terminal 503, no resolution) | **GAP-P1** | ALL w/ shared token (omp/pi multi-session) |
| 6 | `model_locked` GET→DELETE→POST auto-repick | `use-freebuff-session.ts:676-720` | `classify.go:206-210` → `errors.go:325-334` (409, no repick) | **GAP-P1** | ALL (failed turn after model switch) |
| 7 | Country-block best-effort DELETE | `use-freebuff-session.ts:411-431` | `classify.go:146-147`, `errors.go:383-385` (no DELETE) | **GAP-P1** | ALL mild (lingering server row) |
| 8 | Sponsored-run settle timers | `sponsored-run.ts:437,1281-1285`, `exit-cleanly.ts:60-63` | — | **GAP-P1** (→WONT if CLI-only) | none today |
| 9 | Freebucks countdown/copy | `freebucks.ts:122-203` | `session_parse.go:76-82,224-228` (data only) | P2 | cosmetic |
| 10 | Backoff exact constants | `polling-backoff.ts:15-59` | `pool_lifecycle.go:26-40` | P2 | none |
| 11 | `turn_spend_limit` terminal 429, no Retry-After | `error-handling.ts:125-139`, `freebuff-errors.ts:11-14` | `classify.go:79-87`, `errors.go:179-194` | PORTED | — |
| 12 | Session envelope: POST model header, GET/DELETE instance header, compact, timezone, first-tab-discount | `freebuff-session-api.ts:163-179` | `session.go:89-92,117-123,164-165,281-283` | PORTED | — |
| 13 | 404→`none`; 403 `country_blocked`/`banned` passthrough; 409 `model_locked` family; 429 `rate_limited` family as data | `freebuff-session-api.ts:149-193` | `classify.go:36-53,146-147,206-210,234-235`; 404→`none` in `session.go:171-176` | PORTED | — |
| 14 | POST retry only on 408/429/503; GET retries transient | `freebuff-session-api.ts:48-73` | `client_chat.go:43-45,107-111` | PORTED | — |
| 15 | Poll cadence 30s±20%, fail backoff 20s→300s + Retry-After floor | `polling-backoff.ts`, `use-freebuff-session.ts:76-111` | `pool_lifecycle.go:26-40,98-102,320-324` | PORTED | — |
| 16 | Config-dir resolution (+ absolute guard, env suffix) | `config-dir.ts:16-36` | `clicreds.go:16-41` | PORTED | — |
| 17 | Credential read: `default` profile, `authToken` field only | `auth.ts:75-96,136-150` | `clicreds.go:43-71` | PORTED | — |
| 18 | Headless login (code-in-URL + status poll, plus protocol login) | `cli/src/login/login-flow.ts` (via `/api/auth/cli/*`) | `auth_login.go:1-58`, `refreshtoken.go:1-66` | PORTED | — |
| 19 | Session DELETE + refund receipt + 404 tolerance | `freebuff-session-store.ts:90-93`, `freebuff-session-api.ts:122-124` | `session.go:264-273`, `session_parse.go:447-451` | PORTED | — |
| 20 | Chat-gate recovery (`session_expired`/`waiting_room_*`/`superseded`) | `send-message.ts:580-631` | `errors.go:269-295,313-339` (+ pool re-admit) | PORTED | — |
| 21 | `402` out-of-credits (user-billing shape) | `error-handling.ts:43-53`, `:243` | `classify.go:101-102`, `errors.go:431-437` | PORTED (copy differs, semantics same) | — |
| 22 | `free_mode_cli_required` / `invalid_agent_hierarchy` 403 | `freebuff-models.ts` gate codes | `classify.go:102-110`, `errors.go:371-376` | PORTED | — |
| 23 | Waiting-room queued/required, superseded, fanout, capacity-deferred, ip_capped, load-shedding, peak-hours, no-endpoints, outside-hours | `freebuff-session.ts` gate codes | `classify.go` full matrix + `errors.go:191-341` | PORTED | — |
| 24 | `session_limit_reached` excluded from CLI recovery (Desktop cap) | `error-handling.ts:213-241` | `errors.go:238-245` (distinct 409, never session-invalid) | PORTED+ (superset) | — |
| 25 | Strict gates before conversion, all 3 surfaces | n/a (proxy-only) | `strict_tools.go`, `openai.go:77`, `responses.go:94`, `anthropic.go:98`, `anthropic_json.go:54` | PORTED+ | Hermes/OpenClaw protected |
| 26 | Exit: end session (DELETE) on quit | `exit-cleanly.ts:81-83,102` | shutdown persist / `LeaseRelease` path | PORTED (concept) | — |
| 27 | Freebucks read-off-wire, presence-is-gate, no client role check | `freebucks.ts:46-58` | `session_parse.go` freebucks block | PORTED (concept) | — |
| 28 | Proxy-only startup flag surface (`-setup`, `-refresh-token`, `-doctor`, …) | n/a — upstream exposes only `--continue`/`--cwd`/`--trust-agents` + `login` (`cli-args.ts:52-73`); no such flags exist | `backend/cmd/freebucks-proxy/main.go:98-111` (flags), `:119-158` (dispatch) | PORTED+ (proxy-only) | — |
| 29-37 | W1–W9 wont-port rows | see WONT table | — | WONT | none |
