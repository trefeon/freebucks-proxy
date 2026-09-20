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
> longer matches the clone tip. This audit has **not** been re-run; the delta,
> and which files audited below actually changed, is recorded in
> `UPSTREAM-CLI.md` §14 (`Version delta 0.0.178 → 0.0.180`) and §14.6 (the 15
> commits after it).

- Verdicts: **PORTED** / **GAP-P0** (breaks interop — harness retry-spin or
  wrong-operator billing) / **GAP-P1** (parity gap, degraded UX, no spin) /
  **WONT-PORT-BY-DESIGN** (presentation, timing cosmetics, credential-write,
  owner-liveness mechanism, ads rendering, build recipe).
- **No Go behavior edits were made for this doc.** P0/P1 arms are specified
  for the owning lane (TransLayerPort); TransLayerPort owns
  `backend/internal/upstream/*` + `backend/internal/server/*`.
- Counts: **P0 = 2** (one with an anonymous-network variant), **P1 = 5**,
  P2 = 2, WONT = 9, PORTED = 18.

Harness key: aider / cline / codex / continue / goose / kilocode / opencode /
pi / omp / qwen / roo. "ALL" = every harness consuming the OpenAI
(`/v1/chat/completions`, `/v1/responses`) or Anthropic (`/v1/messages`)
error envelopes.

## P0 — exact arm required (TransLayerPort)

### P0-1 — `403 free_mode_unavailable` (+ `anonymous_network` variant) unmapped

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
- Proxy: **no arm anywhere**. `backend/internal/upstream/wirecodes_gen.go`
  has no `free_mode_unavailable` entry (only `country_blocked:111` /
  `WireCodeCountryBlocked`, which is the *session-status* shape, not this
  chat-`403` error-code shape); `backend/internal/upstream/classify.go:24-212`
  has no `free_mode_unavailable` match; `backend/internal/server/errors.go`
  has no branch → falls to `errors.go:342-358` generic `UpstreamError`
  (`502 upstream_unavailable`). A geo-fenced account therefore reads as a
  flap-and-retry 502 instead of a terminal 403, and harnesses retry-spin a
  fence that never clears. (Precedent for the fix shape: the `cli_required`
  arm at `errors.go:371-373`.)
- Required arm:
  1. `wirecodes_gen.go` (+ generator source): new
     `WireCodeFreeModeUnavailable = "free_mode_unavailable"`.
  2. `classify.go`: `status == 403 && contains(lower,
     "free_mode_unavailable")` → new typed error carrying
     `countryCode` / `countryBlockReason` / `ipPrivacySignals`
     best-effort (mirror `countryBlockFromBody`, `classify.go:526-544`),
     terminal (no cooldown — a retry cannot move the account).
  3. `server/errors.go` (reuse `error_taxonomy.go` only): `errors.As` branch
     → `403` + code `free_mode_unavailable` (+ `anonymous_network` variant
     message mirroring `error-handling.ts:205-209`), **no Retry-After**
     (same terminal shape as `turn_spend_limited`, `errors.go:175-190`).
- Harness impact: **ALL break** — every harness loops 502-retry on a
  permanent geo fence; VPN/Tor users (anonymous_network) get no actionable
  "disable it" message.

### P0-2 — provider-billing failure → usage mapping unmapped

- CLI: `error-handling.ts:164-171` (`isFreebuffProviderUsageError`:
  `statusCode === 402` **or** `FREEBUFF_PROVIDER_USAGE_ERROR_PATTERN` on the
  message); pattern + operator copy at
  `common/src/constants/freebuff-errors.ts:2-7`
  (`/(not enough|insufficient|out of) credits?|(add|refill|top up) …
  credits?/i` → `"Freebuff ran out of provider usage and needs a refill.
  This is on us, not your account."`); chat-gate mapping at
  `cli/src/hooks/helpers/send-message.ts:404-408,530-533` (operator problem,
  never the credit-purchase flow).
- Proxy: `classify.go:85-86` maps **every** 402 → `CreditsError`;
  `errors.go:377-384` surfaces `402 out_of_credits` with the upstream body
  verbatim. A provider-side billing outage (CrofAI/OpenRouter-shaped, 401 or
  402 with credit wording) therefore tells the *user* to buy credits for an
  *operator* problem — wrong actor, wrong runbook, and BYOK-adjacent
  harnesses may auto-escalate billing UX.
- Required arm:
  1. `classify.go`: before the blanket 402 arm, match the vendor
     `FREEBUFF_PROVIDER_USAGE_ERROR_PATTERN` against the body (both 401 and
     402 — upstreams disagree on status, `error-handling.ts:159-163`) →
     distinct typed error / `RateLimitError.Status`-style marker (never
     `CreditsError`, never a cooldown).
  2. `server/errors.go`: surface the vendor `FREEBUFF_PROVIDER_USAGE_MESSAGE`
     verbatim with a distinct code (e.g. `provider_usage_exhausted`), status
     402 (keeps the billing family) but **not** `out_of_credits` and not the
     purchase hint (`defaultHintForCode`, `error_taxonomy.go:34-82`, must not
     attach a buy-credits hint to this code).
- Harness impact: **ALL degrade** — users shown "add credits" for an operator
  outage; codex/cline-style harnesses with billing UX surface the wrong
  remediation.

## P1 — parity gaps (no retry-spin, degraded UX)

### P1-1 — setup writers missing for roo / cline / goose / qwen / kilocode / pi / omp

- CLI-side need: per-client config writers so each harness points at the proxy.
- Proxy: `backend/internal/cli/setup/setup.go:19-113` (`Run`) covers only
  **Continue** (`:41-70`), **opencode** (`:72-89`), **aider** (`:91-103`).
  No writers for roo, cline, goose, qwen, kilocode, pi, omp (nor codex).
- Harness impact: **roo / cline / goose / qwen / kilocode / pi / omp degrade**
  (manual config, drift-prone); continue / opencode / aider OK.

### P1-2 — no owner-liveness multi-instance takeover

- CLI: `cli/src/utils/freebuff-instance-owner.ts:45-66`
  (`recordFreebuffInstanceOwner` pid-file + `isFreebuffInstanceOwnedByDeadLocalProcess`
  via `kill(pid, 0)`); takeover UX at `send-message.ts:620-627`
  ("Another freebuff CLI took over this account…") with poll-stop
  (`markFreebuffSessionSuperseded`, `use-freebuff-session.ts:96`).
- Proxy: deliberately surfaces `503 session_superseded` and never
  auto-re-acquires in-request (`classify.go:160-170`, `errors.go:281-291`).
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
- Proxy: `model_locked` → `ErrSessionInvalid` (`classify.go:171-175`) →
  `502 session_invalid` (`errors.go:309-324`). No GET→DELETE→POST repick; a
  stale row on another model wedges the request until the row dies
  naturally.
- Harness impact: **ALL degrade** after a model switch against a stale row
  (extra failed turn + manual retry); aider/continue users see a cryptic
  502 instead of a silent switch.

### P1-4 — country-block best-effort DELETE

- CLI: `markFreebuffSessionCountryBlocked`
  (`use-freebuff-session.ts:411-431`): terminal-state flip **plus**
  best-effort `releaseSlot()` DELETE so the server does not hold a row it
  already refuses to serve at chat time.
- Proxy: `country_blocked` → `403 country_blocked` (`classify.go:111-112`,
  `errors.go:368-370`) with cache invalidation, but no best-effort upstream
  DELETE on this path — the row lingers server-side until natural expiry.
- Harness impact: **ALL mildly degrade** (server-side slot held during a
  fence); blocked further once P0-1 lands (same release path should be reused).

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
| W7 | First-tab-discount / wallet-consent *UX copy* | `use-freebuff-session.ts:630-651` | Wire headers PORTED (`session.go` POST headers, `freebuff-session-api.ts:117-133`); user-facing consent strings are client UX. No harness impact. |
| W8 | Strict tool-calling UX | n/a (CLI has no strict concept) | Proxy-side harness hardening (`strict_tools.go`, gates in `openai.go:77`, `responses.go:94`, `anthropic.go:98`, `anthropic_json.go:54`) *exceeds* the CLI — PORTED+ by design. Protects Hermes/OpenClaw loose schemas. |
| W9 | Build recipe / release plumbing | CLI package scripts | Out of scope for wire behavior. No harness impact. |

## Verdict table (full)

| # | CLI behavior | CLI source | Proxy source | Verdict | Harness impact |
|---|--------------|------------|--------------|---------|----------------|
| 1 | `403 free_mode_unavailable` gate incl. country fields | `error-handling.ts:59-65,173-198` | — (falls to `errors.go:342-358` generic 502) | **GAP-P0** | ALL break (retry-spin on permanent fence) |
| 2 | `anonymous_network` variant message (disable VPN/Tor) | `error-handling.ts:200-211` | — (same missing arm) | **GAP-P0** (variant of 1) | ALL break (no actionable message) |
| 3 | Provider-billing → operator-usage mapping | `error-handling.ts:164-171`, `freebuff-errors.ts:2-7`, `send-message.ts:404-408,530-533` | `classify.go:85-86` → `errors.go:377-384` (`out_of_credits`) | **GAP-P0** | ALL degrade (wrong "buy credits" remediation) |
| 4 | Setup writers roo/cline/goose/qwen/kilocode/pi/omp | — (absent) | `setup.go:19-113` (only continue/opencode/aider) | **GAP-P1** | roo/cline/goose/qwen/kilocode/pi/omp manual-config |
| 5 | Dead-owner takeover resolution | `freebuff-instance-owner.ts:45-66`, `send-message.ts:620-627` | `classify.go:160-170` (terminal 503, no resolution) | **GAP-P1** | ALL w/ shared token (omp/pi multi-session) |
| 6 | `model_locked` GET→DELETE→POST auto-repick | `use-freebuff-session.ts:676-720` | `classify.go:171-175` → `errors.go:309-324` | **GAP-P1** | ALL (wedged turn after model switch) |
| 7 | Country-block best-effort DELETE | `use-freebuff-session.ts:411-431` | `errors.go:368-370` (no DELETE) | **GAP-P1** | ALL mild (lingering server row) |
| 8 | Sponsored-run settle timers | `sponsored-run.ts:437,1281-1285`, `exit-cleanly.ts:60-63` | — | **GAP-P1** (→WONT if CLI-only) | none today |
| 9 | Freebucks countdown/copy | `freebucks.ts:122-203` | `session_parse.go:76-82,224-228` (data only) | P2 | cosmetic |
| 10 | Backoff exact constants | `polling-backoff.ts:15-59` | `pool_lifecycle.go:26-40` | P2 | none |
| 11 | `turn_spend_limit` terminal 429, no Retry-After | `error-handling.ts:125-139`, `freebuff-errors.ts:11-14` | `classify.go:72-80`, `errors.go:175-190` | PORTED | — |
| 12 | Session envelope: POST model header, GET/DELETE instance header, compact, timezone, first-tab-discount | `freebuff-session-api.ts:117-133` | `session.go:264-273`, `client.go:124-128` | PORTED | — |
| 13 | 404→`none`; 403 `country_blocked`/`banned` passthrough; 409 `model_locked` family; 429 `rate_limited` family as data | `freebuff-session-api.ts:149-193` | `classify.go:29-46,95-101,111-112,171-200` | PORTED | — |
| 14 | POST retry only on 408/429/503; GET retries transient | `freebuff-session-api.ts:48-73` | `client_chat.go:43-45,107-111` | PORTED | — |
| 15 | Poll cadence 30s±20%, fail backoff 20s→300s + Retry-After floor | `polling-backoff.ts`, `use-freebuff-session.ts:76-111` | `pool_lifecycle.go:26-40,98-102,320-324` | PORTED | — |
| 16 | Config-dir resolution (+ absolute guard, env suffix) | `config-dir.ts:16-36` | `clicreds.go:16-41` | PORTED | — |
| 17 | Credential read: `default` profile, `authToken` field only | `auth.ts:75-96,136-150` | `clicreds.go:43-71` | PORTED | — |
| 18 | Headless login (device-code + protocol) | CLI `login-flow.ts` (via `/api/auth/cli/*`) | `auth_login.go:1-58`, `refreshtoken.go:1-66` | PORTED | — |
| 19 | Session DELETE + refund receipt + 404 tolerance | `freebuff-session-store.ts:90-93`, `freebuff-session-api.ts:122-124` | `session.go:264-273`, `session_parse.go:447-451` | PORTED | — |
| 20 | Chat-gate recovery (`session_expired`/`waiting_room_*`/`superseded`) | `send-message.ts:580-631` | `errors.go:238-300` (+ pool re-admit) | PORTED | — |
| 21 | `402` out-of-credits (user-billing shape) | `error-handling.ts:43-53`, `:243` | `classify.go:85-86`, `errors.go:377-384` | PORTED (copy differs, semantics same) | — |
| 22 | `free_mode_cli_required` / `invalid_agent_hierarchy` 403 | `freebuff-models.ts` gate codes | `classify.go:102-110`, `errors.go:371-376` | PORTED | — |
| 23 | Waiting-room queued/required, superseded, fanout, capacity-deferred, ip_capped, load-shedding, peak-hours, no-endpoints, outside-hours | `freebuff-session.ts` gate codes | `classify.go` full matrix + `errors.go:191-341` | PORTED | — |
| 24 | `session_limit_reached` excluded from CLI recovery (Desktop cap) | `error-handling.ts:213-241` | `errors.go:238-245` (distinct 409, never session-invalid) | PORTED+ (superset) | — |
| 25 | Strict gates before conversion, all 3 surfaces | n/a (proxy-only) | `strict_tools.go`, `openai.go:77`, `responses.go:94`, `anthropic.go:98`, `anthropic_json.go:54` | PORTED+ | Hermes/OpenClaw protected |
| 26 | Exit: end session (DELETE) on quit | `exit-cleanly.ts:81-83,102` | shutdown persist / `LeaseRelease` path | PORTED (concept) | — |
| 27 | Freebucks read-off-wire, presence-is-gate, no client role check | `freebucks.ts:46-58` | `session_parse.go` freebucks block | PORTED (concept) | — |
| 28 | Startup flags surface (`-setup`, `-refresh-token`, `-doctor`, etc.) | CLI login/setup flows | `main.go:98-160` | PORTED (concept) | — |
| 29-37 | W1–W9 wont-port rows | see WONT table | — | WONT | none |
