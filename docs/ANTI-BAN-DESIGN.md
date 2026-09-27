# ANTI-BAN design — what keeps accounts alive through this proxy

Threat model first, then the controls. Every claim cites code or observed
wire behavior. Companion: `docs/CLI-WIRE-TRACE.md` (what genuine clients do),
`docs/UPSTREAM-PORT-QUEUE.md` (accepted gaps and why).

## 1. What the server detects (evidence)

1. **Fanout shape.** The wire has a dedicated marker:
   `free_mode_run_fanout` — "the account's concurrent-run counter looked
   like proxy fanout" (`upstream/wirecodes_gen.go:13-15`). Concurrent runs
   from one account beyond human plausibility is a first-class signal, not
   an inference.
2. **Worker/datacenter egress.** `common/src/constants/cf-worker-signals.ts`
   (edge-stamped worker detector lineage) + `free_mode_unavailable` for
   `anonymous_network` egress (`upstream/classify.go:133-140`). The ban we
   took rode on datacenter IPs.
3. **Session churn.** Repeated admissions/claims/refunds in minutes
   (our 2026-09-27 incident: ~15 claim-DELETE cycles on one account in
   ~15 min across two proxies sharing tokens → both accounts hard-banned).
   Upstream counts purchase claims, not just spend.
4. **Account sharing.** Same token from two egresses/concurrency patterns
   (vps-sg + vps-us shared `AUTH_TOKENS` 05:36–06:0x) reads as sharing/farm.
   `superseded` ping-pong is the visible symptom; the ban is the invisible one.
5. **Signup integrity.** 9 signup-block reasons + Turnstile/reCAPTCHA
   (`common/src/constants/freebuff-signup-block.ts`, `env-schema.ts`) —
   fresh/shallow accounts (no established GitHub) get suspended. The
   `account_banned` hint says it outright: use established GitHub logins.
6. **Country/tier.** US + 13 + 10 allowlist, VPN → limited, hard-gated
   server-side by IP (`common/src/constants/freebuff-countries.ts`).

## 2. How genuine clients behave (the model to follow)

- **One process, one seat.** A CLI holds ONE active session per account and
  polls it every 30s±20%; `superseded` is terminal — it never fights, it
  re-joins fresh (`use-freebuff-session.ts:90-125`). Desktop caps premium
  models at one active session per user (`slotLimit: 1`,
  `freebuff-desktop-sessions.ts`).
- **Admission is rare.** Startup GET-probes first; POST only on
  join/rejoin/takeover/model-change (`:591-595,686-700`). POST retries only
  408/429/503 — never blind-retry (a POST may already have committed).
- **Spend is paced.** Slots pace bursts; per-turn spend ceilings kill runaway
  turns (`turn_spend_limit` terminal for the turn, session survives).
- **Render-gated legs fire only on render.** Clicks, first-party acks,
  zeroclick pixels, offer rail — absent without the UI event. A headless
  client that fires them fabricates fraud-shaped rows; one that omits them
  matches the server-fallback contract (`ad-request.ts:99-104`).

## 3. Proxy controls (mapped to threats)

| # | Threat | Control (shipped) | Enforcing code |
|---|---|---|---|
| 1 | Fanout | One seat per account: single active premium session bound; slots pace bursts; superseded stays terminal, never ping-pong | `pool/`, `session/session_poll.go:118`, `upstream/session.go` takeover-absent |
| 2 | Churn | Admission POSTs never transport-retry; `model_unavailable` window cache (no 409 churn); poll cadence = CLI 30s±20%, backoff 20s→300s | `upstream/client_chat.go` R5 guard, `session/model_unavailable.go`, `pool/pool_lifecycle.go:26-47` |
| 3 | Sharing | One proxy per account set, ever. No concurrent proxies, no token reuse across hosts | Operational rule (memory: vps-us exclusively) |
| 4 | Wire mismatch | Byte-parity headers/UAs/envelope/fingerprint-shape at tip; verbatim loginUrl; Bearer-only agent-runs | `upstream/*`, `wirefacts/*`, `docs/CLI-WIRE-TRACE.md` |
| 5 | Fraud-shaped rows | Never fire click/ack/pixel/offer without the UI event; honest-values-only auction/impression | `upstream/ads*.go`, tests `TestAuctionBodyPinsHonestSignals` |
| 6 | Dead accounts | Quarantine on terminal states (`banned`, `account_suspended`, `country_blocked`); pool skips quarantined | `pool/` quarantine, `upstream/classify.go:36-46,160` |
| 7 | Egress | Residential/US egress; `SAFE_MODE=true` anti-ban preset; `COST_MODE=free` | `.env.example`, operational rule |

## 4. Operational rules (non-negotiable)

1. One proxy ↔ one account set. Never share `AUTH_TOKENS` across hosts.
2. Fresh accounts = established GitHub logins, added ONE at a time via the
   dashboard login wizard (verbatim URL), then a single verification chat.
3. No probe hammering: refusals don't burn, but admissions/claims do —
   every admission is a purchase-claim-shaped event. Debug against the mock,
   not upstream.
4. Dashboard `ban: None` is not ground truth — only an admission attempt is
   (probe GETs don't see admission-enforced bans). Verify liveness by
   admitting, not by polling.
5. On `superseded`: end the loser, never re-fight. On slot refusal: wait
   for `nextExpiryAt`, never hammer.

## 5. Residual risks (accepted, monitored)

- Fingerprint is shape-stable but synthesized — binds to no genuine install
  (mitigated: stable per host, isolated per account).
- Datacenter egress is inherently weaker than residential — mitigated by
  single-proxy discipline + `SAFE_MODE`, not eliminable.
- Vendor moves the tip; pins + comments drift between re-pins — covered by
  the drift bot flow (`upstream-drift.yml`), never hand-pinned.
