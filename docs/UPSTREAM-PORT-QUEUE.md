# UPSTREAM-PORT-QUEUE — what the vendor has that we haven't ported

Pin state: `scripts/vendor-version.txt` = `0.2.1`,
`backend/internal/wirefacts/wirefacts_gen.go:7` UpstreamSHA `a2fd4806`
(npm `freebuff` 0.2.1 published from this commit — verified in-clone).
Vendor tip checked here: `25f1d6153` (gitignored `upstream/freebuff`,
`origin/main`, fetched 2026-09-28). Live: npm `0.2.1`, CLI `0.1.1`
(release package at tip), Desktop latest `freebuff-desktop-v0.0.150`.
0.1.1 assessed 2026-09-28 (25 files, §14.11): **no wire port needed** —
abort/watchdog/banner fixes, compaction heuristics, BYOK internals; zero
new routes/headers/statuses. Re-pin rides the drift bot, not hand edits.

Recently ported (do NOT re-queue): wire port 0.2.1 (a2fd480, lane drift/wire-021):
`streakBonus` / `note` on the daily Freebucks pool (opaque parse in
`upstream/session_types_freebucks.go`, `TestParseFreebucksStreakBonusNote`);
`FREEBUFF_TIER_CHANGE_NOTICE` deleted upstream → retired copy in
`wirefacts/emit_wire.go:wireRetiredNotices` (dashboard announcement card
retired in drift/dashboard-021); availability
residential-proxy branch + VPN/Freebucks reword, peak-hours
`isSupportedTimeZone` + zone validation, picker `dealEndingSoon` /
`promotional` chips, agent-runtime `onFinishReason` + compaction rework —
all verified CLI/agent-local with no proxy mirror, snapshots carried. No
proxy behavior change: `streakBonus` / `note` are display-only (admission
and charging still use `limit` as sent) and the retired tier copy is
byte-identical to the previously served value.
Registry port 0.2.1 (a2fd480, lane drift/registry-021): `free-agents.ts` /
`freebuff-models.ts` re-pinned (byte-identical testdata mirrors, registry pin
hashes refreshed, `modelcat/catalog_gen.go` regenerated via wiregen). New row
`openai/gpt-6.1-sol` (GPT-6.1 Sol, OpenAI flagship on flex, promotional 100
Freebucks, US-or-paid gated) in every agent map + picker + plan/paywall lists;
Muse Spark 1.3 returns from PAUSED as a plan-gated tier row (tagline `Falls
back when busy`, OpenRouter lane, efforts capped at high); 1.2 leaves every
picker for the same paywall (unserved, tierless, still recognized — the
retired-from-picker row); GPT-6 Luna drops `max` from its ladder, Gemini 3.8
fixes at `high`, GLM 5.3 Flash gains a `dealEndingSoon` notice (Sep 30 → 15/hr).
Served set 9→8 (`/v1/models` 11→12 with the 4 tier rows, healthz/metrics 9→8).
New picker pill fields (`promotional`, `dealEndingSoon`, `newBadge`) are
presence-validated and skipped by the emitter (client-side marketing, no
admission meaning); the proxy NEW badge still derives from `isNew`.

`complete_compaction` anti-ban signal
(`convert/foreign_signals.go:114-118`); `model_unavailable` refusal + window
cache + fallback + metric (#158: `session/session_admission.go:659`,
`session/model_unavailable.go`, `MODEL_UNAVAILABLE_CACHE_TTL` 1h);
`premium_slot_taken` + purchase arms terminal (#8: `upstream/classify*.go`,
`session/session_poll.go:118-132`); off-peak pricing fixture; 0.2.1
registry (8 served models).

CLI-sweep ports (2026-09-28, do NOT re-queue): P1 SessionState parse gaps
(`desktopPurchases/desktopRefunds/desktopSessionCounts/freeWindows` +
`purchase_capacity` holder, opaque passthrough in `upstream/session_parse.go`);
P2 compact-merge carry (single `session.MergeCompactSnapshot`, pool
delegates, wired into the `session_poll.go` active path with `forceFullPoll`);
P3 refund-pending replay holder (`session_poll.go` compact-GET fold-in);
P4 admission→tokenhealth feedback (`NoteAdmissionTerminal` sticks
BANNED/COUNTRY_BLOCKED/INVALID, folds over OK/UNKNOWN probes only);
P5 wirecode consts (`WireCodeModelUnavailable/PremiumSlotTaken/
PurchaseClaimReleased/PurchaseInUse/PurchaseCapacity` via wiregen regen,
match-site swaps only). P6 notices copy verified SAME (no action).

## Queue (impact × effort)

### P1 — SessionState parse gaps — PORTED 2026-09-28 (see Recently ported)

### P2 — Compact-merge carry fields — PORTED 2026-09-28 (see Recently ported)

### P3 — Refund-pending replay holder — PORTED 2026-09-28 (see Recently ported)

### P4 — Tokenhealth admission-visibility gap — PORTED 2026-09-28 (see Recently ported)

### P5 — Wirecode vocab for purchase/slot statuses — PORTED 2026-09-28 (see Recently ported)

### P6 — Notices/spend-ceiling copy — VERIFIED SAME 2026-09-28, no action (re-pin bot owns pin-age lag)

### P7 — Setup writers for roo/cline/goose/qwen/kilocode/pi/omp (S effort, L impact)
Folded from `docs/CLI-Limitations.md` (purged 2026-09-28, pin 0.0.178): `cli/setup/setup.go`
covers only Continue/opencode/aider; the rest stay manual. No harness impact beyond setup UX.

### P8 — Dead-owner takeover resolution (M effort, M impact)
Folded from CLI-Limitations P1-2: proxy surfaces terminal `503 session_superseded`,
never resolves (pid-file mechanism is single-user WONT). Open: steer shared-token
clients (omp/pi multi-session) on takeover instead of a dead screen.

### P9 — `model_locked` GET→DELETE→POST auto-repick (M effort, M impact)
Folded from CLI-Limitations P1-3: refusal is terminal `409 model_locked` with
re-pick copy; no repick — a stale row on another model costs a failed turn.

### P10 — Country-block best-effort DELETE (S effort, L impact)
Folded from CLI-Limitations P1-4: `403 country_blocked` invalidates cache but
sends no upstream DELETE; the row lingers server-side until natural expiry.

## Accepted gaps (do NOT port — reasoning recorded in code)

- Agentic-offer rail (`POST /api/v1/ads/agentic/offer`,
  `common/src/ads/agentic-offer.ts:50`): needs project-root fs+git
  capability + thread ids + card UI; headless firing mints unreadable
  server-side rows. Reasoned in `upstream/ads_chat.go` header.
- First-party ack transport (`use-gravity-ad.ts:391-461`): gate opens only
  for provider `first_party`; proxy auctions gravity/zeroclick only —
  unreachable. `postAdEvent` fall-through already mirrors the CLI path.
- Auction extras (`sessionId`, `traceContext`, capability, `placementIds`,
  `cliDockArm`) + impression `mode`/`renderDelayMs`: omitted
  honest-values-only per `cli/src/ads/ad-request.ts:99-104` (server
  fallback beats no ad). Pinned by `TestAuctionBodyPinsHonestSignals`,
  `TestImpressionBodyPinsNoRenderSignals`.
- Click leg: never fire (fraud-shaped). Zeroclick pixel: needs
  impressionIds the parse drops.
- `/api/logs` telemetry + PostHog: wrong principal, correctly absent.
- Browser TLS/canvas, Turnstile/captchas: unreproducible by a Go gateway;
  egress discipline is the control.
- `x-freebuff-takeover-instance-id`: never sent — no user-confirmed holder;
  `session_superseded` stays terminal to avoid ping-pong
  (`upstream/session.go` const block).
- Sponsored-run settle timers (`sponsored-run.ts`, `exit-cleanly.ts`): CLI-only
  concept, no harness drives sponsored runs through the proxy. Folded from
  CLI-Limitations P1-5.
