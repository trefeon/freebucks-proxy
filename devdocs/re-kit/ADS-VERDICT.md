# ADS-VERDICT — chat-surface ads mirror: feasibility verdict

> Verdict: **DO NOT MIRROR the chat surface.** Keep the waiting-room chain
> only, document this decision, revisit only on server evidence.
> Sources: `devdocs/re-kit/ADS.md` (live CLI 0.0.194 vs static pin 0.0.193),
> `devdocs/re-kit/PORT-MAP.md` §8 + §10 item 4, proxy
> `backend/internal/upstream/ads.go`. No tokens/hosts; samples redacted.

## Verdict

A faithful proxy-side mirror of the CLI chat ads surface (`cli_chat`) is
**not feasible without a rendered transcript**, and attempting one would
fabricate engagement signal. The proxy therefore mirrors **only the
waiting-room chain** (`FireWaitingRoomChain`, `surface:"waiting_room"`,
fired on the server 428 `waiting_room_required` gate) and leaves the entire
chat surface (`cli_chat` auction, inline pool, rotation, acks, pixels)
unmirrored. This matches PORT-MAP §8 row 2 (`MISSING`) and §10 item 4.

## What a faithful mirror would need

Each item below is something the CLI chat surface does that a faithful
proxy mirror would have to reproduce (CLI refs via ADS.md §gaps(2),
PORT-MAP §8):

1. **`cli_chat` auction** — `POST {API_ORIGIN}/api/v1/ads` with
   `surface:'cli_chat'`, `inlinePlacementId:'CLI-Chat-Inline'`,
   `slotPlacementId:'Single-Ad-Unit-1'`
   (`cli/src/chat.tsx:200-222`; `use-gravity-ad.ts:528-562,768-789`).
2. **`CLI-Chat-Inline` pool (≤4)** — `requestResponseAds(messageId,count)`
   fills up to `MAX_RESPONSE_AD_POOL_SIZE` (4) per eligible answer
   (`isInlineAdEligibleAnswer`: ai-variant, id prefix,
   `metadata.allowInlineAds`); pool reused without new auctions/impressions
   past 4 (`use-gravity-ad.ts:683-748`). Per-answer analytics
   `cli.inline_ad_slot_eligible` + `cli.inline_ad_pool_reused` (`:704-717`).
3. **`Single-Ad-Unit-1` rotation** — `SingleAdBanner ad=ads[0]` re-renders /
   fires per rotation of `ads[0]` (`chat.tsx:1927-1948`;
   `ad-banner.tsx:675-676`).
4. **60 s / 3-per-30 s cadence** — immediate fetch + 60 s interval
   (`AD_ROTATION_INTERVAL_MS`, `use-gravity-ad.ts:656-676`); fetch allowed
   only if `adsShownSinceActivity<3 && isUserActive(30s)`
   (`MAX_ADS_AFTER_ACTIVITY`, `ACTIVITY_THRESHOLD_MS`, `:603-619`), counter
   reset on activity (`:642-647`).
5. **Choice cache** — on fetch miss, round-robin cache (cap 50, dedup by
   first impUrl, ZeroClick excluded, `:125-143,621-634`).
6. **ZeroClick pixel** — `POST https://zeroclick.dev/api/v2/impressions
   {ids: impressionIds}` then local impression (`:395-421`).
7. **First-party ack** — `acknowledgeFirstPartyView({token:impUrl, url, init,
   surface:surface??'cli_chat', placementId, clientFamily:'cli',
   [renderDelayMs]})`, 2 s timeout x3, 10 s ceiling
   (`:305-343`; `first-party-view-ack.ts:47-79`).
8. **`sessionId` + message history + placementIds** — auction body carries
   `sessionId` (chatSessionId), `messages: AdMessage[]` (user+assistant text
   only, `INSTRUCTIONS_PROMPT` excluded, latest UI user message appended as
   `<user_message>`, `:492-520,768-789`), `[sponsoredCapability,
   capabilityInspection]`, `[surface]`, `[placementId]`, `[placementIds]`
   (`:799-815` device block: os/timezone/locale).
9. **Dock dwell fields** — click body `{impUrl, clientEventId, [surface],
   [dockFrom, dockDwellMs, dockAccidentalClick]}` (`:446-460`); dock/panel
   origin + dwell + <300 ms accidental label via `DOCK_ACCIDENTAL_CLICK_MS`
   (`ad-event-hygiene.ts:126`); dock expand/collapse telemetry is never
   billable (`use-dock-panel.ts:201-227`).

Also absent proxy-side (PORT-MAP §8 rows 3–4): impression dedupe across
fires, BYOK/subscription/`adsEnabled`/terminal-height gating, house-floor
rendering and `bfcid` attribution, sponsored-capability route,
`mode/agentMode`, `renderDelayMs`, `cliDockArm`.

## Why each is untranslatable without a rendered transcript

The proxy renders no transcript and no ad card. Every chat-surface signal
is defined by that rendering, so proxy-side synthesis would be fabrication:

- **No mount = no impression semantics.** CLI "shown" is card mount,
  deduped per impUrl; remount/scroll churn is free (`AdCard` doc,
  `ad-banner.tsx:120-122`); `onImpression(ad)` fires on mount / ad change
  (`:134-136`); hidden cards skip impression (`use-gravity-ad.ts:285`).
  The proxy mounts nothing, so it has no receipt-to-mount
  `renderDelayMs`, no visibility state, and no honest dedupe key — which is
  why the waiting-room impression leg deliberately omits `mode` and
  `renderDelayMs` (`ads.go:186-199`) and mints a fresh uuid per leg with no
  cross-fire dedupe (PORT-MAP §8 row 3).
- **No gesture = click is fabrication.** CLI "clicked" is a CTA press:
  `onClick(ad)` then `safeOpen(ad.clickUrl)`, no-op without clickUrl
  (`ad-banner.tsx:138-143`); each gesture mints a fresh `clientEventId`
  (repeat POST = new id, `alreadyRecorded`, `use-gravity-ad.ts:435-437`).
  The proxy has no user behind the card, so any chat-surface click it sent
  would be fake engagement — the exact condition the code flags at
  `ads.go:84-90` ("the click leg has no user gesture behind it … That leg
  fabricates engagement signal the CLI only sends on a real click").
- **No conversation = no auction inputs.** The `cli_chat` auction body is
  the live transcript (user+assistant text, latest user message appended)
  plus the live `sessionId` and placement ids. The proxy's sessions are
  pooled/multiplexed operator seats, not the end-user's chat; it cannot
  supply the user's message history, activity timestamps (cadence gate),
  terminal height (`showAds = terminalHeight >= 18`), settings
  (`adsEnabled`), or BYOK/subscription context the CLI gates on
  (`chat.tsx:213,222`; `commands/ads.ts:48-54`). Synthesizing any of these
  invents user behavior.
- **No dock = no dwell.** Dock origin, dwell milliseconds, and the
  <300 ms accidental-click label exist only as measurements of a real
  panel interaction (`use-dock-panel.ts:201-227`). Omitted fields are the
  only honest values, as the waiting-room click leg already does
  (`ads.go:201-212`).

## Recommended posture

1. **Waiting-room chain only.** Keep `FireWaitingRoomChain` (auction +
   impression; click leg per its own retreat decision in PORT-MAP §10
   item 5) fired on the server 428 gate — the one leg grounded in a
   server-issued `impUrl` for a surface the gate actually names.
2. **Document, don't synthesize.** Keep this verdict next to ADS.md §gaps
   and PORT-MAP §10 item 4 so future work does not re-litigate the chat
   surface without new evidence.
3. **No transcript fabrication.** Never invent `sessionId`, message
   history, activity, dwell, or gesture ids to "complete" the chat ad loop.

## Revisit triggers

Revisit this verdict **only** on server evidence — not on completeness
instinct:

- **Free-mode gating counts chat ad-loop participation.** If a live capture
  shows `cost_mode=free` enforcement (`freebuff-cost-mode.ts:25-32`,
  currently UNVERIFIED) rejecting or throttling principals that never walk
  the `cli_chat` ad loop, the cost/benefit changes; re-study with captures
  first (PORT-MAP §10 item 4 is capture-gated for this reason).
- **Abuse treatment of gestureless legs.** If the server is observed
  penalizing gestureless clicks/impressions (currently UNVERIFIED, ADS.md
  §UNVERIFIED), retreat further (drop the click leg) rather than expand.
- **Server-sent chat placement directives.** If the wire starts ordering
  per-message ad participation (placement ids, ack tokens) on responses the
  proxy relays, mirroring becomes relay rather than synthesis — evaluate
  then.
- **Pin drift.** Re-verify D1–D9 and this verdict at each re-pin (live CLI
  is 0.0.194 vs pin 0.0.193); close or widen per capture.
