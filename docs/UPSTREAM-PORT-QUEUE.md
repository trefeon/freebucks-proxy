# UPSTREAM-PORT-QUEUE — what the vendor has that we haven't ported

Pin state: `scripts/vendor-version.txt` = `0.0.204`,
`backend/internal/wirefacts/wirefacts_gen.go:7` UpstreamSHA `775383b3`.
Vendor tip checked here: `57943aa71` (gitignored `upstream/freebuff`,
`origin/main`). Live since 2026-09-27: npm `0.1.0` (wrapper + all 8 binary
hashes rotated), CLI tag `freebuff-v0.1.0`, Desktop latest
`freebuff-desktop-v0.0.150`, vendor tip `ede39b345` (48 sync commits past
pin). Functional delta so far: new `x-freebuff-client: desktop` header
const (Desktop-only send; CLI sends nothing — proxy absence stays
CLI-shaped), peak-hours TZ hardening, placements/inline-ad/partner-trigger
changes. Re-pin rides the drift bot, not hand edits.

Recently ported (do NOT re-queue): `complete_compaction` anti-ban signal
(`convert/foreign_signals.go:114-118`); `model_unavailable` refusal + window
cache + fallback + metric (#158: `session/session_admission.go:659`,
`session/model_unavailable.go`, `MODEL_UNAVAILABLE_CACHE_TTL` 1h);
`premium_slot_taken` + purchase arms terminal (#8: `upstream/classify*.go`,
`session/session_poll.go:118-132`); off-peak pricing fixture; 0.0.204
registry (9 served models).

## Queue (impact × effort)

### P1 — SessionState parse gaps (S effort, M impact)
`upstream/common/src/types/freebuff-session.ts:829-1265` carries
`desktopPurchases`, `desktopRefunds`, `desktopSessionCounts`,
`freeWindows` which `upstream/session_parse.go:13-202` drops. We already
hit this live: a `409 purchase_capacity` names `currentInstanceId` +
`nextExpiryAt` that the parsed state cannot surface, forcing manual
forensics. Parse + expose (no behavior change), then use in admission
error messages.

### P2 — Compact-merge carry fields (S effort, M impact)
Vendor merges `rateLimit/rateLimitsByModel/subscription/freebucks` on
compact polls (`freebuff-session-api.ts:301-328`); proxy sends the compact
header (`upstream/session.go:164`) but merge ownership is pool-side and
unverified. Stale quota displays after compact polls. Verify + port the
merge.

### P3 — Refund-pending replay holder (M effort, M impact)
Vendor `cli/src/state/freebuff-session-store.ts:121-201` replays
refund-pending state across polls; proxy has no equivalent — a refund that
lands between polls can be missed, mis-stating balance. Port the holder.

### P4 — Tokenhealth admission-visibility gap (S effort, M impact, observed live)
Ban is enforced at admission POST, invisible to the GET-based probe:
dashboard showed `ban: None` while admission returned banned on both tokens
(pool quarantined both). Admission-side terminal states should feed back
into tokenhealth so the Tokens page reflects reality. (Recorded 2026-09-27
against `vps-us`; no upstream change needed — proxy-side fix.)

### P5 — Wirecode vocab for purchase/slot statuses (S effort, L impact)
`upstream/wirecodes_gen.go` has no `model_unavailable`,
`premium_slot_taken`, `purchase_*` constants — session layer matches raw
strings (`session_admission.go:659`, `session_poll.go:118`) while
`classify.go` speaks only `WireCode*`. Behavior is correct; add the
constants + snapshot source comments for consistency (watch: `wiregen`
inputs — generated file, edit via the tool, not by hand).

### P6 — Notices/spend-ceiling copy at tip (S effort, L impact)
`upstream/common/src/constants/freebuff-spend-ceilings.ts:5-15` and
`tier-change` source `upstream/common/src/util/freebuff-model-availability.ts:26-27`
postdate the pin; `upstream/notices_gen.go:1-39` is generated at pin.
Re-check copy at next re-pin (bot covers if snapshots refresh; hand-verify
`dashboard/data/upstream_drift.json`).

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
