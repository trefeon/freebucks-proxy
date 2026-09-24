# SESSION — none→active→ended machine, chat gates, backoff

> Version: live CLI 0.0.194 vs static pin 0.0.193. All tokens/hosts redacted.

## State machine
none (pre-join; carries rateLimitsByModel picker snapshot, referral, limitedModelOffers, subscription, freebucks meter) --POST(model)--> active {instanceId, model immutable, admittedAt, expiresAt, remainingMs, rateLimit, freebucks} --GET poll--> active…; active --expiry/sweep--> ended {instanceId? grace: chat OK, no new prompts; no instanceId: fully gone, rejoin via POST} --GET--> none.
- Statuses union: upstream/freebuff common/src/types/freebuff-session.ts:809-1153; none :824; active :865-894; ended :898-930 (refund fields); country_blocked :932-937; banned :1027; model_locked :944-948; model_unavailable :951-1026; first_tab_discount_changed :810-813; consent_required :815-822; rate_limited :1050-1087; spend_limited :1089-1099; ip_capped :1036-1048; premium_slot_taken :1122-1140; purchase_* :1101-1120 (Desktop-only).
- takeover_prompt/superseded are local-only (freebuff-session.ts:5-9; use-freebuff-session.ts:783-804): startup probe hit live seat -> ask before POST rotates; dead-local-process row auto-takes-over. Proxy never auto-takeovers on superseded.
- Routing: every non-admitted status renders pre-chat landing except superseded (own screen) and ended (falls through to `<Chat>`) (cli/src/app.tsx:364-405).
- Ownership: freebuff-instance-owner.json {instanceId,pid} (freebuff-instance-owner.ts:45-58); liveness process.kill(pid,0), EPERM=running (:35-43); match+dead pid = silent-takeover trigger (:60-66).

## POST response decoding (freebuff-session-api.ts:187-256)
- POST 404/405 -> session_admission_unsupported (fail closed); any 404 -> {status:'none'}; 403 country_blocked|banned -> state, else throw; POST 409 model_locked|model_unavailable|first_tab_discount_changed|consent_required -> body; POST 429 rate_limited|spend_limited|ip_capped -> body; else throw with Retry-After (seconds|HTTP-date->ms) + body error string.

## Chat-request gate (freebuff-session.ts:1200-1244, matched on error+status pairs, never message)
- endsTheSession=true -> waiting_room_required 428, session_expired 410, session_superseded 409, session_model_mismatch 409 (forget window, re-admit same instance).
- survivable -> session_limit_reached 409, waiting_room_queued 429 (transient race), model_unavailable 410 (keep window, pick another model).

## Backoff / timing constants
- Active poll POLL_INTERVAL_ACTIVE_MS=30000 ±20% jitter, clamp max(1000,min(jittered,remainingMs+1000)) (use-freebuff-session.ts:63,76-88; polling-backoff.ts:46-59).
- ended polls only while instanceId present; else null/stop (use-freebuff-session.ts:89-110).
- Failure classify: GET retryable 408/429/5xx, POST only 408/429/503 pre-commit (freebuff-session-api.ts:48-73); tick backoff 20s base doubling to 300s cap + Retry-After honored (polling-backoff.ts:3-44); session fetch 20s timeout abort-combine (freebuff-session-api.ts:17,90-96); codebuff-api default 30s/3 retries 1s->10s +30% jitter on 408/429/500/502/503/504.
- Proxy chat transient: same-session retry capacity_deferred/waiting-room, retry-after floor 10s, budget TRANSIENT_RETRIES (backend/internal/upstream/chat.go:91-208).

## UNVERIFIED
- Server grace-window duration for ended-with-instanceId; server-side expiry vs remainingMs clock source.
- 0.0.193 vs 0.0.194 timing changes (re-verify on live capture).
