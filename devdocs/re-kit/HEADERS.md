# HEADERS — auth, content, timezone, freebuff set, UA personas, TLS

> Version: live CLI 0.0.194 vs static pin 0.0.193. All tokens/hosts redacted.

## Authorization
- Bearer `<redacted-token>` on all authed calls (upstream/freebuff cli/src/utils/codebuff-api.ts:338-340; chat same sdk/src/impl/model-provider.ts:426-435; proxy backend/internal/upstream/client.go newRequest + backend/internal/upstream/auth_login.go:50-58 token-less NewForAuth skips auth).
- Omitted on code/status poll (includeAuth:false) (codebuff-api.ts:516-536).
- Legacy Cookie next-auth.session-token option (codebuff-api.ts:341-343). Proxy TokenKey is sha256 hex of raw token, token never persisted (backend/internal/upstream/client.go:119-124).

## Content-Type
- application/json iff body !== undefined — bodyless session POST carries none (codebuff-api.ts:344-346; proxy backend/internal/upstream/client_chat.go:58-63; session.go:80-86 notes bare POST).

## x-fb-timezone
- IANA zone per request via Intl (upstream/freebuff common/src/util/freebucks-timezone.ts:1,17-26); stamped on EVERY session call incl. DELETE (freebuff-session-api.ts:163-167; proxy session.go:497-504,318-319). Server derives account daily reset zone from it at admission (session.go:490-496). Proxy: resolver value when installed else host zone, Local->unset, unparseable->UTC (session.go:196-223).

## x-freebuff-* set
- x-freebuff-model: POST only, when picked (freebuff-session-api.ts:151-185; proxy session.go:95-97).
- x-freebuff-instance-id: GET/DELETE when held, never on POST (freebuff-session-api.ts:163-179; proxy session.go:124-126,302-346).
- x-freebuff-compact-session: 1 on GET when previous==active (freebuff-session-api.ts:261-291; proxy session.go:127-129).
- x-freebuff-wallet-spend-limit: POST String(limit ?? 0) (freebuff-models.ts:2609-2614; proxy always "0" session.go:26-32,98).
- x-freebuff-first-tab-discount: 1|0 on EVERY session call (freebuff-session-api.ts:163-179; proxy always "0" session.go:43-51).
- x-freebuff-reuse-instance-id + /api/v1/freebuff/session/reuse: pinned strings (freebuff-models.ts:3415-3423) but NOT called by CLI (proxy session.go:20-23).
- x-freebuff-acting-user-id: token's OWN /me id only (model-provider.ts:430-435; database.ts:391-394,470-473; proxy session.go:348-359 + chat.go:136-150). Foreign value = impersonation risk.
- x-freebuff-multi-session / x-freebuff-heartbeat: Desktop-only (models.ts:3462-3467; proxy omits heartbeat deliberately session.go:110-115).

## 3 UA personas
1. Plain Bun default on ALL non-chat calls, no override — login/session/agent-runs/streak/usage (proxy bunUserAgent Bun/1.3.14 backend/internal/upstream/client.go:139-144; login authLoginRequest :60-78).
2. Chat ONLY: ai-sdk/openai-compatible/<VERSION>/codebuff (model-provider.ts:432; proxy pins literal ai-sdk/openai-compatible/1.0.0/codebuff backend/internal/upstream/client.go:126-137; set at chat.go:130). No Accept header on chat (chat.go:122-126).
3. Ads API: Freebuff-CLI/<cli-version> (cli/src/hooks/use-gravity-ad.ts:817-821).

## TLS / retry
- codebuff-api.ts:197-244 detects cert errors (SELF_SIGNED/EXPIRED/ALTNAME) and NEVER retries them; other network errors retried (default 30s timeout, 3 retries exp-backoff 1s->10s +30% jitter on 408/429/500/502/503/504: codebuff-api.ts:84-89,255-263,304-306).

Sample (redacted): `GET {API_ORIGIN}/api/v1/freebuff/session` H: `Authorization: Bearer <redacted>`, `x-fb-timezone: <IANA>`, `x-freebuff-instance-id: <redacted>`, `User-Agent: Bun/<redacted>`.

## UNVERIFIED
- Server UA fingerprinting weight (403 free_mode_cli_required keys on envelope per client.go:126-136 comment — server code absent).
- 0.0.193 vs 0.0.194 UA/version drift (chat UA carries live package version; proxy literal may lag vendor bump).
