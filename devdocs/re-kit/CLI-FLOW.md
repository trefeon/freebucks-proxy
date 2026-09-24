# CLI-FLOW — freebuff CLI end-to-end lifecycle (cold start → logout)

> Source pins: CLI side = vendor clone `upstream/freebuff` at tip `a9ef994`
> (npm wrapper 0.0.193-generation tree; live CLI 0.0.194 vs static pin 0.0.193
> gap is UNVERIFIED — see §11). Proxy side = repo `backend/` at current checkout.
> This doc SYNTHESIZES `devdocs/re-kit/*.md` (ENDPOINTS, HEADERS, SESSION,
> LOGIN-TUI, MODEL-SELECT, ACCOUNT-REQUEST, DIVERGENCES, ADS, LOCAL-EMU) and
> deepens only where the port map needs proxy owners. No tokens/hosts; all
> samples redacted. Each step: **trigger → CLI file:line → wire effect →
> state change.**
>
> Path convention: `cli/...`, `common/...`, `sdk/...`,
> `packages/agent-runtime/...` live under `upstream/freebuff`
> (= `D:/github_repo/freebuff-reference` per `scripts/check-upstream.sh:90-98`);
> `backend/...` lives in this repo. Kit line numbers are pin-0.0.193; lines
> re-verified at tip `a9ef994` on 2026-09-24 carry a `(✓ tip)` mark.

---

## 1. Cold start / init

| # | Trigger | CLI file:line | Wire effect | State change |
|---|---------|---------------|-------------|--------------|
| 1.1 | Process launch, argv parsed | `cli/src/index.tsx:237` `isLoginCommand = command === 'login'` (✓ tip) | None (local dispatch) | Branch: `login` → plain-login path; else TUI path |
| 1.2 | App bootstrap | `cli/src/index.tsx:241` `await initializeApp({ cwd })` → `cli/src/init/init-app.ts:9-33` (✓ tip; note: kit cites `init-app.ts` — real path is `init/init-app.ts`) | None yet; `initAnalytics()` first (`init-app.ts:16-17`, logger needs it), then `void getFingerprintId()` background prefetch (`init-app.ts:33`) (✓ tip) | Analytics ready; fingerprint promise cached process-wide, no network |
| 1.3 | API client arming | `cli/src/index.tsx:244` `setApiClientAuthToken(getAuthToken())` (✓ tip) | None | All later authed calls carry `Authorization: Bearer <redacted>` (`cli/src/utils/codebuff-api.ts:338-340`) |
| 1.4 | Config/analytics-id load | `cli/src/utils/config-dir.ts:16-36` (✓ tip: `FREEBUFF_CONFIG_DIR` absolute-only at :17-21, else `~/.config/manicode[-<env>]` at :30); `cli/src/utils/anonymous-id.ts:26-55` random UUID in `<configDir>/analytics-id.json` | None | Pre-login anonymous identity exists; aliased via `identifyUser` at login |

Related kit: LOGIN-TUI §Ordered trace steps 1–2; HEADERS §Authorization.

---

## 2. Credential resolve + TUI auth gate

| # | Trigger | CLI file:line | Wire effect | State change |
|---|---------|---------------|-------------|--------------|
| 2.1 | Token lookup | `cli/src/utils/auth.ts:143-157` — `credentials.json` `default.authToken` wins, else `CODEBUFF_API_KEY` env | None | `getAuthToken()` resolved (or empty) |
| 2.2 | TUI auth effect | `cli/src/index.tsx:353-364` (`AppWithAsyncAuth`): no token → `requireAuth=true`; token present → `requireAuth=false` + `hasInvalidCredentials=true` pending validation | None | Gate armed |
| 2.3 | Gate render | `cli/src/app.tsx:238-250` (✓ tip): `requireAuth && isAuthenticated===false && authStatus==='ok'` → `<LoginModal>` | None | User sees Screen 1 (`login-modal.tsx:357-360` "Press ENTER to login...") |
| 2.4 | Token validation (when a token exists) | `cli/src/hooks/use-auth-query.ts:64-76` `validateApiKey` → `getUserInfoFromApiKey` → `GET /api/v1/me?fields=id,email,discord_id` (`cli/src/utils/codebuff-api.ts:501-507`) | `GET {API_ORIGIN}/api/v1/me` Bearer | Valid → authenticated; invalid → invalid-creds banner (`login-modal.tsx:261-278`) |

Related kit: LOGIN-TUI steps 1–2; ENDPOINTS row 4.

---

## 3. Device-code login (generate → poll → persist → logout)

| # | Trigger | CLI file:line | Wire effect | State change |
|---|---------|---------------|-------------|--------------|
| 3.1 | ENTER on Screen 1 (or `c` = copy URL, `ctrl+c` = exit) | `cli/src/hooks/use-login-keyboard-handlers.ts:32-73`; `fetchLoginUrlAndOpenBrowser` (`login-modal.tsx:110-133`) | — | Intent to mint a code |
| 3.2 | Fingerprint resolve (process-cached promise) | `cli/src/utils/fingerprint.ts:152-157` `getFingerprintId()` (✓ tip); enhanced `calculateEnhancedFingerprint` `:84-129` = node-machine-id (`:23-34`) + systeminformation system/cpu/osInfo (`:36-77`) + shell + MAC list → `sha256 → base64url → "enhanced-<hash>"` (`:123-128`) (✓ tip); failure → legacy `codebuff-cli-<8rand>` (`:135-138`) (✓ tip); sync legacy-only `generateFingerprintIdSync` (`:223-225`) | Only the `fingerprintId` string leaves the box; hostname/MAC/serial stay local (folded into the hash) | `enhanced-…` (or legacy) id ready |
| 3.3 | Code mint | `cli/src/login/login-flow.ts:37-86` `generateLoginUrl` → `POST {LOGIN_WEBSITE_URL}/api/auth/cli/code` body `{fingerprintId}` unauthenticated (`cli/src/utils/codebuff-api.ts:516-523`); `LOGIN_WEBSITE_URL = FREEBUFF_WEB_URL` for IS_FREEBUFF builds (`cli/src/login/constants.ts:21-24`, prod `FREEBUFF_WEB_URL_PROD` in `common/src/constants/hosts.ts:6`) | `POST …/api/auth/cli/code {"fingerprintId":"enhanced-<redacted>"}` → `{loginUrl, fingerprintHash, expiresAt}` (`login-flow.ts:44-48`) | Triple held in `login-store` (`use-fetch-login-url.ts:43-52`); browser opened verbatim via `safeOpen(loginUrl)` (`open-url.ts:29-63`; skips headless Linux, powershell check on WSL) |
| 3.4 | Screen 2 wait | `login-modal.tsx:366-471`: URL + "Press c to copy" + "[ Copy link (c) ]" + "Waiting for login..." + remote-session tip | None | User completes OAuth in browser |
| 3.5 | Status poll | `use-login-polling.ts:52-113` → `pollLoginStatus` via `'modal'` (`login-flow.ts:112-204`): `GET /api/auth/cli/status?fingerprintId&fingerprintHash&expiresAt` unauthenticated (`codebuff-api.ts:525-536`); defaults `intervalMs=5000, timeoutMs=5min` (`login-flow.ts:122-123`) (✓ tip); **401 = pending (silent)**, other non-ok warn, network error logged — all continue (`:168-181,:192-201`) (✓ tip) | `GET …/api/auth/cli/status?…` every 5 s, up to 5 min | `LOGIN_TIMEOUT` / `LOGIN_ABORTED` on expiry; success on `data.user` object (`:183-189`) (✓ tip) |
| 3.6 | Success handling | `login-modal.tsx:148-163` `handleLoginSuccess` → `useLoginMutation` (`use-auth-query.ts:190-204`): `saveUserCredentials` FIRST, then `validateApiKey` (`GET /api/v1/me`), then `use-auth-state` `handleLoginSuccess` (`:97-125`): `identifyUser` + `LOGIN` via `'modal'` + `resetCodebuffClient/resetChatStore/resetLoginState` + `isAuthenticated=true` | `GET /api/v1/me` validation | Authenticated session; analytics id aliased |
| 3.7 | Persist | `saveUserCredentials` (`auth.ts:205-230`): merge `{default:{id?,name(null→''),email,authToken,fingerprintId?,fingerprintHash?}}` (schema `:14-29`) into `<configDir>/credentials.json` (`getCredentialsPath :45-47`, ✓ tip), file `0600` / dir `0700`, drops legacy `chatgptOAuth` (`:189-200`; tighten helper `:48-67`, ✓ tip) | None (local) | Credentials durable across restarts |
| 3.8 | Plain-command mirror | `cli/src/login/plain-login.ts:65-104`: same poll via `'plain_command'`, `saveUserCredentials`, identify + `LOGIN` + `flushAnalytics`, exit 0; timeout/abort exit 1 | Same as 3.3/3.5 | Headless login done |
| 3.9 | Proxy port (not CLI) | — | `backend/internal/upstream/auth_login.go:125-185` `StartCLILoginWithFingerprint` (onboard rewrite `:172-177`); `PollCLILogin :231-310` single-shot (`Done=false` on 401/5xx/transport); `pollForCompletion :416-443` 5 min loop; isolated random fingerprint `:118-123`; stable machine hash `backend/internal/upstream/login/fingerprint.go` (proxy `GenerateFingerprintID :41`, `GenerateIsolatedFingerprintID :54`, `fingerprintIDFrom :115`, all ✓ repo) | Proxy-owned loop split (D4) |

Related kit: LOGIN-TUI (all); ENDPOINTS rows 1–3; DIVERGENCES D1–D4.

---

## 4. First-run onboarding (no typed form)

| # | Trigger | CLI file:line | Wire effect | State change |
|---|---------|---------------|-------------|--------------|
| 4.1 | First prompt submitted | `cli/src/chat.tsx:155-156,838-842` (✓ tip): `showSuggestedPrompts = IS_FREEBUFF && !hasSubmittedFirstPrompt()`; on `messages.length > 0` → `markFirstPromptSubmitted()` + hide | None | `hasSubmittedFirstPrompt:true` in `<configDir>/settings.json` (`cli/src/utils/settings.ts:77-78,341-352`; `hasSubmittedFirstPrompt? :67`, ✓ tip) |
| 4.2 | Chips render | `cli/src/components/suggested-prompts.tsx:30-46` three fixed labels → `chat.tsx:1687-1713,1903` (✓ tip) | None | New-user guidance only |
| 4.3 | Explicit non-goals | NO name/email/timezone/locale prompts — id/email/name arrive from the poll user object (§3.5); timezone is per-request `x-fb-timezone` (`common/src/util/freebucks-timezone.ts:17-25`, ✓ tip: header const `:1`, `Intl` resolve `:8-21`); ads device `{os,timezone,locale}` goes only to the ads API (`use-gravity-ad.ts:793-815`); GitHub appears only as referral banner (`freebuff-referral-banner.tsx:561-567,666-667`) | Headers/body on their own legs | — |

Related kit: LOGIN-TUI §No typed onboarding form.

---

## 5. Model select (store → snapshot feed → tier/filter/joinable → commit → admission intent)

| # | Trigger | CLI file:line | Wire effect | State change |
|---|---------|---------------|-------------|--------------|
| 5.1 | Picker mount | `cli/src/components/freebuff-model-selector.tsx:248-249` store `selectedModel` (default recommended hero); `:786` focus cursor `focusedId`; `:257-260` session snapshot arrives (`useFreebuffSessionStore` session; `accessTier` full\|limited `:258-260`; freebucks block presence = meter gate `:292-296`; `planRequired` via `freebuffPlanRequired(model,hasPaidSubscription,freebucks)` `:305-309`) | Snapshot arrives via the §6 tick loop | Picker has tier + meter context |
| 5.2 | List build | `:323-333` (✓ tip: `getFreebuffModelsForAccessTier(accessTier, hasPaidSubscription)` at :227, `sortModelsByPrice` wrap at :328-329): `availableModels = getForTier + sortModelsByPrice` when metered → `:344-350` `offers = getLimitedModelOffers(session)` filtered by `isSupported` → `:378-400` `meterFor = getFreebuffModelMeter{freebucks, quota: rateLimitsByModel[model]}` → `:407-424` `premiumSectionQuotas` shared-pool header → `:560-567` (✓ tip: :561-566) `recommendedModel = getRecommendedFreebuffModelId(accessTier,{premiumExhausted})` else first joinable | None (pure client compute) | Ordered rows + hero |
| 5.3 | Joinability | `:536-557` (✓ tip: `isJoinable` at :536) `isFreebuffModelAvailable(now) + offer.userRemaining>0 + active-session pin + !planRequired + meter.canStart`; `:614-628` priced-out/pressable gate on Enter audibility | None | Rows enabled/disabled |
| 5.4 | Sections + navigation | Metered = flat metered list (`:826-830`); limited = unlabeled (`:813-816`); full = PREMIUM + UNLIMITED (`:831-847`); offer leads LIMITED TRIAL both states (`:861-874`); render hero + sections (`:878-884`); nav = rows + TOGGLE + referral (`:888-895`); Tab/arrows focus, Enter/Space commit (`:1244-1305`) | None | Keyboard-driven selection |
| 5.5 | Commit intent | `:579-595` `pick(modelId)` → `rowIntent(freebucks,model,active)`: allow → start; confirm/paywall first Enter asks (`pendingAsk`), second commits; paywall → `safeOpen(plans URL)` (`:1192-1231`) → `startSession(modelId, walletSpend?)`, default `startFreebuffSession` (`:240,1220-1225`; default at :240 ✓ tip) | Intent resolves to §6 admission POST | Session (re)admission begins |
| 5.6 | Catalog (single source) | `common/src/constants/freebuff-models.ts` `FREEBUFF_MODELS` (served rows, `[0]` = `DEFAULT_FREEBUFF_MODEL_ID`), `SUPPORTED_FREEBUFF_MODELS` (admissible incl. retired/withdrawn for drain/coercion), `FREEBUFF_WEB_ALL_MODELS` (web-only), `LIMITED_FREEBUFF_MODELS` (limited-tier hero +1); ids `freebuff-model-ids.ts`, entitlements `freebuff-model-entitlements.ts`; availability via row `availability` + `isFreebuffModelAvailable/isAvailableAt` + `resolveFreebuffModel` fallback → `FALLBACK_FREEBUFF_MODEL_ID` | None | Proxy mirrors via generated `backend/internal/modelcat/catalog_gen.go` (queries `catalog_query.go`, ladder `catalog_ladder.go`; registry `registry.go` / `registry_resolve.go:14,57` ✓ repo / `registry_refresh.go` / `parse.go`) |
| 5.7 | Display rows | Line 1 name + tagline + Reasoning/Images/NEW/TEST suffixes; line 2 `rowDetails` price N Freebucks/hr (warn if `balance<price`, accent if first-tab list price) + warning + deployment/closed label + off-meter own-pool chip | None | Price-transparent rows; `PIN_MODEL` has NO CLI equivalent (proxy-only slot:model pin `backend/internal/config/pin_model.go`, ✓ repo) |

Related kit: MODEL-SELECT (all); ACCOUNT-REQUEST §Flow.

---

## 6. Session admission + tick loop

| # | Trigger | CLI file:line | Wire effect | State change |
|---|---------|---------------|-------------|--------------|
| 6.1 | Admission POST (one send) | `cli/src/utils/freebuff-session-api.ts:151-185` `callFreebuffSession` (args shape `:155-158` ✓ tip: `instanceId?`, `walletSpendLimit?`, `firstTabDiscount?`); `...freebucksTimeZoneHeaders()` spread `:165` (✓ tip) | `POST /api/v1/freebuff/session/admission` — **no body, no Content-Type**; headers `Authorization: Bearer <redacted>` + Bun UA + `x-freebuff-model:<id>` (if set) + `x-freebuff-wallet-spend-limit:0` + `x-fb-timezone:<IANA>` + `x-freebuff-first-tab-discount:0/1` (proxy mirror `backend/internal/upstream/session.go:87-99`, ✓ repo) | `none → active {instanceId, model immutable, admittedAt, expiresAt, remainingMs, rateLimit, freebucks}` |
| 6.2 | POST decode | `freebuff-session-api.ts:187-256`: POST 404/405 → `session_admission_unsupported` (fail closed); any 404 → `{status:'none'}`; 403 `country_blocked`\|`banned` → state else throw; POST 409 `model_locked`\|`model_unavailable`\|`first_tab_discount_changed`\|`consent_required` → body; POST 429 `rate_limited`\|`spend_limited`\|`ip_capped` → body; else throw with Retry-After (seconds\|HTTP-date→ms) + body error string | Error → typed state or throw | Gate landing renders (see 6.7) |
| 6.3 | Tick loop | `cli/src/hooks/use-freebuff-session.ts:587-613` tick → `GET /api/v1/freebuff/session?instanceId=` + compact merge (`use-freebuff-session.ts:261-291`); cadence `POLL_INTERVAL_ACTIVE_MS=30000` (`:63`, ✓ tip) `±20%` jitter (`polling-backoff.ts:48-59`, ✓ tip: default `jitterRatio=0.2` at :49) clamped `max(1000,min(jittered,remainingMs+1000))` (`:76-88`) | `GET …/session` with `x-freebuff-instance-id` (when held) + `x-freebuff-compact-session:1` (when previous==active) (`freebuff-session-api.ts:261-291`); proxy `backend/internal/upstream/session.go:116-131` (✓ repo: `:116` `GetSessionWithOpts`, compact `:127-129`) | `active…` refreshed; quota/balance update comes from HERE, never the chat stream |
| 6.4 | Poll-only states | `use-freebuff-session.ts:89-110`: `ended` polls only while `instanceId` present, else null/stop | Same GET or stop | `ended{instanceId?}` grace vs fully-gone `none` |
| 6.5 | Startup takeover probe | `freebuff-session.ts:5-9` + `use-freebuff-session.ts:783-804` (probe at :97 ✓ tip: `takeover_prompt` case; guard :387-400): live seat found → `takeover_prompt` ask before POST rotates; dead-local-process row auto-takes-over; owner file `freebuff-instance-owner.json {instanceId,pid}` (`freebuff-instance-owner.ts:45-58`, shape `:8-12` ✓ tip); liveness `process.kill(pid,0)`, EPERM = running (`:35-43` ✓ tip); match + dead pid = silent-takeover trigger (`:60-66`) | Local only — no wire | Proxy NEVER auto-takeovers on superseded |
| 6.6 | Pre-chat landing routing | `cli/src/app.tsx:364-405`: every non-admitted status renders landing EXCEPT `superseded` (own screen) and `ended` (falls through to `<Chat>`) | None | User sees gate copy, not a composer |
| 6.7 | Full status union | `common/src/types/freebuff-session.ts:809-1153` (✓ tip: gate codes `:1221-1243`): `none :824`; `active :865-894`; `ended :898-930` (refund fields); `country_blocked :932-937`; `banned :1027`; `model_locked :944-948`; `model_unavailable :951-1026` (`:971` ✓ tip); `first_tab_discount_changed :810-813`; `consent_required :815-822`; `rate_limited :1050-1087`; `spend_limited :1089-1099`; `ip_capped :1036-1048`; `premium_slot_taken :1122-1140`; `purchase_* :1101-1120` (Desktop-only) | Each decodes from POST/GET bodies | Proxy `wireKnownStatuses` subset only (`backend/internal/wirefacts/emit_wire.go:124-126`; D8) |
| 6.8 | Chat-request gate (error+status pairs, never message text) | `freebuff-session.ts:1200-1244` (✓ tip: `endsTheSession` doc `:1210`, codes `:1221-1243`): `endsTheSession=true` → `waiting_room_required` 428, `session_expired` 410, `session_superseded` 409, `session_model_mismatch` 409 (forget window, re-admit same instance); survivable → `session_limit_reached` 409, `waiting_room_queued` 429 (transient race), `model_unavailable` 410 (keep window, pick another model) | Gate errors from §8 chat POST feed back into session state | Window kept or forgotten per code |
| 6.9 | Streak (on demand, never in probe) | `cli/src/hooks/use-freebuff-streak-query.ts:21-24` → `GET /api/v1/freebuff/streak` Bearer → `{streak,todayUsed,lastUsageDate,timeZone,freebucksDailyBonus?}`; proxy `backend/internal/upstream/session.go:242-291` (✓ repo: `GetStreak :242`, path `:257`); proxy never probes streak inside `ProbeAccount` (`session.go:158-160`, ✓ repo) | Separate GET, no session headers | Streak display only |
| 6.10 | Proxy pooled mirror (not CLI) | — | `EnsureSessionForModel` fast-reuse (active to expiresAt-5s else 30 m grace) else §6.1 POST (`backend/internal/upstream/session.go:87-100`, ✓ repo); GET poll + compact (`:116-131`); DELETE release (`:302-346`); single-flight refresh, re-admit lead + seat gate, instance-guarded invalidate, never auto-takeover (`backend/internal/session/session_manager.go:142-149` `EnsureSession/EnsureSessionForModel` ✓ repo, `session_admission.go:723,787,858` superseded/invalid guards ✓ repo) | Pool seat held across client requests (CLI holds exactly one) |

Related kit: SESSION (all); ENDPOINTS rows 6–8, 11; HEADERS §§ x-fb-timezone, x-freebuff-* set; ACCOUNT-REQUEST §Flow.

---

## 7. Agent runs (START → steps ride FINISH)

| # | Trigger | CLI file:line | Wire effect | State change |
|---|---------|---------------|-------------|--------------|
| 7.1 | Run START | `sdk/src/impl/database.ts:379-444` (✓ tip: params `:382`, `action:'START'` `:396`, `ancestorRunIds` `:398`) | `POST /api/v1/agent-runs {action:START, agentId, ancestorRunIds:[]}` → `{runId}` (proxy `backend/internal/upstream/session.go:362-486`, Bearer-only + optional acting-user `:375-381` ✓ repo) | `runId` minted; per-run `RunID/TraceSessionID/ClientID` allocated |
| 7.2 | Per-step chat | `packages/agent-runtime/src/run-agent-step.ts:564-571` `onUsageReceived → onAgentUsageReceived{isRoot,agentId}` (✓ tip: handler at :565); `:1203-1205` `onCostCalculated → creditsUsed`; `:1217-1226` `addAgentStep` completed/skipped/failed | One §8 chat POST per step (same `runId`) | Steps accumulate client-side |
| 7.3 | Run FINISH | `sdk/src/impl/database.ts:446-502` (✓ tip: `totalSteps :454`, `action:'FINISH'` `:475-478`) | `POST /api/v1/agent-runs {action:FINISH, runId, status, totalSteps, steps[], errorMessage?}` — steps ride FINISH, NO `/steps` endpoint; `errorMessage` truncated 5000 (proxy `session.go:435-442`) | Run closed as completed\|cancelled\|failed |
| 7.4 | Proxy run management (not CLI) | — | `RunManager` per-agent acquire/rotate (6 h) (`backend/internal/session/` + `backend/internal/pool/`); run-invalid retried once with a fresh run (`backend/internal/server/engine_attempt.go:255-258`) | Pool multiplexes many client turns onto managed runs |

Related kit: ENDPOINTS row 10; MODEL-SELECT §Chat send; ACCOUNT-REQUEST §Flow.

---

## 8. Chat turn (template → envelope → POST → stream → quota-from-next-poll)

| # | Trigger | CLI file:line | Wire effect | State change |
|---|---------|---------------|-------------|--------------|
| 8.1 | Template → stream params | `packages/agent-runtime/src/prompt-agent-stream.ts:81,97` `selectedModel → template.model` via `getAgentStreamFromTemplate → aiSdkStreamParams{model, runId, clientSessionId (promptId per prompt), extraCodebuffMetadata:{freebuff_instance_id, freebuff_reasoning_effort?}}` only if `IS_FREEBUFF && !byok && instanceId` (`use-send-message.ts:664-676`, ✓ tip: `:670-675`) | Local assembly | Params ready |
| 8.2 | Provider options stamp | `sdk/src/impl/llm.ts:70-121` (✓ tip; kit says "SDK llm.ts" — real path `sdk/src/impl/llm.ts`): `getProviderOptions` stamps `codebuff_metadata{run_id, client_id: 13-char base36 per-run repeated, trace_session_id per-run, freebuff_instance_id per-session, llm_step_number: String(n) per step, cost_mode, n?, cache_debug_correlation?, freebuff_reasoning_effort mirrored}` (`:114-121` ✓ tip) + `provider{data_collection:deny, order?, allow_fallbacks?}` (`:99-104` ✓ tip) + `stream:true` forced | Envelope complete (proxy `injectEnvelope` `backend/internal/upstream/chat.go:402-438` ✓ repo; client-id shape guard `:422-426` ✓ repo) | Body ready |
| 8.3 | Chat POST | `sdk/src/impl/model-provider.ts:426-435` (✓ tip: UA at :432, acting-user at :433) | `POST /api/v1/chat/completions` OpenAI-shaped `{model, messages, stream:true, …}` + envelope; headers `Authorization` + `User-Agent: ai-sdk/openai-compatible/<VERSION>/codebuff` (proxy pins literal `1.0.0` — `backend/internal/upstream/client.go:137`, ✓ repo; D6) + optional `x-freebuff-acting-user-id` own-id only (`chat.go:136-150` + `session.go:348-359` ✓ repo); NO `x-freebuff-model`/instance headers on chat (`chat.go:131-135`); `stream:true` forced; no `Accept` header (`chat.go:122-126`) | SSE stream opens |
| 8.4 | Stream callbacks | `prompt-agent-stream.ts:95` `PromptAiSdkStreamFn maxRetries:3`; `run-agent-step.ts` usage/cost/steps (§7.2) | SSE relays verbatim (proxy `relayReadLoop` `backend/internal/server/engine_sse.go:100-105` ✓ repo + usage-chunk spend ledger `RecordSpend/RecordRunStep` `engine_attempt.go:37-66` ✓ repo) | Tokens flow; spend ledger accrues |
| 8.5 | Quota source rule | — (kit MODEL-SELECT: "quota NOT from stream — next session GET poll") | Quota/balance read from the NEXT §6.3 session GET, never the stream | No stream-derived quota anywhere |

Related kit: MODEL-SELECT §Chat send; ACCOUNT-REQUEST §§ Flow, Pooled vs bridge; ENDPOINTS row 9.

---

## 9. Errors / backoff taxonomy

| # | Trigger | CLI file:line | Wire effect | State change |
|---|---------|---------------|-------------|--------------|
| 9.1 | Session POST vs GET retry rule | `freebuff-session-api.ts:43-72` (✓ tip: comment `:43-47`, `408/429/503 → retry` `:59-61`): POST retries ONLY 408/429/503 pre-commit (a POST without response may already have rotated the instance); GET retries 408/429/5xx (`:67-72` ✓ tip) | Bounded refire | No double-admission |
| 9.2 | Tick backoff | `polling-backoff.ts:3-44` (✓ tip): `FAILURE_BACKOFF_BASE_MS=20000 :3`, `MAX=300000 :4`, doubling `base*2^exp :24-28`, Retry-After honored + bounded (`:31-43`); per-request `sessionFetchSignal` 20 s abort-combine (`freebuff-session-api.ts:90-96` ✓ tip: `:90-96`) | Sleeps stretch 20 s → 300 s cap | Poll loop survives outages |
| 9.3 | codebuff-api default | `codebuff-api.ts:84-89,255-263,304-306`: default 30 s timeout, 3 retries, exp-backoff 1 s→10 s +30% jitter on 408/429/500/502/503/504 | Retried fetch | API calls resilient |
| 9.4 | TLS fail-closed | `codebuff-api.ts:197-244`: cert errors (SELF_SIGNED/EXPIRED/ALTNAME) detected and NEVER retried; other network errors retried | No retry on cert failure | No downgrade loop |
| 9.5 | Gate-code table | `freebuff-session.ts:1210-1244` (§6.8 ✓ tip) + `freebuff-session-api.ts:227-230` (429-on-POST body parse, ✓ tip: `:227`) | Typed states (§6.2/6.8) | Landing vs rejoin vs terminal |
| 9.6 | Proxy chat transient (no CLI counterpart — D7) | — | Same-session retry on `free_mode_capacity_deferred`/waiting-room, retry-after floor 10 s, budget `TRANSIENT_RETRIES` (`backend/internal/upstream/chat.go:91-208`, ✓ repo: deferral `:95-98`, waiting-room `:99-102`, floor `:178-184`, budget `:105-108`; classify `classify.go:60-78,168-186,207-217` ✓ repo) | Chat survives deferral in-request |
| 9.7 | Proxy error mirror | — | `classifyError → chatAttempt invalidate → writeError` HTTP mirror (`backend/internal/upstream/classify.go`, `errors.go`; `engine_attempt.go`, `errors.go`, `error_taxonomy.go`): 401 → `ErrAuthRejected`; 403 banned/country_blocked terminal; 409 `session_superseded` → `InvalidateSessionSuperseded` terminal (never auto-reacquire, `classify.go:207-217` ✓ repo); 428 waiting-room-required invalidate; session-invalid invalidate; `turn_spend_limit` terminal no failover; 429/quota verbatim Retry-After, no local cooldown on chat path | Upstream semantics preserved 1:1 |

Related kit: SESSION §§ Backoff, Chat-request gate; ACCOUNT-REQUEST §Error table; HEADERS §TLS/retry.

---

## 10. Logout / unmount DELETE

| # | Trigger | CLI file:line | Wire effect | State change |
|---|---------|---------------|-------------|--------------|
| 10.1 | Logout | `cli/src/utils/auth.ts:263-295` (kit LOGIN-TUI cites `:270-274` for the call site): `POST /api/auth/cli/logout {userId?, fingerprintId?, fingerprintHash?}` (`codebuff-api.ts:549-556`) best-effort, authed | `POST …/api/auth/cli/logout` | Credentials cleared REGARDLESS (delete `default` key / unlink) |
| 10.2 | Unmount / exit | `use-freebuff-session.ts:1012+` unmount; restart path `:223-260` (✓ tip: DELETE-before-restart `:223`, halt poll `:237-239`); refund-pending re-DELETE every 3 s (`:480-497`) | `DELETE /api/v1/freebuff/session` with `x-freebuff-instance-id` (required) + tz + first-tab headers, only if `holdsLiveFreebuffSlot`; 404 tolerated → nil; response `{status:'ended', freebucksRefund?, freebucksRefundPending?}` (proxy `backend/internal/upstream/session.go:302-346`, ✓ repo: 404-tolerated `:293-301`, receipt type `:64-69`) | Seat released; refund settled or replayed |
| 10.3 | Proxy release (not CLI) | — | Pool releases on lease end; bridge/p pooled share `chatAttempt/chatCore` (`engine_attempt.go:120` per kit) | Fleet seats recycled |

Related kit: ENDPOINTS rows 3, 8; SESSION §State machine (`ended` grace: chat OK, no new prompts; no `instanceId` → fully gone, rejoin via POST).

---

## 11. UNVERIFIED gaps (live-capture gated)

- Server handler impl for code/status (no `web/` dir in vendor clone); exact
  `loginUrl` shape/query keys; browser OAuth UI; `fingerprintHash` derivation
  (candidate `genAuthCode=sha256(secret+fingerprintId+expiresAt)`
  `common/src/util/credentials.ts:16-26`); server expiry (client 5 min
  `login-flow.ts:122-123` vs `CLI_AUTH_CODE_LIFETIME_MS=1h`
  `common/src/constants/auth.ts:18`); `expiresInMs` field (`auth.ts:8-16`)
  absent from CLI `LoginCodeResponse` type.
- Server grace-window duration for ended-with-`instanceId`; server-side expiry
  vs `remainingMs` clock source; `resolveFreebuffReasoningEffort` authority.
- Live catalog rows at 0.0.194 (Solar swap / Mini 4 / Space Bunny notes in
  `docs/UPSTREAM-CLI.md:12-19` are 0.0.188-era); served-set deltas
  0.0.193→0.0.194; 0.0.193 vs 0.0.194 timing/UA changes.
- Server gate/envelope behavior (CLI+SDK call sites only); live 0.0.194
  error-copy deltas; authoritative quota values.
- Whether 0.0.194 closes or widens any of D1–D9 (PORT-MAP.md).
