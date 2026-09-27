# ADS trace — every ad the free-tier CLI shows, and the proxy mirror

Vendor tip `57943aa71`. CLI rule that governs everything: render-gated legs
fire only on actual render; omission falls inside defined server fallbacks
(`ad-request.ts:99-104`). Proxy posture: honest-values-only — never fabricate
render, dwell, clicks, capability, or widths.

## 1. Waiting room (always-on monetization)

CLI: `useGravityAd({enabled, forceStart, provider:'gravity',
surface:'waiting_room', placementIds: width-derived})`
(`freebuff-landing-screen.tsx:481-489`); forceStart bypasses the
first-message gate; render gate `terminalHeight >= 18` (`:452`); cards =
`min(available, max(1, floor((width-2)/60)))`, slots `waiting-room-1..4`
(`waiting-room-placements.ts:11-20`, `freebuff-placements.ts:242-265`).
 ONE gravity auction, `messages: []`, `sessionId` always, fetch on mount +
60s rotation. User sees 1–4 cards under the picker; click opens the URL.
Proxy (`FireWaitingRoomChain`, `ads.go:92-111`): single gravity auction
(2026-09-27 fix — was two), `messages: []` exact match, stable sessionId
minted per chain, impression via legacy/first-party transport. Residual:
no placementIds (no honest terminal width), no clicks (no press exists).

## 2. Chat inline + rotation

CLI (`chat.tsx:252-265`, `use-gravity-ad.ts`): enabled `!BYOK &&
(IS_FREEBUFF || !hasSubscription)`; 60s rotation, ≤3 fresh fetches per 30s
activity window then cache (max 50, zeroclick excluded, dedupe by impUrl);
hidden (`height<=17`) → no fetch, no impression; sponsor dock pauses the
slot. Inline pool ≤4 per answer, cycled without re-auction. Auction carries
the transcript (user+assistant, instructions excluded, latest user msg
appended). Impression posts back the server-minted `impUrl` +
`renderDelayMs` — that POST-back IS the render proof.
Proxy (`ads_chat.go`): burst cap 3, 30s idle reset, per-impUrl dedupe,
detached background rounds; auctions now carry the served turn's transcript
(`noteChatServedWithBody`, `chat.go:242`) and stable sessionId; impression
via legacy or first-party transport per the served provider. Residual: no
60s timer (collapsed onto served turns — conservative, fewer auctions), no
rotation cache / inline pool (equivalent to empty cache → server refills),
no `mode`/`renderDelayMs` (nothing renders; zero would fabricate).

## 3. Dock

CLI (`use-dock-panel.ts`): policy fetched once per process
(`GET /api/v1/ads/policy` → `{dockArm, partnerPlacementIds}`, fail-open);
sticky arm rides later auctions as `cliDockArm`; expansion is client
telemetry only; clicks carry `dockDwellMs` + `dockAccidentalClick`
(`dwell<300ms` is a label, not a filter).
Proxy: fetches the policy once per token, fail-open, sends the resolved arm;
no render/dwell/click-context (no dock exists). Undetectable per-event —
omitted arm is the vendor-specified fallback.

## 4. Partner rows

CLI (`partner-ads.ts`): composer + slash-review slots, `surface:
'cli_chat'`, NO provider, PR-keyword trigger, 30min fill/no-fill hold,
first-party-only fills (paid-network fills rejected client-side),
impression once per impUrl.
Proxy: no partner slots. Aggregate-only absence — per-session it's identical
to "user never typed a PR word". Partner inventory is first-party CPM, so
no paid-network signal is lost.

## 5. First-party ack / zeroclick / offer

- First-party: resilient transport (shared event id, 3×2s, dedupe-tolerant
  208) — proxy uses it exactly when the server answers `first_party`
  inventory, legacy POST otherwise. Never claimed unless served.
- Zeroclick pixel: needs `impressionIds` the parse drops — unfireable by
  design; local impression still acked. Advertiser-side signal only, no ban
  vector on the proxy account.
- Agentic offer + sponsored turns: unmodellable without project fs+git
  (capability null → vendor's own builder returns null), unmintable
  conversation ids, no card UI. Silence = the common no-capability client.
- Axiom telemetry: best-effort, failures swallowed — not sent.
- `clientEventId` always on legacy impressions (only `renderDelayMs` is
  optional) — proxy mints when absent; headerless events cannot go out.

## 6. Gating facts (verified, commonly misbelieved)

- Subscription removes NO ads on freebuff builds (`ads.ts:47-53`,
  `house-ad.ts:32,78-86`); only BYOK-equivalent (no session) suppresses.
- No quiet hours / do-not-disturb anywhere in `cli/src`+`common/src` —
  only the 30s activity gate.
- Locale/country: device `{timezone,locale}` is targeting signal only;
  geo is server-side from egress IP. Proxy reports the gateway host honestly.
- `X-Freebuff-Event-Id` per logical event + body echo; UA family derived
  server-side from the leading UA token — keep the triple consistent
  (Freebuff-CLI header / browser-like body / matching device OS).
