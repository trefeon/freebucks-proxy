# PORT-MAP — CLI behavior → proxy owner (status per row)

> Companion to `CLI-FLOW.md` (lifecycle) — this file answers "who owns this
> behavior in the proxy, and does it match the CLI?" Statuses:
> **PORTED** (proxy mirrors CLI), **DIVERGED** (both do it, differently —
> the 9 kit divergences D1–D9), **MISSING** (CLI does, proxy does not),
> **PROXY-ONLY** (proxy does, CLI has no counterpart).
> Sources: `devdocs/re-kit/*.md`; CLI side = vendor clone `upstream/freebuff`
> at tip `a9ef994` (kit pins are 0.0.193; rows re-verified 2026-09-24 carry
> ✓ tip / ✓ repo); proxy side = repo `backend/` + `frontend/` at checkout.
> No tokens/hosts; samples redacted.

---

## 1. Auth (device-code login)

| CLI behavior | CLI file:line | Proxy owner file:line | Status | Notes |
|---|---|---|---|---|
| Mint login code `POST /api/auth/cli/code {fingerprintId}` unauthenticated | `cli/src/login/login-flow.ts:37-86` via `cli/src/utils/codebuff-api.ts:516-523` | `backend/internal/upstream/auth_login.go:125-185` `StartCLILoginWithFingerprint` | DIVERGED (D1) | Proxy rewrites `loginUrl` → onboard path preserving only `auth_code` (`auth_login.go:172-177` ✓ repo) vs CLI opens verbatim (`plain-login.ts:54`). Deliberate (D1). |
| `expiresAt` ISO string echoed into status poll | `codebuff-api.ts` `LoginCodeResponse` type | `backend/internal/upstream/auth_login.go:87-94,195-220` `ExpiresAtRaw` verbatim echo; numeric/millis tolerated (`:245-249` ✓ repo) | DIVERGED (D3) | Proxy is more tolerant than the CLI type; verbatim echo avoids re-encode skew. |
| Poll loop 5 s / 5 min in client, 401 = pending | `cli/src/login/login-flow.ts:112-204` (defaults `:122-123` ✓ tip) | `backend/internal/upstream/auth_login.go:231-310` single-shot + `:416-443` 5 min loop in callers | DIVERGED (D4) | Same timing, split ownership: single-shot client, loop in proxy callers. |
| Persist creds `credentials.json` 0600, drop legacy OAuth | `cli/src/utils/auth.ts:205-230` (mode tighten `:48-67` ✓ tip) | No proxy file (operator-owned pool tokens, never persisted; `TokenKey` = sha256 hex, `backend/internal/upstream/client.go:119-124`) | MISSING (by design) | Proxy must never write credential files; pool tokens live in env/config. See §9 M1. |
| Logout best-effort + clear regardless | `cli/src/utils/auth.ts:263-295` via `codebuff-api.ts:549-556` | No logout leg (pool tokens are long-lived operator secrets) | MISSING (by design) | No CLI-user session to clear; token removal is operator action. |
| `GET /api/v1/me` validation + acting-user id | `cli/src/hooks/use-auth-query.ts:64-76`; `codebuff-api.ts:501-507` | `backend/internal/upstream/session.go:348-359` `stampActingUser` | PORTED | Own-id only; foreign value = impersonation risk (HEADERS). |

## 2. Fingerprint

| CLI behavior | CLI file:line | Proxy owner file:line | Status | Notes |
|---|---|---|---|---|
| One machine id per process: `enhanced-` sha256 hardware JSON; legacy `codebuff-cli-<8rand>`; process-cached | `cli/src/utils/fingerprint.ts:84-129,135-138,152-157` (all ✓ tip); prefetch `cli/src/init/init-app.ts:33` (✓ tip) | `backend/internal/upstream/login/fingerprint.go:106-184` shape synthesis; `:41` stable / `:54` isolated (`GenerateIsolatedFingerprintID` = `enhanced-`+base64url(random-32B)); isolated use `auth_login.go:118-123` | DIVERGED (D2) | Same `enhanced-`+base64url shape, NOT byte-identical to real hardware. Isolated-per-account contains blast radius. |
| Sync legacy-only fallback | `fingerprint.ts:223-225` | None | MISSING (trivial) | Proxy always has async context; no sync path needed. |

## 3. Session admission + tick

| CLI behavior | CLI file:line | Proxy owner file:line | Status | Notes |
|---|---|---|---|---|
| Admission POST, no body, `x-freebuff-model?` + wallet + tz + first-tab | `cli/src/utils/freebuff-session-api.ts:151-185` (args `:155-158`, tz spread `:165` ✓ tip) | `backend/internal/upstream/session.go:87-100` (+ header consts `:26-51` ✓ repo) | DIVERGED (D5) | Proxy always wallet `"0"` + first-tab `"0"` (`:26-32,:43-51` ✓ repo): correct only for no-consent/no-offer; `consent_required` / `first_tab_discount_changed` surface as 409 the proxy cannot complete. |
| Poll GET with instance + compact, 30 s ±20% jitter, clamp to `remainingMs` | `cli/src/hooks/use-freebuff-session.ts:63,76-88` (✓ tip); compact merge `:261-291`; `polling-backoff.ts:48-59` (✓ tip) | `backend/internal/upstream/session.go:116-131` (`GetSessionWithOpts`, compact `:127-129` ✓ repo); pool cadence via session manager | PORTED | Compact discipline preserved (previous==active only). |
| `ended` polls only while `instanceId` present | `use-freebuff-session.ts:89-110` | `backend/internal/session/session_manager.go` + `session_admission.go` | PORTED | Grace vs fully-gone distinction kept. |
| Zero-cost probe (no instance header, never streak) | (implicit: headerless GET) | `backend/internal/upstream/session.go:133-173` `ProbeAccount` (`:161` ✓ repo); streak exclusion `:158-160` ✓ repo | PROXY-ONLY | CLI has no probe; proxy needs token validation without burning session allowance. |
| Startup takeover probe + owner file; dead-pid silent takeover | `freebuff-session.ts:5-9`; `use-freebuff-session.ts:97,387-400` (✓ tip); `freebuff-instance-owner.ts:8-12,35-43,60-66` (✓ tip) | `backend/internal/session/session.go:76-87` `ReasonSuperseded`; `session_admission.go:351,723,787,858` (✓ repo); `session_poll.go:37` | DIVERGED (D8-local) | `takeover_prompt`/`superseded` are local-only CLI states that must never be expected on wire (`emit_wire.go:124-126`); proxy never auto-takeovers — terminal 409, next request re-joins (`classify.go:207-217` ✓ repo). |
| Full admission union incl. Desktop `purchase_*` | `common/src/types/freebuff-session.ts:809-1160` (gates `:1221-1243` ✓ tip) | `backend/internal/wirefacts/emit_wire.go:124-126` known-status subset | DIVERGED (D8) | Proxy handles the serving subset; unknown statuses fail closed. |
| Streak GET on demand | `use-freebuff-streak-query.ts:21-24` | `backend/internal/upstream/session.go:242-291` `GetStreak` (path `:257` ✓ repo) | PORTED | Same route, same fields. |
| DELETE release, 404 tolerated, refund-pending re-DELETE 3 s | `freebuff-session-api.ts:293-301`; `use-freebuff-session.ts:480-497,1012+`; restart DELETE `:223-260` (✓ tip) | `backend/internal/upstream/session.go:302-346` `EndSession`; receipt type `:64-69` (✓ repo) | PORTED | 404→nil, tz + first-tab stamped (`:313-319` ✓ repo). |
| No `web/` server source in clone | n/a (client tree only) | `backend/internal/wirefacts/testdata/wire/` file list | DIVERGED (D9) | Server gates verified from call sites only; all server-side claims UNVERIFIED. |

## 4. Agent runs

| CLI behavior | CLI file:line | Proxy owner file:line | Status | Notes |
|---|---|---|---|---|
| START `{action:START, agentId, ancestorRunIds[]}` → runId | `sdk/src/impl/database.ts:379-444` (`:382,396,398` ✓ tip) | `backend/internal/upstream/session.go:362-486` (`:375-381` Bearer-only ✓ repo) | PORTED | Acting-user stamped both legs (`:381,:474` ✓ repo). |
| Per-step chat on same runId; usage/cost/step callbacks | `run-agent-step.ts:564-571,1203-1205,1217-1226` (handler `:565` ✓ tip) | `backend/internal/server/engine_attempt.go:37-66` pooled/bridge backends; `RecordSpend/RecordRunStep` | PORTED | Pool multiplexes turns onto managed runs. |
| FINISH `{runId, status, totalSteps, steps[], errorMessage?}`; steps ride FINISH, no `/steps` | `sdk/src/impl/database.ts:446-502` (`:454,475-478` ✓ tip) | `backend/internal/upstream/session.go:435-442+` (errorMessage trunc 5000) | PORTED | No `/steps` endpoint either side. |
| Per-agent acquire/rotate (6 h); run-invalid retried once fresh | n/a (CLI runs are 1:1 with the chat) | Pool `RunManager` + `engine_attempt.go:255-258` | PROXY-ONLY | CLI has no pooling; proxy-only by necessity. |

## 5. Chat envelope

| CLI behavior | CLI file:line | Proxy owner file:line | Status | Notes |
|---|---|---|---|---|
| `extraCodebuffMetadata{freebuff_instance_id, freebuff_reasoning_effort?}` iff `IS_FREEBUFF && !byok && instanceId` | `cli/src/hooks/use-send-message.ts:664-676` (`:670-675` ✓ tip) | `backend/internal/upstream/chat.go:402-438` `injectEnvelope` | PORTED | Caller-supplied keys preserved (`:432-438` ✓ repo). |
| `codebuff_metadata{run_id, client_id(13ch base36), trace_session_id, freebuff_instance_id, llm_step_number:String(n), cost_mode, …}` + `provider{data_collection:deny,…}` + `stream:true` | `sdk/src/impl/llm.ts:70-121` (`:99-104,:114-121` ✓ tip; kit says "SDK llm.ts" — real path `sdk/src/impl/llm.ts`) | Same `chat.go:402-438`; client-id shape guard `:422-426` (never `sess:`/`run:` prefixes, never `^wf-[a-z0-9]{8}$`) | PORTED | Shape guard blocks proxy-fingerprintable ids. |
| `x-freebuff-acting-user-id` own-id only; NO model/instance headers on chat | `sdk/src/impl/model-provider.ts:430-435` (✓ tip) | `backend/internal/upstream/chat.go:131-150` + `session.go:348-359` (✓ repo) | PORTED | Foreign id = impersonation; instance rides body only (`:135`). |
| `maxRetries:3` stream via `PromptAiSdkStreamFn` | `prompt-agent-stream.ts:95` | `TRANSIENT_RETRIES` budget `chat.go:91-208` | DIVERGED (D7) | Ownership differs (see §7): CLI absorbs in SDK + poll loop; proxy retries in-request same-session. |
| Quota from NEXT session poll, never stream | (kit MODEL-SELECT; no single line — architectural) | Pool quota tracker reads poll snapshots | PORTED | No stream-derived quota anywhere. |

## 6. UA / TLS

| CLI behavior | CLI file:line | Proxy owner file:line | Status | Notes |
|---|---|---|---|---|
| Bun default UA on ALL non-chat legs | (bare `fetch`, no override) | `backend/internal/upstream/client.go:139-144` `bunUserAgent = "Bun/1.3.14"` (✓ repo) | PORTED | Pinned to upstream `.bun-version`; rebump on vendor move. |
| Chat UA `ai-sdk/openai-compatible/<VERSION>/codebuff` (LIVE version) | `sdk/src/impl/model-provider.ts:432` (✓ tip: `${VERSION}`); BYOK sibling `:372` (`…/freebuff-byok`) | `backend/internal/upstream/client.go:126-137` pinned literal `…/1.0.0/codebuff` (✓ repo) | DIVERGED (D6) | **UA pinning drift risk:** silent mismatch on every vendor bump until re-pinned. BYOK UA shape not mirrored (proxy never sends BYOK). |
| Ads UA `Freebuff-CLI/<cli-version>` | `cli/src/hooks/use-gravity-ad.ts:817-821` | `backend/internal/upstream/ads.go:34,150` (`Freebuff-CLI/<wirefacts.VendorVersion>`, never `1.0.0`) | PORTED | Version-sourced, not literal. |
| Cert errors never retried; 408/429/500/502/503/504 backoff 1 s→10 s +30% | `codebuff-api.ts:197-244,84-89,255-263,304-306` | `backend/internal/upstream/client.go:43-64` (transport retry), `classify.go` matrix | PORTED | Fail-closed on cert. |
| Stealth profiles NOT on API calls | n/a | `backend/internal/stealth/headers.go:48-80` (`SanitizeHeaders`, `ApplyProfileHeaders` ✓ repo); only browser-surface use | PORTED (by policy) | API legs stay Bun-faithful; profiles never leak onto upstream API. |

## 7. Catalog / registry / routing (CLI none vs pool)

| CLI behavior | CLI file:line | Proxy owner file:line | Status | Notes |
|---|---|---|---|---|
| Single-process picker: tier/filter/joinable/sections/keyboard | `freebuff-model-selector.tsx` (flow §5; `:227,240,328-329,536,561-566` ✓ tip) | Dashboard picker (`frontend/src/`, pool cards `dashboard_cards.go:135-137` `pinned_model` ✓ repo) | DIVERGED | Dashboard picker is operator-facing (fleet view), not a user seat picker; no paywall/confirm intents. |
| Catalog single source (`FREEBUFF_MODELS`, `SUPPORTED_*`, `LIMITED_*`, ids, entitlements, availability chain) | `common/src/constants/freebuff-models.ts` + `freebuff-model-ids.ts` + `freebuff-model-entitlements.ts` | `backend/internal/modelcat/catalog_gen.go` + `catalog_query.go` (`DisplayName :16-20`, `WithdrawnModelMessage :84-92` ✓ repo) + `catalog_ladder.go`; `backend/internal/registry/registry.go:124-193`, `registry_resolve.go:14,57` (`ResolveModel`, `AgentForModel` ✓ repo) | PORTED | Generated catalog tracks upstream files; registry resolves alias/suffix. |
| **Catalog ordering:** CLI metered = cheapest-first; `/v1/models` keeps upstream order | `sortModelsByPrice` (`freebuff-model-selector.tsx:328-329` ✓ tip) | `modelcat` serves catalog order | DIVERGED (deliberate) | Proxy does not reorder `/v1/models`; clients sort. |
| **Withdrawal handling:** CLI in-memory flip to FALLBACK + re-POST | `resolveFreebuffModel` fallback chain → `FALLBACK_FREEBUFF_MODEL_ID` | `modelcat.WithdrawnModelMessage` (`catalog_query.go:84-92` ✓ repo) via `server/models.go:16-48` (`ModelUnavailableMessage :20`, `servedModels :55` ✓ repo) | DIVERGED (deliberate) | Proxy refuses paused/tier-gated with replacement copy instead of silent re-POST (a released binary's picker still lists the id). |
| **Probe-model defaulting:** CLI hero = DEFAULT | `getRecommendedFreebuffModelId` (`:561-566` ✓ tip) | `server/models.go:80-112` `probeModel` (CheapestFreeIn → Default → catalog order ✓ repo) | DIVERGED (deliberate) | Proxy probes cheapest-free first to spare quota; CLI shows hero. |
| Served gate `IsServed` | `isSupported` / `SUPPORTED_*` | `server/models.go:55-64` `servedModels`; `modelAdmissible :114`; `modelTierAdmits :136` + `offerFor :164` | PORTED | Refusal copy mirrors `freebuffWithdrawnModelMessage`. |
| No CLI pooling (one seat, one model) | — (architectural) | `pool/acquire_route.go:121` `Acquire` + `:139-150` pin fail-fast + `:658` `admitOnLane` (✓ repo); single-flight `:37-53`; `pool/pin.go:54-55` error copy; `bridge.go`/`bridge_cache.go` bridge lane | PROXY-ONLY | Spill walk, slot park, cooldown/quarantine, FIFO queue — all proxy-only. |
| **PIN_MODEL slot:model pin** | NO CLI equivalent | `backend/internal/config/pin_model.go:9-47` parser (✓ repo: `PinModel` key `config_keys.go:75`, load `config_load.go:126,565`, validate `config_validate.go:58-66`, display `data.go:106,191-194`, catalog `keycatalog.go:204`) + `pool/pin.go` + `acquire_route.go:139-150,292-295` + snapshots `pool.go:240-243,476-479`, `snapshot.go:104-107`, cards `dashboard_cards.go:135-137` (all ✓ repo) | PROXY-ONLY | Fail-fast `allPinnedOut` without touching upstream; skips sit after quarantine gate. |

## 8. Ads legs

| CLI behavior | CLI file:line | Proxy owner file:line | Status | Notes |
|---|---|---|---|---|
| Waiting-room grid always-on (`forceStart`, `floor(width/60)` cards) | `freebuff-landing-screen.tsx:472-485`; `waiting-room-placements.ts:10-20` | `backend/internal/upstream/ads.go:70,91-113` `FireWaitingRoomChain` (15 s cap) on 428 only; auction `:117-141`, impression `:192-199`, click `:206-212`, event-id `== clientEventId` uuidv4 (`:184,226-228,248-256` ✓ repo) | PORTED (waiting-room only) | Chain fires on server 428, not on render. Mode + renderDelayMs deliberately omitted; UA version-sourced (`:34,150`). |
| **Chat-surface gap:** `cli_chat` auction, `CLI-Chat-Inline` pool (≤4), `Single-Ad-Unit-1` rotation, 60 s / 3-per-30 s cadence, choice cache, ZeroClick pixel, first-party ack, sessionId + message history + placementIds | `chat.tsx:200-222,1927-1948`; `use-gravity-ad.ts:603-748` (pool `:683-748`, rotation `:656-676`, activity gate `:603-619`); `ad-banner.tsx`; `first-party-view-ack.ts:47-79` | None | MISSING | Full list in ADS.md §Proxy mirror + gaps (2). Proxy renders no transcript, so no chat-surface mirror exists. |
| Impression dedupe per impUrl; click only on CTA gesture; dock dwell/accidental; `/ads` channel controls | `use-gravity-ad.ts:160-167,283-426,428-473`; `ad-event-hygiene.ts:126`; `use-dock-panel.ts:201-227`; `commands/ads.ts` | Click leg flagged fabricated (`ads.go:84-90`); fresh uuid per leg (no cross-fire dedupe); no BYOK/subscription/`adsEnabled`/height gating; no house-floor or `bfcid` | DIVERGED | Safe retreat = drop click leg (no user gesture exists proxy-side). Subscription does NOT remove ads (house-suppression is the only subscription read). |
| Gating: `!byok && (IS_FREEBUFF \|\| !hasSubscription)`; Freebuff always-on; BYOK kills both | `chat.tsx:213,222`; `commands/ads.ts:48-54`; `settings.ts:26-28` (`adsEnabled:true` ✓ tip) | Chain fires on 428 only | DIVERGED | Proxy has no terminal height / settings / BYOK-connection context on the ad path. |

## 9. Error taxonomy

| CLI behavior | CLI file:line | Proxy owner file:line | Status | Notes |
|---|---|---|---|---|
| POST retries 408/429/503 only; GET retries 408/429/5xx | `freebuff-session-api.ts:48-73` (`:43-61` ✓ tip) | `classify.go` matrix + `client.go:43-64` budgets | PORTED | Pre-commit rationale preserved in comment. |
| Tick backoff 20 s doubling → 300 s cap + Retry-After; 20 s fetch abort | `polling-backoff.ts:3-44` (`:3-4,24-43` ✓ tip); `freebuff-session-api.ts:90-96` (✓ tip) | Pool/session-manager backoff; `sessionCallTimeout` | PORTED | Same shape, pool-owned timing. |
| Gate-code table (`endsTheSession` pairs) | `freebuff-session.ts:1210-1244` (`:1210-1243` ✓ tip) | `classify.go:60-78,131-217` (fanout `:60`, capacity `:74-78`, waiting-room `:168-186`, superseded `:207-217` ✓ repo) + `engine_attempt.go` invalidate + `errors.go`/`error_taxonomy.go` mirror | PORTED | 409 superseded terminal, never auto-reacquire; 429/quota verbatim Retry-After, no chat-path cooldown. |
| Transient-retry ownership: SDK absorbs + poll loop | `model-provider.ts:41-49,62-81`; `freebuff-session-api.ts:48-72` | `chat.go:91-208` same-session retry (deferral `:95-98`, waiting-room `:99-102`, 10 s floor `:178-184`, per-request budget `:105-108` ✓ repo) | DIVERGED (D7) | No direct CLI counterpart; proxy-only in-request recovery. Retry-after honored, amplification-guarded. |

---

## 10. MISSING-first work list (ordered by ban-fidelity impact)

> Impact = how likely the gap is to make upstream treat the proxy as
> non-CLI. UNVERIFIED items need live capture before any code.

1. **UA pinning drift (D6) — highest.** Chat UA pins `1.0.0` literal
   (`client.go:137` ✓ repo) while CLI sends live package `${VERSION}`
   (`model-provider.ts:432` ✓ tip). Every vendor bump silently widens the
   gap; the 403 `free_mode_cli_required` gate keys on the envelope
   (`client.go:126-136` comment) and the server still fingerprints the UA.
   Work: source the UA version from wirefacts at regen (like ads UA already
   does via `wirefacts.VendorVersion`) instead of a literal. *Status: DIVERGED.*
2. **Bun UA pin (`Bun/1.3.14`) staleness.** Same drift class on non-chat legs
   (`client.go:144` ✓ repo vs upstream `.bun-version`). Work: same
   wirefacts-sourcing as (1). *Status: PORTED-today, drifts-tomorrow.*
3. **Fingerprint shape synthesis (D2).** Same `enhanced-`+base64url shape but
   not hardware-identical (`login/fingerprint.go:106-184` vs
   `fingerprint.ts:84-129` ✓ tip). If the server ever validates hardware
   plausibility (UNVERIFIED — derivation candidate
   `credentials.ts:16-26`), synthesized ids are the most detectable proxy
   signal. Work: live-capture comparison first; do NOT "improve" realism
   without server evidence. *Status: DIVERGED, capture-gated.*
4. **Chat-surface ads gap (MISSING).** No `cli_chat` auction / inline pool /
   rotation / first-party ack / ZeroClick pixel. If free-mode gating counts
   ad-loop participation (`freebuff-cost-mode.ts:25-32`, UNVERIFIED), a
   proxy that never walks the chat ad loop may trip the free-mode gate.
   Work: capture-gated feasibility study; proxy renders no transcript, so a
   faithful mirror may be impossible — document the verdict. *Status: MISSING.*
5. **Click-leg fabrication flag (`ads.go:84-90`).** Gestureless click reads as
   fake engagement server-side (UNVERIFIED abuse treatment). Work: safe
   retreat = drop the click leg; keep auction + impression. *Status: DIVERGED.*
6. **Wallet / first-tab zeros (D5).** Always `"0"` (`session.go:26-51` ✓ repo)
   vs CLI per-offer state (`freebuff-session-api.ts:163-179`). Consent /
   first-tab-discount accounts 409 where the CLI would complete. Work: plumb
   offer state only if a real account needs it; until then the 409 surfacing
   is correct behavior. *Status: DIVERGED, account-gated.*
7. **Onboard rewrite (D1).** `auth_login.go:172-177` vs verbatim open. Only
   affects the login-URL leg the proxy mints for operators; low ban surface
   (auth leg, not session/chat). Work: none unless capture shows the onboard
   path rejecting proxy-minted codes. *Status: DIVERGED, accepted.*
8. **Single-shot poll split (D4).** Same timing, different ownership; no wire
   difference. Work: none. *Status: DIVERGED, accepted.*
9. **Known-status subset (D8).** Unknown future statuses fail closed
   (`emit_wire.go:124-126`). Work: drift workflow already flags wire files;
   keep the fail-closed default. *Status: DIVERGED, monitored.*
10. **No `web/` server source (D9).** All server-side claims UNVERIFIED. Work:
    static-first per watchdog; live CLI is 0.0.194 vs pin 0.0.193 — re-pin,
    re-verify D1–D9, close or widen each. *Status: process gap.*
11. **Withdrawn-model refusal vs CLI silent re-POST.** Proxy refuses with
    replacement copy (`catalog_query.go:84-92` ✓ repo) where CLI flips to
    FALLBACK. Lower ban risk (no extra admission), higher client-visible
    difference. Work: none — deliberate and documented. *Status: DIVERGED,
    accepted.*
12. **Probe-model defaulting + catalog ordering.** Proxy probes cheapest-free
    (`models.go:80-112` ✓ repo) and serves catalog order; CLI hero-picks and
    sorts cheapest-first. Cosmetic/operator-visible only. Work: none. *Status:
    DIVERGED, accepted.*
