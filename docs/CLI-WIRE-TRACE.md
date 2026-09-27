# CLI wire trace — how the genuine freebuff CLI talks to the server

Chronological wire conversation, verified at vendor tip `57943aa71` and
re-verified unchanged at `ede39b345` (npm `0.1.0`; session, chat, poll,
ads-transport paths byte-identical across the range). One new const in
range: `x-freebuff-client` (`freebuff-desktop-sessions.ts:82-88`) is
Desktop-only — the CLI sends nothing, so proxy absence stays CLI-shaped.
Complements `docs/UPSTREAM-CLI.md` (CLI UX reference, older audit pin) —
wire only.
## 0. Transport

- Session/auth base: `https://codebuff.com`, overridable via
  `NEXT_PUBLIC_CODEBUFF_APP_URL` (`cli/src/utils/freebuff-session-api.ts:107-112`).
  Chat + agent-runs go to `getWebsiteUrl()` (same host family;
  `sdk/src/impl/model-provider.ts:361-364`, `sdk/src/impl/database.ts:384,463`).
- Bare `fetch` (Bun runtime): non-chat calls carry the default `Bun/<version>`
  UA with no override. Only three UA personas exist on the wire: `Bun/*`
  (session/login/probe/agent-runs), `ai-sdk/openai-compatible/<VERSION>/codebuff`
  (chat only), `Freebuff-CLI/<binary-version>` (ads header only).
- Session calls: `AbortSignal.timeout(20_000)` (`freebuff-session-api.ts:26,99-105`).
  Login/generic API calls: 30s (`codebuff-api.ts:305,368-369`).
- Generic retry (`codebuff-api.ts:84-89,255-293`): 3 tries, 1s→10s doubling +
  0–30% jitter, statuses `[408,429,500,502,503,504]`; AbortError and TLS-cert
  errors never retry. agent-runs path (`sdk/src/retry-config.ts:27,33,39`,
  `sdk/src/error-utils.ts:16`): 3 tries, 1s→8s, same statuses.
- No custom TLS anywhere (Bun/Node defaults); update check runs once per
  process (100ms after spawn, npm registry `latest`) never per message
  (`cli/release-core/launcher.js:1780-1782,563-591,1291-1351`). Generic
  client also supports a legacy `Cookie: next-auth.session-token` opt-in
  (`codebuff-api.ts:337-346`) — proxy never sends it.

## 1. Fingerprint (first, cached per process)

`machineId()` from `node-machine-id`, rejected if empty/`unknown`/short
(`cli/src/utils/fingerprint.ts:23-34`); hashed inputs: SMBIOS serial/uuid,
CPU, distro, hostname, shell, Node version, MACs, `interfaceCount`,
`fingerprintVersion: '2.0'` (`:84-129`) → sha256 → base64url →
`enhanced-<43ch>` (`:124-128`); legacy fallback `codebuff-cli-<8ch>`
(`:135-138`); process-lifetime cache (`:145-157`); type mapping
`enhanced-`→enhanced_cli, `codebuff-cli-`/`legacy-`→legacy (`:230-240`).

## 2. Login (device-code)

1. `POST /api/auth/cli/code`, no auth, body `{fingerprintId}`
   (`cli/src/utils/codebuff-api.ts:516-523`) → `{loginUrl, fingerprintId,
   fingerprintHash, expiresAt}`. Fingerprint prefetched at startup
   (`init-app.ts:32-33`); TUI fetch is Enter-gated
   (`use-login-keyboard-handlers.ts:48-59`).
2. CLI opens `loginUrl` **verbatim** (`plain-login.ts:54`, `open-url.ts:29-63`
   never mutates; headless-Linux/WSL-no-interop skips open, never rewrites).
3. `GET /api/auth/cli/status?fingerprintId&fingerprintHash&expiresAt`,
   5s interval / 5min timeout; 401 = pending-silently, other non-ok warns,
   both sleep+continue (`login-flow.ts:122-123,168-201`); success =
   response carries `data.user` (`:183-189`); stored to
   `<configDir>/credentials.json` (0600, `{authToken, fingerprintId?,
   fingerprintHash?}`; `auth.ts:45-46,205-212`).

## 3. Session admission (one chat)

`POST /api/v1/freebuff/session/admission`, **no body**
(`freebuff-session-api.ts:214-218`). Headers (`:174-208`):

| Header | Value |
|---|---|
| `Authorization` | `Bearer <token>` |
| `x-fb-timezone` | `Intl` host zone, evaluated per request (`freebucks-timezone.ts:17-22`) |
| `x-freebuff-model` | requested model id (`freebuff-models.ts:3413`, POST only) |
| `x-freebuff-wallet-spend-limit` | `String(limit ?? 0)` (`:3414-3415`, POST only) |
| `x-freebuff-first-tab-discount` | `'1'`\|`'0'` per offer state |
| `x-freebuff-multi-session` | `'1'` iff multiSession (`freebuff-models.ts:3460`) |
| `x-freebuff-purchase-continuity` | `'1'` iff multiSession (`freebuff-desktop-sessions.ts:91-92`) |
| `x-freebuff-instance-id` | `cli:<uuid>` iff `(multiSession \|\| method != POST) && instanceId` (`freebuff-models.ts:3410`); claim = `cli:`+randomUUID (`freebuff-session-identity.ts:11-13`) |
| `x-freebuff-desktop-attempt-id` | attempt uuid iff attemptId && method ≠ GET (`freebuff-desktop-sessions.ts:83`) |
| `x-freebuff-takeover-instance-id` | id iff POST + takeover (`freebuff-models.ts:3481-3482`) — explicit user-confirmed takeover only |
| `x-freebuff-heartbeat` | `'1'` iff GET + instanceId (`:3466`) |
| `x-freebuff-include-unused-rate-limits` | `'1'` iff GET + instanceId + NOT compact (`:3452-3453`) |
| `x-freebuff-compact-session` | `'1'` iff GET + compact (`:3457`) |

Response: typed union (`common/src/types/freebuff-session.ts:829-1161`) —
`first_tab_discount_changed`, `consent_required`, `none/active/ended`,
purchase set (`purchase_claim_released/in_use/capacity`, `premium_slot_taken`),
`model_locked/model_unavailable` (+`withdrawn`/`requiresSubscription`/
`updateRequired`/`purchasesPaused`), `rate_limited/spend_limited/ip_capped`,
`country_blocked/banned`; `superseded` is GET-only (`:1163-1175`). There is
NO `waiting-room` admission status at this tip — `waiting_room_required`
(428) / `waiting_room_queued` (429) are chat-gate codes (`:1221-1245`).
POST 404/405 → `session_admission_unsupported` (`:220-227`); GET 404 →
`{status:'none'}` (`:228-230`); `retry-after` parses seconds-or-date
(`:84-96`).

## 4. Chat

`POST {base}/api/v1/chat/completions` (`sdk/src/impl/model-provider.ts:361-370`).
Headers: exactly `Authorization: Bearer` + `user-agent:
ai-sdk/openai-compatible/${VERSION}/codebuff` (+ optional acting-user id,
optional BYOK openrouter key) — no `Accept`, no model/instance headers.
`VERSION = __PACKAGE_VERSION__ ?? '0.0.0-test'`
(`packages/llm-providers/src/openai-compatible/version.ts:1-5`; package
`1.0.0`). Ids ride `codebuff_metadata` (`client_id` 13×`[a-z0-9]`,
`run_id`, `cost_mode`, `llm_step_number` as string, `complete_compaction`
tool verbatim; reserved keys win over caller extras — `llm.ts:70-129`).
Per-turn session link (`use-send-message.ts:751-759`,
`freebuff-session-identity.ts:21-29`): `{freebuff_instance_id,
freebuff_multi_session:'1', surface:'cli', freebuff_reasoning_effort?}`.
Sponsored turns instead send grant keys (`freebuff_sponsored_proposal_id/
run_id/procedure_sha256/compute_token/surface:'cli'`, `sponsored-run.ts:904-910`)
and NO `freebuff_instance_id`. Retries: stream ×2 + retryable-transient
wrapper (`model-provider.ts:224-288`); `turn_spend_limit` 429 is terminal
for the turn (message names continuing in a new message;
`freebuff-errors.ts:21-24`); `free_mode_capacity_deferred` 429 stays
silently retried. Chat-gate codes need code+status pairs
(`freebuff-session.ts:1221-1264`): `waiting_room_required:428`,
`waiting_room_queued:429`, `session_expired:410`, `session_superseded:409`,
`session_model_mismatch:409`, `session_limit_reached:409`,
`model_unavailable:410`.

## 5. agent-runs (per chat)

`POST /api/v1/agent-runs`, `Authorization: Bearer` (+ optional acting-user),
no key header (`sdk/src/impl/database.ts:384-403,463-487`), same
`fetchWithRetry` (3 tries, 1s→8s, statuses 408/429/500/502/503/504).
START body `{action, agentId, ancestorRunIds}` → `{runId}`; FINISH body
`{action, runId, status, totalSteps, directCredits, totalCredits,
errorMessage≤5000, steps}` — full payload in ONE request, no `/steps`
endpoint; non-ok only logs, never throws. Fired once per hosted run
start/end (`agent-runtime.ts:129-130`); BYOK runs use local stub ids.

## 6. Polls, heartbeat, end

- Startup begins with a GET probe; POST only on join/rejoin/takeover/
  model_locked-release/fallback (`use-freebuff-session.ts:591-595,686-700`).
  Success cadence 30s ±20% jitter (`:77,91-93`, `polling-backoff.ts:47-59`);
  `active` lands just past expiry: `max(1000, min(cadence, remainingMs+1000))`
  (`:99-102`).
- POST retries ONLY 408/429/503 (pre-mutation rejections); GET retries
  408/429/5xx (`freebuff-session-api.ts:57-82`); failure records
  `retry:{attempt, retryAtMs}` + `outcomeUnknown` (`:1100-1123`). Failure
  backoff 20s base, doubling, 300s max, Retry-After honored as
  `max(backoff, retryAfter×1.2)` capped 300s (`polling-backoff.ts:15-44`).
  Terminal statuses stop polling (`null`): everything except `active` and
  grace-window `ended` (`:90-125`).
- Compact polls only when `previousStatus === 'active' && !needsFullActivePoll`
  (`:667-668`); merge carries `rateLimit/rateLimitsByModel/subscription/
  freebucks` from current only when both polls are same-instance `active`
  (`freebuff-session-api.ts:301-328`), else one full poll is forced.
- `premium_slot_taken|purchase_in_use|purchase_capacity` → local
  `takeover_prompt`, no extra wire (`:755-775`). `model_locked` with
  explicit pick → GET-verify + DELETE + re-POST (`:840-903`);
  `model_unavailable` → in-memory fallback + re-POST (`:904-961`).
- `superseded` is terminal (rejoin = fresh POST, never fight).
- End: `releaseSlot()` → `DELETE .../session/attempt` (attempt ids) else
  `DELETE .../session` (legacy); non-`ended` receipt throws
  (`freebuff-session-store.ts:153-223`); `freebucksRefundPending` →
  `pendingRefund`, re-polled every 3s until the receipt lands (`:510-527`,
  `:111-124`). Expiry `active|ended → none` synthesizes `ended` carrying
  fresh-or-prior quota fields (`:1012-1037`).

## 7. Ads per chat (cli_chat surface)

Auction: `POST` to `FREEBUFF_WEB_URL/api/ads` when sponsored-capable else
`WEBSITE_URL/api/v1/ads` (`ad-request.ts:141-142`; no auth token → no
request at all, `:127-131`). Headers `Content-Type` + `Authorization:
Bearer` + `User-Agent: Freebuff-CLI/<ver>` (`:143-149`). Gravity rail:
starts after first user message, 60s rotation, ≤3 fresh fetches per 30s
activity window then cache (`use-gravity-ad.ts:36-38,598-665`); inline
slots cycle per answer without re-auction.
Impression (render proof): `POST WEBSITE_URL/api/v1/ads/impression` with
`x-freebuff-event-id: <uuid>`, body `{impUrl, mode: agentMode, userAgent,
os, clientEventId (ALWAYS on the legacy path — only `renderDelayMs` is
optional), renderDelayMs?}` (`use-gravity-ad.ts:440-461`) — the server
fires the pixel itself; the POST-back of its minted `impUrl` +
render proof. Partner rows: `surface: 'cli_chat'`, no `provider`, 30min
fill TTL (`cli/src/ads/partner-ads.ts:53-64,164-170,260-308`). Offer rail
(`POST /api/v1/ads/agentic/offer`, 10s timeout) fires only with sponsored
capability. Click / first-party ack / zeroclick pixel fire only on actual
render.
