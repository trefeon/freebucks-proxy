# WEB — freebuff.com web shapes (chat + account, cookie-observed)

> Observed 2026-09-24 via cookie-authenticated GETs + static client-chunk
> analysis against `https://freebuff.com` (vendor constant
> `FREEBUFF_WEB_URL_PROD`, upstream/freebuff/common/src/constants/hosts.ts:6).
> The vendor clone has NO `web/` dir, so everything below is live
> observation, not source. All values redacted to types; no tokens, emails,
> ids, or cookie values appear anywhere in this file.
>
> Request discipline: cookie-authenticated **GETs only** (page shells, static
> `/_next/*` chunks, read-only `/api/*` JSON). **Zero state-changing
> requests were sent** — no chat ping, no POST/PUT/PATCH/DELETE of any kind.
> Mutation routes below are documented from static client code (method + body
> key names), never invoked.

## 0. Cookie jar inventory (names + domains only)

User-provided jar (untracked, gitignored, unmodified): 12-col dump,
`name, value, domain, path, ...` column order; 17 cookies, all with expiry
present. Names observed:

- `__Host-next-auth.csrf-token`, `__Secure-next-auth.callback-url`,
  `__Secure-next-auth.session-token` (NextAuth; session validity PROVEN —
  `GET /api/auth/session` returns 200, see §2)
- `bfcid` (ad-attribution id; cf. ADS.md `bfcid` cookie + `x-freebuff-bfcid`)
- `_gr`, `gr_attrib`, `gr_session` (Gravity ad pixel family)
- `_rdt_em`, `_rdt_uuid` (Reddit pixel family)
- `freebuff_meta_ads_allowed`, `freebuff_paid_social_allowed`,
  `freebuff_signup_challenge`, `freebuff_signup_recaptcha_v3`,
  `human_behavior_end_user_id`, `last_provider`, `vly_device_id`
- `ph_phc_<redacted>_posthog` (PostHog; name contains a public project key,
  value redacted)

Domains: `freebuff.com` + `.freebuff.com` only. Cookie header for probes was
built at runtime from the jar file; values never printed, logged, or stored.

## 1. Page shells (App Router, no redirects)

| Page | Status | Time | Bytes | Notes |
|---|---|---|---|---|
| `GET /chat` | 200 `text/html` | ~460ms | ~57k | `self.__next_f` flight; no `__NEXT_DATA__`; no `Set-Cookie`; no redirect |
| `GET /account` | 200 `text/html` | ~460ms | ~87k | same shell markers; SSR tab labels: `Activity`, `Legal`, `Links`, `Tokens`, `Your usage` |

- Turbopack hashed chunks (`/_next/static/chunks/*.js`, 34 chat / 38 account,
  26 shared). Page HTML carries NO `/api/*` refs — all routes below come
  from the page-unique chunks (8 chat-only, 12 account-only).
- Guessed RSC flight (`RSC: 1` + hand-built state tree) → 500: flight needs
  the real router state; not pursued (chunks sufficed).

## 2. Auth / session (cookie, not Bearer)

| Method+Path | Purpose | Shape (types only) |
|---|---|---|
| `GET /api/auth/session` | NextAuth session (200, ~295B, ~280ms) | `{user:{name:string,email:string,image:string,id:string,stripe_customer_id:string},expires:string}` |
| `GET /api/auth/providers` | Login providers (200, ~524B) | `{github:{id,name,type,signinUrl,callbackUrl},google:{...},apple:{...}}` (all `string`) |
| `GET /api/web/convex-token` | Convex access token (200, ~922B, ~310ms) | `{token:string}` — value redacted, shape only |

Auth mechanism: `__Secure-next-auth.session-token` cookie per request. No
`Authorization` header anywhere on web (vs CLI Bearer on every authed call).
`stripe_customer_id` rides the session user object (billing link without an
extra call).

## 3. Chat section (`/chat`)

Thread model is server-side threads + fetch-streamed replies (no SSE
`EventSource`, no `/api/v1/chat/completions`).

| Method+Path | Purpose | Request shape | Response shape |
|---|---|---|---|
| `GET /api/chat/threads` | List threads (200, ~177B, ~270ms) | — | `{threads:[{id,title,model,updated_at}:string],accessTier:string}` (`"limited"`→limited UI else full; model via `resolveChatModelForAccessTier`) |
| `GET /api/chat/threads/{id}` | Open thread (200, ~611B) | — | `{thread:{id,title,model,reasoning_effort,created_at,updated_at},messages:[{id,role,content,blocks,attachments,model,created_at}]}` (nulls where empty) |
| `POST /api/chat/stream` | Send turn (**POST-only: GET→405**; never invoked — shape from static code) | JSON `{threadId,content,model,reasoningEffort,[retry:true],gravity,images:[{storageId,mediaType,name,descriptionStorageId}],attachments:[{storageId,mediaType,name,chars,truncated}]}` (`Content-Type: application/json`, `AbortController` signal) | Fetch-stream: `body.getReader()` + `TextDecoder`, split `\n`, `data: {json}` lines; `{type:"meta",accessTier,model,...}` events; block assembler (`rootText`/activity blocks, rAF-throttled render). Error: `{error:code-string, message}` fallback `request_failed`, retryable on `>=500 \|\| 429` |
| `POST /api/chat/upload` | File upload (**GET→405**; never invoked) | multipart `FormData {file, model}` | `{storageId,url,chars,truncated,...}` |
| `PATCH /api/chat/threads/{id}` | Rename (observed in code; never invoked) | JSON `{title}` | ok-gated, refetch on failure |
| `DELETE /api/chat/threads/{id}` | Delete thread (observed in code; never invoked) | — | ok-gated, refetch on failure |

- No client-side thread-create route: first `stream` POST carries the
  (null) thread id; the server mints the thread (no `POST /api/chat/threads`
  in any chunk).
- Per-send sidecars (static): `gravity: readGravityCapiData()` =
  `window.gravityPixel?.getCAPIData?.()` (`{event_source_url,client_context,...}`);
  `trackRedditPromptActivity("chat")` fires per send.
- Model picker inputs: `resolveChatModelForAccessTier(model, tier)` +
  `resolveChatReasoningEffort(...)`; `reasoning_effort` also stored per thread.

## 4. Account section (`/account`)

SSR tabs: `Activity`, `Legal`, `Links`, `Tokens`, `Your usage`; client chunks
add Plan/subscription, provider keys, email, country, GitHub, top-up, danger
zone (labels: `Plan sessions today`, `Auto top-up`, `Anthropic API key`,
`OpenAI API key`, `Bedrock bearer token`, `Claude Code provider`,
`Change email`, `Country`, `Delete account`, `Danger zone`, ...).

| Method+Path | Purpose | Shape (types only) |
|---|---|---|
| `GET /api/web/usage-summary` | Streak + usage ledger (200, ~487B, ~710ms) | `{timeZone,todayDateKey,streak:{current,longest,todayUsed,lastUsageDate},activeDates:[string],windowDays,allTimeActiveDays,recent:{days,messages,inputTokens,cacheReadTokens,outputTokens,totalTokens,modelSpendUsd},sessionsByModel:[{model,sessions,units}]}` |
| `GET /api/web/freebuff-session` | Session + meter snapshot (200, ~5196B, ~830ms) | `{status,accessTier,instanceId,model,admittedAt,expiresAt,remainingMs,countryCode,countryBlockReason,ipPrivacySignals:[string],subscription:{tierId,tiers:[14-field tier]},freebucks:{balance,daily:{limit,spent,remaining,resetAt,resetTimeZone},wallet:{balance,monthlyBonus},planId,prices:map<model-id,number>(14 models),priceNotices,offPeak:{model:{startHourUtc,endHourUtc,price,regularPrice}},priceChanges:[{at,modelId,price,tagline}],planRequiredModelIds:[string],upgrade:{kind,cta,tooltip,modelId}},rateLimit:{...entitlementBreakdown{base,referral,streak}...},rateLimitsByModel:{4 models},referral:{code,referrerName,githubLinked,qualifiedCount}}` — tier element: `{id,displayName,priceUsd,yearlyPriceUsd,firstPeriodPriceUsd,dailySessions,fiveDaySessions,monthlySessions,dailyPremiumSessions,disclaimers[],current,upgrade,downgrade,purchasable,yearlyPurchasable[,yearlyPrepaidPurchasable]}` |
| `GET /api/web/subscriptions` (+ `/` alias, same shape) | Subscription state (200, ~1877B, ~440ms) | `{subscription:{tierId,tiers:[tier×3]}}`; `POST` variant observed in code (purchase/change; never invoked) |
| `GET /api/web/freebucks/auto-topup` | Top-up settings read (200, ~104B, ~700ms) | `{enabled:bool,amount,freebucks,disabledReason,lastToppedUpAt,card}`; `POST {amount}` observed (never invoked) |
| `GET /api/account/providers` | Linked OAuth (200, ~24B) | `{providers:[]}` (empty here) |
| `GET /api/account/country` | Country verification (200, ~56B, ~240ms) | `{verification:null,active:bool,nextVerifyAt:null}` |
| `GET /api/account/paid-api/access` | Paid-API visibility (200, ~17B) | `{visible:bool}` |
| `GET /api/account/email` → **405** | Email change is write-only (never invoked) | method UNVERIFIED (POST or PUT) |
| `POST /api/account/delete {code,confirmation}` + `POST /api/account/delete/confirm` | Two-step account delete — **SHAPES ONLY, never invoked** | keys only |
| `POST /api/web/freebucks/buy {amount,sessionId}` | Credit purchase (observed; never invoked) | keys only |
| `POST /api/feedback {message,surface}` | Feedback (**GET→405**; never invoked) | keys only |
| `POST /api/logs` | Client logs (**GET→405**; never invoked) | body keys UNVERIFIED |
| `GET /api/changelog` / `GET /api/github/stars` | `{entries:[{id,date,version,items[]}],lastReadDate,signedIn}` / `{stars:number,url:string}` | public-ish reads |
| `/api/account/delete*`, `/api/imessage/link`, `/api/nodepod/launch` (GET-method, no fetch-method in chunk) | Observed paths only | UNVERIFIED |

## 5. Ads on web (differs from CLI legs)

| Method+Path | Purpose | Request keys |
|---|---|---|
| `POST /api/ads` (**GET→405**, ~3.5s; never invoked) | Web ad fetch (server-rendered Gravity path) | `{adSequenceId,gravity_context,messages,sessionId,surface}` |
| `POST /api/ads/viewer/explanation` | Why-this-ad (never invoked) | `{deliveryRef}` |
| `POST /api/ads/viewer/feedback` | Ad feedback (never invoked) | keys UNVERIFIED |
| `POST /api/gravity/conversion {surface,gravity}` | First-message CAPI conversion; `keepalive`, 5s timeout, `localStorage` once-flag `freebuff_gravity_first_message:<n>` (never invoked) | keys only |
| `POST /api/reddit/conversion`, `POST /api/meta/ads-attribution`, `POST /api/meta/revoke`, `POST /api/paid-social/revoke` | Attribution + consent revoke (observed; never invoked) | keys UNVERIFIED |
| `GET /api/admin/ad-preview/inventory` | Admin preview (observed in chat chunk; never invoked) | UNVERIFIED |

Vendor context (static): surfaces `cli|web|chat|desktop|cloud`, `web` = the
freebuff.com builder (analytics-events.ts:9-16); web/cloud analytics go via
the **Convex send mutation** (PostHog + Axiom direct from Convex); the
`/chat` ads experiment pits server-rendered Gravity ads against the
`@gravity-ai/react` inline slot (`freebuff.chat_ads.experiment_exposed` /
`.ad_shown`, analytics-events.ts:415-420); `bfcid` attribution cookie present
in jar (cf. ADS.md).

## 6. Convex (web realtime/analytics transport)

- `GET /api/web/convex-token` → `{token:string}` mints the browser token.
- Convex host family `convex.cloud` referenced from account chunks; client
  paths `/api/query|mutation|query_at_ts|query_ts|function|action|debug_event`
  are the Convex client calling convention on that host (browser functions
  explicitly allowed: `__convexAllowFunctionsInBrowser`).
- Quota/usage reads above are plain `/api/web/*` GETs, not Convex calls.

## 7. Web-vs-CLI differences

| Axis | Web (this doc) | CLI (ENDPOINTS/HEADERS/MODEL-SELECT) |
|---|---|---|
| Origin | `https://freebuff.com` pages + `/api/*` | API origin (`getWebsiteUrl`), `/api/v1/*` |
| Auth | `__Secure-next-auth.session-token` cookie; `GET /api/auth/session` identity incl `stripe_customer_id` | `Authorization: Bearer` everywhere; identity via `GET /api/v1/me?fields=` |
| UA | Browser Chrome 151 (real page loads) | Bun default (non-chat), `ai-sdk/openai-compatible/1.0.0/codebuff` (chat), `Freebuff-CLI/<ver>` (ads) |
| Chat send | `POST /api/chat/stream {threadId,content,model,reasoningEffort,retry?,gravity,images[],attachments[]}`; threads server-side | `POST /api/v1/chat/completions` OpenAI-shaped + `codebuff_metadata{run_id,client_id,trace_session_id,freebuff_instance_id,llm_step_number,cost_mode}` |
| Stream | fetch `body.getReader()` + `data: {json}` lines, `meta` events, rAF render | SSE via `PromptAiSdkStreamFn` (maxRetries 3); quota from NEXT session poll, never stream |
| Session create | None observed (server mints thread on first stream POST) | `POST /api/v1/freebuff/session/admission` (no body, `x-freebuff-*` headers) + 30s-jitter GET poll + DELETE release |
| Session read | `GET /api/web/freebuff-session` (cookie; full meter incl `prices`, `offPeak`, `priceChanges`, `rateLimitsByModel`, `referral`) | GET poll with `x-freebuff-instance-id` + `x-freebuff-compact-session` |
| Quota/balance UI | `GET /api/web/usage-summary` (streak+tokens+sessions) + freebucks block above | `POST /api/v1/usage {fingerprintId}` → `{usage,remainingBalance,next_quota_reset}` + session poll |
| Streak | `usage-summary.streak{current,longest,todayUsed,lastUsageDate}` + `timeZone/todayDateKey` fields | `GET /api/v1/freebuff/streak` → `{streak,todayUsed,lastUsageDate,timeZone,...}`; timezone via `x-fb-timezone` header |
| Ads fetch | `POST /api/ads {adSequenceId,gravity_context,messages,sessionId,surface}` + viewer + conversion legs | `POST {API_ORIGIN}/api/v1/ads {provider,messages,sessionId,device,...}` + impression/click + ZeroClick/first-party legs |
| Attribution | `bfcid` cookie + Gravity/Reddit/Meta pixels + `/api/*/conversion` | `bfcid` header/cookie only at Stripe stamp time |
| Threads/history | `GET /api/chat/threads[/{id}]`, `PATCH {title}`, `DELETE` | No web threads; agent-runs `START/FINISH` ledger instead |
| Uploads | `POST /api/chat/upload` multipart `{file,model}` → `{storageId,url,chars,truncated}` | No CLI equivalent on chat path |

## 8. Quota / balance display sources (web)

1. `GET /api/web/usage-summary` — streak, daily token ledger, per-model sessions.
2. `GET /api/web/freebuff-session` → `freebucks{balance,daily,wallet,prices,offPeak,priceChanges,planRequiredModelIds}` + `rateLimit/rateLimitsByModel{limit,recentCount,resetAt,resetTimeZone,entitlementBreakdown}` + `subscription.tiers[]`.
3. `GET /api/auth/session` → `user.stripe_customer_id` (billing link).
4. `GET /api/web/freebucks/auto-topup` → `{enabled,amount,freebucks}`.

## 9. UNVERIFIED (explicitly not probed — would need mutations or a send)

- Chat ping deliberately NOT sent: send-shape recovered statically to body-key
  precision, so no ping was required. Live stream bytes, SSE-vs-framing edge
  cases, and `meta`-event vocabulary beyond `accessTier/model` unverified.
- All POST/PUT/PATCH/DELETE bodies beyond key names (delete confirm flow,
  buy/subscriptions/topup/email/feedback/logs/ads/viewer-feedback/revoke
  legs, upload response tail, thread-POST-create absence).
- `freebuff-session` poll cadence (no timer near the fetch in-chunk; likely
  mount-fetch only).
- `/api/nodepod/launch`, `/api/imessage/link`, `/api/admin/ad-preview/inventory`
  purpose + methods.
- Convex mutation/query names + payloads (token minted, no calls made).
- Server analytics payloads for `freebuff.chat_ads.*` (names only, per vendor).
- `__Host` cookie prefix enforcement details; session expiry/rotation.
- Whether `/chat` 200 shell differs authed vs anonymous (only the authed
  shape was fetched; no logged-out control — would need a clean profile).

## 10. Secret-sweep statement

- Jar file untouched in place (size + mtime verified post-run), still
  untracked via gitignore (`/this are web dump cookies`).
- This doc contains: endpoint paths, HTTP methods, status codes, timings,
  byte sizes, JSON key names + value TYPES, public model ids (catalog data),
  UI label strings. No cookie/token/session/thread/user values, no emails,
  no names, no `bfcid`/pixel ids, no Convex token, no `stripe_customer_id`
  value, no response bodies beyond shapes.
- `06-web-shapes.sh` (companion, dry-run default) takes `COOKIE_JAR` as an
  env/file pointer, builds the `Cookie` header at runtime, redacts all JSON
  scalars to types via `jq walk`, and embeds no values.
