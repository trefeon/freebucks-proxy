# DIVERGENCES — 9 proxy-vs-CLI divergences (static, pin 0.0.194)

> Version: npm freebuff 0.0.194 @ 08b5a38f3 (re-pinned 2026-09-24). Proxy side = repo backend (current); CLI side = pin-0.0.194 vendor tree. No tokens/hosts.

| # | Title | CLI does | Proxy assumes/diverges | Evidence |
|---|---|---|---|---|
| D1 | Onboard rewrite | Opens loginUrl verbatim | Rewrites loginUrl to the onboard path preserving only auth_code | proxy backend/internal/upstream/auth_login.go:172-177 vs upstream/freebuff cli/src/login/plain-login.ts:54 |
| D2 | Synthesized fingerprint | One machine id per process: enhanced sha256 hardware JSON -> `enhanced-<hash>` (fingerprint.ts:84-129), legacy `codebuff-cli-<8rand>` (:135-138), process-cached (:152-157) | Synthesizes hash from MAC/hostname/cpu digests + pinned persona literals (backend/internal/upstream/login/fingerprint.go:106-184): same `enhanced-`+base64url shape, NOT byte-identical to real hardware; isolated-login random fingerprint per account (auth_login.go:118-123) | fingerprint.ts:84-138 vs login/fingerprint.go:106-184; auth_login.go:118-123 |
| D3 | expiresAt echo | Types expiresAt string (ISO instant) (codebuff-api.ts LoginCodeResponse) | Echoes wire string verbatim (ExpiresAtRaw; auth_login.go:87-94,195-220); numeric/millis tolerated | auth_login.go:187-229; codebuff-api.ts type |
| D4 | Single-shot poll | 5s/5min loop in client (login-flow.ts:112-204) | PollCLILogin single-shot (Done=false on 401/5xx/transport; auth_login.go:231-310); 5s/5min loop lives in proxy callers (pollForCompletion :416-443), not the client | login-flow.ts:112-204 vs auth_login.go:231-310,416-443 |
| D5 | Wallet/first-tab 0 | Sends wallet String(limit ?? 0) + first-tab 1\|0 per offer state (freebuff-session-api.ts:163-179) | Session POST always wallet '0' + first-tab '0' (session.go:26-32,98; 497-504,318-319): correct only for no-consent/no-offer; consent_required / first_tab_discount_changed surface as 409, proxy cannot complete them | session.go:26-51,98 vs freebuff-session-api.ts:163-179 |
| D6 | Pinned chat UA | Live package VERSION in ai-sdk UA (model-provider.ts:432) | Pinned literal ai-sdk/openai-compatible/1.0.0/codebuff (client.go:137; code:126-137) — silent drift on vendor bump | client.go:126-144 vs model-provider.ts:432 |
| D7 | Transient retry | Absorbs deferral in AI SDK + session poll loop (model-provider.ts:41-49,62-81; freebuff-session-api.ts:48-72) | In-request same-session retry (capacity-deferred/waiting-room, retry-after floor 10s; chat.go:91-208) — no direct CLI counterpart | chat.go:91-208 |
| D8 | Known-status subset | Full FreebuffSessionAdmissionResponse union (freebuff-session.ts:828-1160) | wireKnownStatuses subset (emit_wire.go:124-126); takeover_prompt/superseded local-only CLI states (freebuff-session.ts:5-9) must never be expected on wire | emit_wire.go:124-126 vs freebuff-session.ts:828-1160,5-9 |
| D9 | No web/server | (n/a — client tree only) | Server code (web/ chat _post, session handlers) NOT in vendor clone (no web/ dir) — server-side gate/envelope verified from CLI+SDK call sites only | vendor clone layout; backend/internal/wirefacts/testdata/wire file list |

Extra proxy-only deltas noted in ModelSelectFlow (not counted above): LimitedModelID proxy=mimo/mimo-v2.5 vs upstream LIMITED=deepseek flash + HERO=GLM flash (deliberate); proxy refuses paused/tier-gated with WithdrawnModelMessage vs CLI in-memory flip to FALLBACK + re-POST; probeModel=CheapestFreeIn→Default→catalog order vs CLI hero=DEFAULT; catalog order kept for /v1/models vs CLI metered cheapest-first; pool PIN_MODEL/quarantine/cooldown/Freebucks pre-gates (server verdict authoritative).

## UNVERIFIED
- Whether 0.0.194 closes or widens any of D1–D9 (live capture required).
- Server-side truth for D7/D9 (no web/ source).

## 0.0.194 pin delta (2026-09-24, 276db8d -> 08b5a38f3)
- Registry pins (6/6) SAME: catalog/notice/status sources unchanged at the pin, so D8 (session-types SAME) and notice copy (spend-ceilings/model-availability/peak-hours SAME) stand as written.
- Wire drift (classified BEFORE baseline refresh): FUNCTIONAL in common/src/tools/constants.ts (new `report_project_profile` tool) and packages/agent-runtime/src/run-agent-step.ts (project-profile offer/report loop); other 11 wire files SAME.
- Neither FUNCTIONAL row needs a Go-side port: wiregen extracts only param constants from tools/constants.ts (toolnames_gen header-only change), and the proxy runs no agent-step loop. D1-D7 files sit outside the pin set and were not re-verified by this re-pin.
