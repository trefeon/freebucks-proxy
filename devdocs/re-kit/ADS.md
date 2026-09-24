# ADS — surfaces, auction/impression/click, gating, house promo, proxy mirror

> Version: live CLI 0.0.194 vs static pin 0.0.193. All tokens/hosts redacted.

## Surfaces + triggers

One hook drives both surfaces: `useGravityAd` (upstream/freebuff/cli/src/hooks/use-gravity-ad.ts).

1. **Waiting-room landing grid — always-on** (upstream/freebuff/cli/src/components/freebuff-landing-screen.tsx:472-485): `useGravityAd({enabled:true, forceStart:true, provider:'gravity', surface:'waiting_room', placementIds: visibleWaitingRoomPlacementIds(width)})`. `forceStart` bypasses the first-message gate (`shouldStart = forceStart||hasUserMessaged`, use-gravity-ad.ts:257-262). Card count = `floor(width/60)`, min 1 (upstream/freebuff/common/src/ads/waiting-room-placements.ts:10-20). Rendered as `ChoiceAdBanner` row (upstream/freebuff/cli/src/components/ad-banner.tsx).
2. **Chat transcript — gated dock + lazy inline pool** (upstream/freebuff/cli/src/chat.tsx:200-222,1927-1948): `useGravityAd({enabled: !byok && (IS_FREEBUFF||!hasSubscription), provider:'gravity', inline:true, surface:'cli_chat', inlinePlacementId:'CLI-Chat-Inline', slotPlacementId:'Single-Ad-Unit-1'})`, rendered as `SingleAdBanner ad=ads[0]` when `ads?.[0] && showInlineAds` (chat.tsx:1927-1948). `SingleAdBanner` re-renders/fires per rotation of `ads[0]` (ad-banner.tsx:675-676). Inline pool: `requestResponseAds(messageId,count)` fills up to 4 (`MAX_RESPONSE_AD_POOL_SIZE`) per eligible answer (`isInlineAdEligibleAnswer`: ai-variant, id prefix, `metadata.allowInlineAds`), reuses pool without new auctions/impressions past 4 (use-gravity-ad.ts:683-748).
3. **Rotation/cadence** (use-gravity-ad.ts:603-676): immediate fetch + 60s interval (`AD_ROTATION_INTERVAL_MS`, :656-676); fetch allowed only if `adsShownSinceActivity<3 && isUserActive(30s)` (`MAX_ADS_AFTER_ACTIVITY`, `ACTIVITY_THRESHOLD_MS`, :603-619), counter reset on activity (:642-647). On fetch miss falls back to round-robin choice cache (cap 50, dedup by first impUrl, ZeroClick excluded, :125-143,621-634).
4. **Renderers** (upstream/freebuff/cli/src/components/ad-banner.tsx): heights `AD_CARD_HEIGHT=5`, `INLINE_AD_CARD_HEIGHT=4`; disclosures `Ad` / `Sponsored·{Brand}` / `Sponsored` + ` ↗` + `[ Close ]`. Dock arm policy + expand/collapse handling in (upstream/freebuff/cli/src/hooks/use-dock-panel.ts); CTA sibling never toggles panel (ad-banner.tsx:460-462).

## Endpoints + fields

- **Auction `POST {API_ORIGIN}/api/v1/ads`** (use-gravity-ad.ts:528-562; capability route `/api/ads` when sponsored-capability probe hits). Headers: `Content-Type: application/json`, `Authorization: Bearer <redacted>`, `User-Agent: {Freebuff-CLI|Codebuff-CLI}/<redacted-version>` (`getCliAdRequestUserAgent`, :817-821; IS_FREEBUFF picks product). Body: `{provider, messages: AdMessage[] (user+assistant text only, INSTRUCTIONS_PROMPT excluded, latest UI user msg appended as <user_message>, :768-789,492-520), sessionId (chatSessionId), device:{os: darwin->macos/win32->windows/linux default, timezone: Intl, locale: Intl} (:799-815), [sponsoredCapability, capabilityInspection], [surface], [placementId], [placementIds], userAgent: getAdUserAgent() (Chrome 151 Mozilla/ per-platform, linux fallback, upstream/freebuff/common/src/util/ad-user-agent.ts:22-36), [cliDockArm]}`. Response: `{ads: AdResponse[], provider?}`; stamped `receivedAtMs=Date.now()` (:581-594). `AdResponse={adText,title,cta,url,favicon,clickUrl,impUrl,[placementId],[provider],[impressionIds],[credits],[receivedAtMs],[expandedBody,bullets,diagram]}` (:44-72).
- **Impression `POST {API_ORIGIN}/api/v1/ads/impression`** (`recordImpressionOnce`, :283-426): same headers + `X-Freebuff-Event-Id: <redacted-uuid>` per logical event (:347). Body: `{impUrl, mode: agentMode, userAgent: getAdUserAgent(), os, clientEventId, [renderDelayMs=renderDelaySinceReceipt(ad)]}` (:356-368). Response `{creditsGranted}` -> sets `ad.credits` + logs when >0 (:379-392). Event-id/render-delay header rules in (upstream/freebuff/common/src/ads/ad-event-hygiene.ts).
- **Click `POST {API_ORIGIN}/api/v1/ads/click`** (`recordClick`, :428-473): same headers + fresh `clientEventId` per gesture (repeat POST = new id, server answers `alreadyRecorded`, :435-437). Body: `{impUrl, clientEventId, [surface], [dockFrom, dockDwellMs, dockAccidentalClick]}` (:446-460). Dock metadata rides the click so canonical `ads.clicked` carries it; no second client event (:450-452).
- **First-party ack (provider=first_party)** (:305-343): resilient `acknowledgeFirstPartyView({token:impUrl, url, init, surface:surface??'cli_chat', placementId: ad.placementId??slotPlacementId??'unknown', clientFamily:'cli', [renderDelayMs]})`; transport shape 2s timeout x3, 10s ceiling (upstream/freebuff/common/src/ads/first-party-view-ack.ts:47-79).
- **ZeroClick pixel side-path** (:395-421): `POST https://zeroclick.dev/api/v2/impressions {ids: impressionIds}` then local impression.

Sample (redacted): `POST {API_ORIGIN}/api/v1/ads` H: `Authorization: Bearer <redacted>`, `User-Agent: Freebuff-CLI/<redacted>`, `X-Freebuff-Event-Id: <redacted>` B: `{"provider":"<redacted>","surface":"waiting_room","device":{"os":"<redacted>","timezone":"<IANA>","locale":"<redacted>"},"userAgent":"Mozilla/5.0 (<redacted>) Chrome/151"}` R: `{"ads":[{"impUrl":"{API_ORIGIN}/<redacted>","clickUrl":"{API_ORIGIN}/<redacted>"}]}`.

## Shown / clicked / dismissed + dedupe

- Shown = card mount (deduped per impUrl; remount/scroll churn free, `AdCard` doc ad-banner.tsx:120-122); `AdCard` fires `onImpression(ad)` on mount / ad change (`useEffect [ad]`, :134-136). Hidden cards skip impression (:285 in use-gravity-ad.ts); hiding (`shouldHideAds`) suppresses fetch AND impression.
- Impression dedupe per impUrl via `claimAdImpression` set (use-gravity-ad.ts:160-167,289).
- Clicked = CTA press: `onClick(ad)` then `safeOpen(ad.clickUrl)`, no-op if no clickUrl (ad-banner.tsx:138-143). `DockAdCard` same but impression only in `dock` layout mode (:354-357). Dock/panel origin + dwell + <300ms accidental label via `DOCK_ACCIDENTAL_CLICK_MS` (upstream/freebuff/common/src/ads/ad-event-hygiene.ts:126).
- Dismissed = no ad-dismiss primitive — close affordances are dock panel collapse (`⌃O details`, Esc/close/click-away; `ADS_DOCK_COLLAPSED` with method+dwell, never billable, use-dock-panel.ts:201-227) and `/ads:disable` / `/ads:proposal*` channel controls (upstream/freebuff/cli/src/commands/ads.ts).

## Analytics names

- Client: `cli.inline_ad_slot_eligible {response_id, chat_session_id, eligible_slot_count, pool_size, provider, surface, placement_id, is_freebuff}` (use-gravity-ad.ts:704-717) + `cli.inline_ad_pool_reused`; `ads.first_party_view_ack`; `ads.dock_expanded {imp_url,...}` (per-impUrl per-session dedupe, NOT impression/billable) / `ads.dock_collapsed {imp_url,provider,placement_id,method,dwell_ms}` (use-dock-panel.ts:1-9,201-227).
- Server: `ads.fetch_completed/impression_recorded/clicked/first_party_*` — names only (upstream/freebuff/common/src/constants/analytics-events.ts:194-209), payloads UNVERIFIED (see below).

## Gating / tiers

- Chat hook `enabled = !hasSelectedByokConnection && (IS_FREEBUFF || !hasSubscription)`; `showInlineAds` uses `getAdsEnabled()` instead of subscription (chat.tsx:213,222). `getAdsEnabled() = IS_FREEBUFF ? true : settings.adsEnabled ?? false` (commands/ads.ts:48-54); default `settings.json {mode:DEFAULT, adsEnabled:true}` (upstream/freebuff/cli/src/utils/settings.ts:26-28). `/ads:enable|disable` flips it (Codebuff only; hidden when `hasSubscription` per chat.tsx:568-576); Freebuff always true so no opt-out. BYOK selected connection kills both surfaces.
- Height: `showAds = terminalHeight >= 18`; hook additionally hides when `terminalHeight<=17` unless IS_FREEBUFF (use-gravity-ad.ts:245-254). Compact height kills non-Freebuff only.
- Placement catalog: waiting-room-1..4, CLI-Chat-Inline, Single-Ad-Unit-1 (CLI-Dock), sellable vs legacy batch ids (upstream/freebuff/common/src/constants/freebuff-placements.ts). Interop surfaces/heights/disclosures + settings keys (docs/UPSTREAM-CLI.md:470-476,753-764).

## House promo + bfcid

- Creatives (upstream/freebuff/common/src/constants/freebuff-house-ad.ts): destination `https://freebuff.com/plans` (:60), favicon `.../favicon/favicon-32x32.ico` (:129), display image `.../opengraph-image.png` (:348); `HouseAdCreative={title,adText,cta,url,favicon,[imageUrl]}`; budgets title 12 / text 28 chars (:156-157); 4 variations/surface (`cli_chat, waiting_room, freebuff_web_chat, chat_assistant, chat_assistant_sr`); break copy keyed by placement (`Desktop-Spotlight/Showcase`); display card 3 variants.
- Floor vs campaign: floor = variation 0 (`HOUSE_AD_CREATIVES`, `HOUSE_AD_VARIATIONS/VARIATIONS[0]`); campaign = seeded `CAMPAIGN` checked in alongside floor.
- Catalog copy reads live: `Starter $8/mo, +3/day +30/mo` (`ENTRY_TIER=TIERS[0]`, :72-101 in upstream/freebuff/common/src/constants/freebuff-subscriptions.ts; tiers `starter/plus/pro 8/25/60`, free baseline 4/14/40).
- Suppression: entitled subscribers served nothing on house paths (`resolveHouseSuppressedForUser`, since 2026-09-02, freebuff-house-ad.ts:23-29) — the ONLY subscription read; **subscription does not remove ads** (:79-83).
- Attribution: `bfcid` cookie + `x-freebuff-bfcid` header, HMAC-verified, stamped onto Stripe subscription (upstream/freebuff/common/src/constants/freebuff-models.ts:3437-3451). Prod hosts: house destination + favicon + creative image base (upstream/freebuff/common/src/constants/hosts.ts).

## Proxy mirror + gaps

Mirror lives in (backend/internal/upstream/ads.go); auth/UA scoping in (backend/internal/upstream/client_chat.go:64-96); stealth profiles NOT applied to API calls (backend/internal/stealth/headers.go); gate flag + chain trigger (backend/internal/upstream/client.go:102-106,421-425; backend/internal/upstream/classify.go:184-186).

- Mirrored: pre-session `FireWaitingRoomChain` (15s cap, ads.go:70,91-113) on the 428 `waiting_room_required` gate only. `POST /api/v1/ads` per provider in `("gravity","zeroclick")` (:117) with `{provider, messages:[], device:{os,timeZone,locale}, userAgent:<Chrome151 body UA>, surface:"waiting_room"}` (:128-141), header `User-Agent: Freebuff-CLI/<wirefacts.VendorVersion>` (:34,150) — never placeholder 1.0.0; then `POST /api/v1/ads/impression {impUrl,userAgent,os,clientEventId}` (mode + renderDelayMs deliberately omitted, :192-199) and `POST /api/v1/ads/click {impUrl,clientEventId,surface:waiting_room}` (dock fields omitted, :206-212), each with `X-Freebuff-Event-Id == body clientEventId` (uuidv4, :184,226-228,248-256), Bearer via newRequest + `Content-Type`, proxy headers stripped, NO browser headers (TLS ClientHello impersonation stays, client_chat.go:64-96); then `GET /api/v1/freebuff/streak` with plain Bun UA (no override, :315-338). Best-effort: every leg logged/swallowed, impression 500 still fires click + streak (pinned backend/internal/upstream/signal_guard_test.go:548-581; payload assertions + continuation backend/internal/upstream/signal_guard_test.go:480-581, backend/internal/upstream/client_chat_test.go:985-1010); impUrl never invented (:124-126,175-178); deviceOS darwin->macos + linux fallback (:262-275), tz->UTC fallback (:283-292), locale POSIX->en-US (:294-313), UA/os agreement rule (:44-56). Device-block derivation contract (backend/internal/upstream/egress_device_block_test.go).
- Gaps: (1) click leg has no user gesture — proxy renders no card, CLI only clicks on CTA; code flags it as fabricated engagement, safe retreat = drop click leg (ads.go:84-90); (2) no chat-surface mirror — `cli_chat` auction, `CLI-Chat-Inline` pool, `Single-Ad-Unit-1` rotation, 60s/3-per-30s cadence, choice cache, ZeroClick pixel, first-party ack transport, `mode/agentMode`, `renderDelayMs`, `sessionId`, message history, `placementId(s)`, `cliDockArm`, sponsored-capability route, dock dwell/accidental fields all absent; (3) no impression dedupe across fires (fresh uuid per leg; CLI dedupes per impUrl); (4) no BYOK/subscription/adsEnabled/height gating — chain fires on server 428 only; (5) no house-floor rendering or `bfcid` attribution; (6) streak leg UA intentionally differs (Bun vs Freebuff-CLI).

## Spoof-relevant notes (compatible-client sends only)

- A compatible client sends header `User-Agent: Freebuff-CLI/<redacted>` on ad legs and body `userAgent` = Chrome-151 Mozilla/ string per platform (ad-user-agent.ts:22-36) with matching `device.os` (darwin->macos mapping). Mismatched UA/os is detectable surface-side (ads.go:44-56 agreement rule).
- `X-Freebuff-Event-Id` equals body `clientEventId` (uuidv4) on proxy legs (ads.go:226-228,248-256); CLI emits one uuid per logical event (:347) and a fresh id per click gesture (:435-437).
- No automation of fake engagement is documented here: proxy click leg is flagged fabricated (ads.go:84-90); dock expand/collapse telemetry is never billable (use-dock-panel.ts:201-227).

## UNVERIFIED

- Server fallback order Gravity->ZeroClick/Carbon (comment at landing-screen.tsx:473-475 only).
- `creditsGranted` semantics; `alreadyRecorded` click response; auction response fields beyond `ads[].impUrl`.
- 428 trigger conditions beyond classification (classify.go:184-186); any abuse treatment of gestureless clicks.
- Server analytics payloads `ads.fetch_completed/impression_recorded/clicked/first_party_*` (names only, analytics-events.ts:194-209).
- Free-mode server gate `cost_mode=free` ad-loop coverage among ~20 gates (freebuff-cost-mode.ts:25-32) — enforcement detail unknown.
