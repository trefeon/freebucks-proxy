package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"freebuff-proxy/backend/internal/wirefacts"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// freebuffCliUA is the ads-API request User-Agent, mirroring the installed
// official CLI binary the proxy emulates. The CLI composes it from its own
// build-time version (upstream/freebuff
// cli/src/utils/ad-client-identity.ts:41-45,
// `Freebuff-CLI/${getCliEnv().CODEBUFF_CLI_VERSION}`; IS_FREEBUFF picks the
// product token), injected at binary build time from the release version the
// build was invoked with (upstream/freebuff cli/scripts/build-binary.ts:167-168,
// process.env.CODEBUFF_CLI_VERSION). Released installs therefore advertise
// the released Freebuff wrapper version, never the monorepo's placeholder
// cli/package.json 1.0.0. wirefacts.VendorVersion IS that released wrapper
// version for the snapshots this proxy speaks (npm `freebuff` wrapper version
// stamped at re-pin into scripts/vendor-version.txt and snapshots.json
// vendor_version, regenerated into wirefacts_gen.go), so the version claimed
// on the wire follows the vendored wire at every re-pin instead of freezing
// at a hand-typed literal.
// R3 verdict (2026-09-27): cli/package.json at tip is still 1.0.0 while
// wirefacts.VendorVersion is 0.2.1 — DIVERGED, so the derivation below is
// deliberately unchanged (pinning the UA to cli/package.json would advertise
// a version no released binary ever sends).
var freebuffCliUA = "Freebuff-CLI/" + wirefacts.VendorVersion

const (
	// maxAdResponseRead caps the ad response body read.
	maxAdResponseRead = 512
	// maxAdAuctionRead caps the successful auction body read: the ads array
	// (creatives + impUrls) exceeds the 512B error-path cap.
	maxAdAuctionRead = 64 << 10
)

// adUserAgents maps runtime.GOOS to the browser-like Chrome-151 UA sent to
// ad providers for targeting/fraud screening (#124). The CLI ships one entry
// per platform (reference common/src/util/ad-user-agent.ts: darwin/win32/
// linux AD_USER_AGENTS built from AD_CHROME_VERSION=151.0.0.0;
// use-gravity-ad.ts sends it as the body userAgent) and warns that native
// runtime UAs look bot-like to ad networks. The body UA must agree with the
// device block's os (deviceOS): a mixed signal (e.g. os:"linux" with a
// Windows UA) reads as spoofing to ad networks.
var adUserAgents = map[string]string{
	"darwin":  "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36",
	"windows": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36",
	"linux":   "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36",
}

// adBrowserUserAgent returns the platform-consistent ads body UA for the
// host, falling back to the Linux entry exactly like the CLI's
// getAdUserAgent (AD_USER_AGENTS[platformKey] ?? linux).
func adBrowserUserAgent() string {
	if ua, ok := adUserAgents[runtime.GOOS]; ok {
		return ua
	}
	return adUserAgents["linux"]
}

// waitingRoomChainTimeout bounds the whole best-effort pre-session chain so
// a hung upstream never blocks a session create for long.
const waitingRoomChainTimeout = 15 * time.Second

// FireWaitingRoomChain runs the reference pre-session flow (issue #94(b),
// WAITING_ROOM_CHAIN gate): ONE auction, then the ads impression leg for the
// auctioned ad, then GET /api/v1/freebuff/streak — mirroring
// freebuff2api-optimized codebuff.py _request_ads_and_streak
// (surface="waiting_room") plus the CLI ad loop (use-gravity-ad.ts
// recordImpressionOnce: the free-mode ad loop gates free mode,
// freebuff-cost-mode.ts). Strictly
// best-effort: every failure is logged and swallowed; the caller must never
// depend on it (a gated stub whose real value is keeping the account's
// waiting-room requirement satisfied before the next session create). The
// streak call fires once after the auction, matching the reference.
//
// Single auction, never a provider chain: the CLI fires exactly one
// buildAdAuctionRequest per tick (upstream/freebuff
// cli/src/hooks/use-gravity-ad.ts:535-543) with provider gravity
// (upstream/freebuff cli/src/components/freebuff-landing-screen.tsx:481-488),
// and the server itself tries Gravity first, then falls back to ZeroClick
// and Carbon (landing-screen.tsx:479). A proxy-side gravity→zeroclick
// back-to-back would be a second auction the genuine client never sends —
// traffic-shape-visible. A zeroclick creative answered on the single gravity
// auction is still acked (it was served for this live request); the
// zeroclick rule (use-gravity-ad.ts:151-154) only keeps such offers out of
// the display rotation cache, which the proxy has none of (nothing is ever
// rendered, so no rotation exists).
//
// Honesty note: the impression acks a server-issued impUrl the auction just
// returned (grounded). The click leg (recordClick) is deliberately not
// mirrored: the proxy renders no ad card, so no user gesture exists behind
// a click, and the CLI only sends it on a real click — a gestureless click
// would fabricate engagement signal the server may treat as abuse.
func (c *Client) FireWaitingRoomChain(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, waitingRoomChainTimeout)
	defer cancel()
	// Session boundary: the pool fires this chain immediately before the
	// session create (backend/internal/pool/acquire_route.go) on a consumed
	// 428, so the upcoming upstream session gets a fresh auction id
	// (upstream/freebuff cli/src/state/chat-store.ts:552-554 regenerates
	// chatSessionId on reset the same way).
	c.rotateAdsSessionID()
	ad, err := c.requestAdsForSurface(ctx, waitingRoomAdProvider, "waiting_room", nil)
	if err != nil {
		slog.Debug("waiting room chain: ads request failed", "provider", waitingRoomAdProvider, "err", err)
	} else if ad.ImpURL != "" {
		if _, err := c.ackAuctionedAd(ctx, ad); err != nil {
			slog.Debug("waiting room chain: ads impression failed", "provider", servedAdProvider(ad, waitingRoomAdProvider), "err", err)
		}
	}
	if err := c.getStreak(ctx); err != nil {
		slog.Debug("waiting room chain: streak request failed", "err", err)
	}
}

// waitingRoomAdProvider is the single auction provider on every rail. The
// CLI auctions provider gravity once per tick (upstream/freebuff
// cli/src/components/freebuff-landing-screen.tsx:481-488 waiting_room,
// cli/src/chat.tsx:252-265 cli_chat) and the server owns fallback ordering
// (landing-screen.tsx:479); the proxy never fires a second auction itself.
const waitingRoomAdProvider = "gravity"

// auctionedAd is the first creative of an auction response: the
// server-issued impUrl the impression leg acks (never invented locally),
// plus best-effort display metadata for the ads ledger. Title/brand keys
// beyond impUrl are UNVERIFIED on the wire: they ride the ledger only when
// the server actually sends them, and impUrl/clickUrl values never reach
// the ledger (titles/brands only).
// Provider is the serving provider the SERVER answered
// (use-gravity-ad.ts:575 `provider: data.provider ?? provider`): the
// impression transport is chosen from it, never from the requested
// provider, so first_party is used only when the server actually served it.
type auctionedAd struct {
	ImpURL   string
	Title    string
	Brand    string
	Provider string
}

// servedAdProvider reports the provider the server served for ledger and
// log lines: the auctioned provider when the server named one, else the
// requested provider the auction went out under.
func servedAdProvider(ad auctionedAd, requested string) string {
	if ad.Provider != "" {
		return ad.Provider
	}
	return requested
}

// adAuctionMessage is one transcript turn as the auction sees it: user or
// assistant text only (upstream/freebuff cli/src/ads/ad-request.ts:30-57
// convertToAdMessages). Shaped by adAuctionMessagesFromChatBody (ads_chat.go)
// on the chat rail; the waiting-room rail passes nil (no history exists).
type adAuctionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// requestAdsForSurface POSTs one /api/v1/ads auction payload
// (reference cli/src/hooks/use-gravity-ad.ts fetchAd +
// common/src/util/ad-user-agent.ts: provider + device block + browser-like
// body userAgent + Freebuff-CLI header UA). Exactly one auction per call:
// the CLI never fires a provider chain itself (use-gravity-ad.ts:535-543).
// On success it returns the first auctioned ad with the serving provider
// the server named; an auction answered with no ads returns the zero ad,
// which the caller must not ack and must not chase with a second auction
// (the server owns fallback ordering — landing-screen.tsx:479).
// R6 auction-signal ledger vs the CLI's buildAdAuctionRequest
// (upstream/freebuff cli/src/ads/ad-request.ts:141-178): every field the
// server reads to target, price and bot-filter is sent only when honestly
// known, and omitted otherwise — the vendor contract is explicit that each
// missing field simply omits its key because "the server's fallback for each
// is better than no ad at all" (ad-request.ts:99-104):
//   - provider, messages, sessionId, device, userAgent, surface: sent.
//     messages carries the served turn's user+assistant text on cli_chat
//     (adMessagesForRequest at ad-request.ts:68-91) and [] on waiting_room
//     (exact match — the landing screen has no conversation yet, and the
//     CLI's own history reads [] there too).
//   - sessionId: always sent (ad-request.ts:153). The value is the proxy's
//     own per-upstream-session uuid (adsSessionID): minted locally at the
//     session boundary and never copied across sessions, so it names a real
//     proxy session the way chatSessionId names a real CLI chat
//     (chat-store.ts:59-60,204-207).
//   - cliDockArm: sent once the dock policy resolves (ad-request.ts:139,175
//     `...(dockArm ? { cliDockArm: dockArm } : {})`), omitted while
//     unresolved — null is deliberately distinct from 'control'
//     (use-dock-panel.ts:48-56) so the server falls back to its own
//     assignment rather than being handed a guess. A resolved 'control'
//     IS sent (it is truthy), exactly like the CLI.
//   - traceContext: omitted. Pointer ids to the last finished agent run's
//     trace (chat-store.ts:341-349); the proxy runs no agent runs.
//   - sponsoredCapability/capabilityInspection: omitted. They require
//     project-root filesystem + git inspection
//     (sponsored-cli-capability.ts:141) which a remote gateway never sees;
//     without a capability the CLI itself stays on the default WEBSITE_URL
//     /api/v1/ads route (ad-request.ts:134-142), exactly where this POST lands.
//   - placementId/placementIds: omitted. Render-slot ids for slots the proxy
//     never fills (waiting-room widths at freebuff-landing-screen.tsx:480-488,
//     CLI-Chat-Inline at chat.tsx:259, partner slots via partner-ads.ts).
func (c *Client) requestAdsForSurface(ctx context.Context, provider, surface string, msgs []adAuctionMessage) (auctionedAd, error) {
	// Snapshot the cached arm BEFORE kicking the fetch: the fetch runs
	// detached, so on a loaded machine it can land before the payload below
	// is built and the first auction would carry an arm the CLI never sends
	// (its first request precedes the policy fetch — ad-request.ts:139 vs
	// use-dock-panel.ts:48-51). Later calls observe the cached arm normally.
	arm, armOk := c.adsDockArm()
	c.ensureAdsPolicy(ctx)
	var messages any = []any{}
	if len(msgs) > 0 {
		messages = msgs
	}
	payload := map[string]any{
		"provider":  provider,
		"messages":  messages,
		"sessionId": c.adsSessionID(),
		"device": map[string]any{
			"os":       deviceOS(),
			"timezone": consistencyAdsZoneOr(egressDeviceTimezone()),
			"locale":   consistencyAdsLocaleOr(egressDeviceLocale()),
		},
		// Body userAgent: the shared browser-like UA (NOT a runtime UA) so
		// every ad provider sees a usable targeting signal — the CLI sends
		// getAdUserAgent() here (#124).
		"userAgent": adBrowserUserAgent(),
		"surface":   surface,
	}
	if armOk {
		payload["cliDockArm"] = arm
	}
	body, _ := json.Marshal(payload)
	req, err := c.newRequest(ctx, http.MethodPost, "/api/v1/ads", body)
	if err != nil {
		return auctionedAd{}, err
	}
	// Header UA: Freebuff-CLI/<version> (getCliAdRequestUserAgent), NOT the
	// chat ai-sdk UA newRequest set — the CLI's ads POST carries exactly
	// this product UA (#124).
	req.Header.Set("User-Agent", freebuffCliUA)
	// The environment descriptor rides every ad request (vendor
	// clientEnvironmentHeaders spread into ad-request.ts / partner-ads.ts /
	// use-gravity-ad.ts fetches).
	stampClientEnv(req.Header)
	resp, cancel, classErr := c.do(req, c.sessionCallTimeout)
	if classErr != nil && resp == nil {
		return auctionedAd{}, classErr
	}
	// do() returns a nil cancel when the context already carried a deadline
	// (the chain's own timeout), so guard the defer.
	if cancel != nil {
		defer cancel()
	}
	defer func() { _ = resp.Body.Close() }()
	if classErr != nil {
		// do() classified a >=400 response once; the ads path keeps its own
		// descriptive error from the (already-read) body.
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxAdResponseRead))
		return auctionedAd{}, fmt.Errorf("ads status %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	// The auction body carries the ads array (fetchAd reads data.ads) plus
	// the serving provider (use-gravity-ad.ts:575
	// `provider: data.provider ?? provider`); parse it on a wider cap than
	// the error path — an ad payload exceeds 512B.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxAdAuctionRead))
	var auction struct {
		Provider string `json:"provider"`
		Ads      []struct {
			ImpURL string `json:"impUrl"`
			Title  string `json:"title"`
			Brand  string `json:"brand"`
		} `json:"ads"`
	}
	if err := json.Unmarshal(raw, &auction); err != nil || len(auction.Ads) == 0 {
		return auctionedAd{}, nil
	}
	return auctionedAd{
		ImpURL:   auction.Ads[0].ImpURL,
		Title:    auction.Ads[0].Title,
		Brand:    auction.Ads[0].Brand,
		Provider: auction.Provider,
	}, nil
}

// adsSessionIDs maps a client token key to the proxy's own auction session
// id: the per-upstream-session uuid sent as sessionId on every auction
// (upstream/freebuff cli/src/ads/ad-request.ts:153 always sends the chat
// store's chatSessionId, waiting-room included). The CLI mints
// chatSessionId with crypto.randomUUID at chat start and regenerates it on
// /new (chat-store.ts:204-207,552-554) — process memory, one id per chat,
// never reused across chats. The proxy mirrors that lifecycle: one uuid per
// upstream session, rotated at the session boundary (FireWaitingRoomChain,
// which the pool fires immediately before the session create), lazily
// minted when the chain never fired. Keyed by TokenKey (the sha256 hex, so
// no secret material lands in the map); tokens are pool-bounded, so the map
// is too. Residual gap: sessions created without a preceding chain (no 428)
// keep the previous id — rotating there needs the session-create signal
// from session.go/pool, outside this file; see the yield report.
var (
	adsSessionIDsMu sync.Mutex
	adsSessionIDs   = map[string]string{}
)

// adsSessionID returns the stable auction session id for this client,
// lazily minting it on first use.
func (c *Client) adsSessionID() string {
	adsSessionIDsMu.Lock()
	defer adsSessionIDsMu.Unlock()
	key := c.TokenKey()
	if id, ok := adsSessionIDs[key]; ok && id != "" {
		return id
	}
	id := mintAdsUUID()
	adsSessionIDs[key] = id
	return id
}

// rotateAdsSessionID mints a fresh auction session id for the upcoming
// upstream session. Called at the session boundary (FireWaitingRoomChain)
// so an id is never reused across sessions.
func (c *Client) rotateAdsSessionID() string {
	adsSessionIDsMu.Lock()
	defer adsSessionIDsMu.Unlock()
	id := mintAdsUUID()
	adsSessionIDs[c.TokenKey()] = id
	return id
}

// resetAdsSessionIDsForTest clears auction session ids between tests.
func resetAdsSessionIDsForTest() {
	adsSessionIDsMu.Lock()
	defer adsSessionIDsMu.Unlock()
	adsSessionIDs = map[string]string{}
}

// adsPolicyPath is the dock-policy route (upstream/freebuff
// cli/src/hooks/use-dock-panel.ts:134): GET {WEBSITE_URL}/api/v1/ads/policy.
const adsPolicyPath = "/api/v1/ads/policy"

// adsDockPolicyState is one token's cached dock policy: the sticky arm plus
// whether it resolved yet. Null (unresolved) is deliberately distinct from
// 'control' (use-dock-panel.ts:48-56): auctions omit cliDockArm until the
// first fetch lands, then report whatever was cached — including 'control'.
type adsDockPolicyState struct {
	resolved bool
	fetching bool
	arm      string
}

var (
	adsPolicyMu sync.Mutex
	adsPolicies = map[string]*adsDockPolicyState{}
)

// adsDockArm reports the cached dock arm and whether the policy resolved
// yet. Unresolved (false) means omit cliDockArm on the auction.
func (c *Client) adsDockArm() (string, bool) {
	adsPolicyMu.Lock()
	defer adsPolicyMu.Unlock()
	st, ok := adsPolicies[c.TokenKey()]
	if !ok || !st.resolved {
		return "", false
	}
	return st.arm, true
}

// ensureAdsPolicy kicks the once-per-token policy fetch the first time an
// auction needs it (upstream/freebuff cli/src/hooks/use-dock-panel.ts:86-100
// resolveAdPolicy: one in-flight fetch shared by all askers, sticky after).
// The vendor resolves once per CLI process (single account); the proxy is a
// multi-account gateway, so the faithful analogue is once per token.
// Non-blocking: the fetch runs detached in the background, so the first
// auctions omit cliDockArm exactly like a CLI whose first ad request
// precedes the policy fetch (use-dock-panel.ts:48-51).
func (c *Client) ensureAdsPolicy(ctx context.Context) {
	adsPolicyMu.Lock()
	st, ok := adsPolicies[c.TokenKey()]
	if !ok {
		st = &adsDockPolicyState{}
		adsPolicies[c.TokenKey()] = st
	}
	if st.resolved || st.fetching {
		adsPolicyMu.Unlock()
		return
	}
	st.fetching = true
	adsPolicyMu.Unlock()
	go func() {
		timeout := c.sessionCallTimeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		defer cancel()
		c.fetchAdsPolicy(fetchCtx)
	}()
}

// fetchAdsPolicy performs one synchronous policy fetch and caches the
// sticky arm (upstream/freebuff cli/src/hooks/use-dock-panel.ts:127-154
// fetchDockArm). Fail-open by design: no token path aside, EVERY failure —
// network, non-200, unparseable body, unrecognised arm — lands on 'control'
// (an experiment that fails open into its own treatment arm is not an
// experiment), and the partner list the proxy has no rows for is not
// requested. The fetch is plain Bearer auth with newRequest's default Bun
// UA: the vendor uses bare fetch with no UA override. Marks resolved on
// every outcome, so later auctions report the cached arm.
func (c *Client) fetchAdsPolicy(ctx context.Context) string {
	arm := "control"
	if req, err := c.newRequest(ctx, http.MethodGet, adsPolicyPath, nil); err == nil {
		resp, cancel, classErr := c.do(req, c.sessionCallTimeout)
		if cancel != nil {
			defer cancel()
		}
		if resp != nil {
			defer func() { _ = resp.Body.Close() }()
			if classErr == nil {
				raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxAdResponseRead))
				var parsed struct {
					DockArm string `json:"dockArm"`
				}
				if err := json.Unmarshal(raw, &parsed); err == nil && (parsed.DockArm == "expandable" || parsed.DockArm == "control") {
					arm = parsed.DockArm
				}
			}
		}
	}
	adsPolicyMu.Lock()
	st, ok := adsPolicies[c.TokenKey()]
	if !ok {
		st = &adsDockPolicyState{}
		adsPolicies[c.TokenKey()] = st
	}
	st.arm = arm
	st.resolved = true
	st.fetching = false
	adsPolicyMu.Unlock()
	return arm
}

// resetAdsPolicyForTest clears cached dock policies between tests. Tests
// that need deterministic arms must use fresh tokens (a background fetch
// from an earlier test resolves fast against its closed server, but only a
// fresh token rules out ordering effects entirely).
func resetAdsPolicyForTest() {
	adsPolicyMu.Lock()
	defer adsPolicyMu.Unlock()
	adsPolicies = map[string]*adsDockPolicyState{}
}

// adEventIDHeader is the per-event id header the server reads (reference
// common/src/ads/ad-event-hygiene.ts FREEBUFF_EVENT_ID_HEADER): one
// crypto/rand uuid per logical event, echoed in the body as clientEventId.
const adEventIDHeader = "X-Freebuff-Event-Id"

// impressionPayload builds the /api/v1/ads/impression body for a
// server-issued impUrl on the LEGACY third-party path (reference
// use-gravity-ad.ts recordImpressionOnce direct-fetch path: impUrl +
// agentMode + browser userAgent/os + clientEventId, :440-461). mode is
// omitted: the CLI's is its agentMode and the proxy has no agent mode to
// declare honestly. renderDelayMs is omitted: no card is mounted, so there
// is no receipt-to-mount delay to report.
// R7: mode/renderDelayMs stay omitted because nothing is rendered here — the
// CLI measures renderDelayMs at card-show time against the auction receipt
// stamp (use-gravity-ad.ts:105-112,391-395) and carries it as a body field
// on this path (:448-460) and as the X-Freebuff-Render-Delay-Ms header on
// the first-party path (ad-event-hygiene.ts:27,
// first-party-view-ack.ts:163-171); with no mount there is no delay to
// report, and a zero would fabricate an instant render. mode is the CLI's
// agentMode (:391,411,450); the gateway has no agent mode to declare.
// clientEventId rides every legacy impression (use-gravity-ad.ts:458 — the
// only optional field on this path is renderDelayMs); postAdEvent enforces
// that invariant even for callers that forgot to mint one. Accepted gaps on
// this leg: the zeroclick third-party pixel (POST
// https://zeroclick.dev/api/v2/impressions {ids}; use-gravity-ad.ts:40,487-513)
// needs impressionIds the auction parse deliberately drops (that pixel fires
// only when a zeroclick card actually renders — use-gravity-ad.ts:487 — and
// nothing renders here), and the click leg stays retired (no gesture exists
// behind a click).
func impressionPayload(impURL string) map[string]any {
	return map[string]any{
		"impUrl":        impURL,
		"userAgent":     adBrowserUserAgent(),
		"os":            deviceOS(),
		"clientEventId": newAdEventID(),
	}
}

// ackAuctionedAd acks one auctioned creative on the transport matching the
// provider the SERVER served (never the requested one): first-party
// creatives take the resilient acknowledgement transport, everything else
// the legacy single POST — the same gate the CLI evaluates per creative
// (use-gravity-ad.ts:200-209 dispatchFirstPartyViewAcknowledgement opens
// only for provider 'first_party'; :433-461 is the fallthrough). A missing
// provider answer means a legacy rail served it (older servers predate the
// field), so "" falls through to the legacy path.
func (c *Client) ackAuctionedAd(ctx context.Context, ad auctionedAd) (float64, error) {
	if ad.Provider == "first_party" {
		return c.postFirstPartyImpression(ctx, ad.ImpURL)
	}
	return c.postAdEvent(ctx, "/api/v1/ads/impression", impressionPayload(ad.ImpURL))
}

// firstPartyAckAttempts is the attempt budget of the resilient ack
// (upstream/freebuff common/src/ads/first-party-view-ack.ts:180: three
// two-second attempts).
const firstPartyAckAttempts = 3

var (
	// firstPartyAttemptTimeout bounds one ack attempt
	// (first-party-view-ack.ts:86 FIRST_PARTY_VIEW_ACK_TIMEOUT_MS=2000).
	// Var (not const) as the deterministic focused-test seam, mirroring
	// the vendor's injectable attemptTimeoutMs (:78).
	firstPartyAttemptTimeout = 2 * time.Second
	// firstPartyRetryDelays spaces attempts (first-party-view-ack.ts:95
	// RETRY_DELAYS_MS=[250,1000]). Var for the same test seam (the
	// vendor's injectable sleep, :74).
	firstPartyRetryDelays = []time.Duration{250 * time.Millisecond, 1000 * time.Millisecond}
)

// postFirstPartyImpression acks a first-party creative with the resilient
// transport (upstream/freebuff
// common/src/ads/first-party-view-ack.ts:148-236
// acknowledgeFirstPartyView, dispatched per creative at
// use-gravity-ad.ts:397-435): up to three 2s attempts sharing ONE
// clientEventId (a retry is the same logical event — :157-160), the event
// id on the X-Freebuff-Event-Id header on every attempt (:161-162), and
// dedupe-tolerant outcomes (208/deduped/alreadyRecorded stop the sequence
// as success — :108-116). Retried only on server errors, timeouts and
// network errors (:221-224); client errors are terminal.
// Deliberate omissions, each grounded: NO render-delay header — it is sent
// only when the caller measured receipt-to-mount (:163-171), and with no
// render the server must store NULL, never a derived value
// (ad-event-hygiene.ts:66-72); NO body clientEventId — the first-party
// request carries the event id as a header only (:160-162, and the vendor
// ack body at use-gravity-ad.ts:409-414 has no such field); NO mode — the
// vendor body carries the CLI's agentMode (:411) which the gateway has
// none of to declare honestly (same R7 retreat as the legacy path).
// The creditsGranted grant is read best-effort for the ads ledger when the
// server actually sends one (the legacy path's grounded grant,
// use-gravity-ad.ts:471-476); 0 otherwise — never invented.
func (c *Client) postFirstPartyImpression(ctx context.Context, impURL string) (float64, error) {
	eventID := mintAdsUUID()
	payload := map[string]any{
		"impUrl":    impURL,
		"userAgent": adBrowserUserAgent(),
		"os":        deviceOS(),
	}
	body, _ := json.Marshal(payload)
	var lastErr error
	for attempt := 1; attempt <= firstPartyAckAttempts; attempt++ {
		grant, retryable, err := c.postFirstPartyAttempt(ctx, body, eventID)
		if err == nil {
			return grant, nil
		}
		lastErr = err
		if !retryable || attempt == firstPartyAckAttempts {
			return grant, err
		}
		delay := time.Duration(0)
		if attempt-1 < len(firstPartyRetryDelays) {
			delay = firstPartyRetryDelays[attempt-1]
		}
		if delay <= 0 {
			continue
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return 0, ctx.Err()
		}
	}
	return 0, lastErr
}

// postFirstPartyAttempt fires one first-party ack attempt. It returns the
// grant with a nil error on accepted OR deduped outcomes (both mean the
// view is recorded — retrying a deduped ack would be a second event), and
// whether a failure may be retried. The transport bypasses do() on
// purpose: do()'s transient-retry loop (1s*2^attempt backoff, fingerprint
// rotation) would swallow the vendor's exact 250ms/1000ms cadence and
// double-count transport metrics — the vendor ack is plain fetch retries
// with no TLS games.
func (c *Client) postFirstPartyAttempt(ctx context.Context, body []byte, eventID string) (float64, bool, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, firstPartyAttemptTimeout)
	defer cancel()
	req, err := c.newRequest(attemptCtx, http.MethodPost, "/api/v1/ads/impression", body)
	if err != nil {
		return 0, false, err
	}
	req.Header.Set("User-Agent", freebuffCliUA)
	req.Header.Set(adEventIDHeader, eventID)
	stampClientEnv(req.Header)
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, false, ctx.Err()
		}
		return 0, true, fmt.Errorf("ads first-party impression attempt: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := wrapDecompress(resp); err != nil {
		return 0, false, fmt.Errorf("ads first-party impression decompress: %w", err)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxAdResponseRead))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Accepted or deduped — both mean the view is recorded (the
		// vendor evaluates isDedupedResponse only inside its response.ok
		// branch: 208, the deduped outcome header and the
		// deduped/alreadyRecorded body all land here as success —
		// first-party-view-ack.ts:108-116,191-193). A retry now would mint
		// a second event, so the sequence stops.
		return firstPartyGrant(raw), false, nil
	}
	if resp.StatusCode >= 500 {
		return 0, true, fmt.Errorf("ads first-party impression status %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	return 0, false, fmt.Errorf("ads first-party impression status %d: %s", resp.StatusCode, truncate(string(raw), 200))
}

// firstPartyGrant reads the ledger grant best-effort when the server sent
// one; 0 otherwise.
func firstPartyGrant(raw []byte) float64 {
	var grant struct {
		CreditsGranted float64 `json:"creditsGranted"`
	}
	_ = json.Unmarshal(raw, &grant)
	return grant.CreditsGranted
}

// postAdEvent POSTs one ads impression event on the LEGACY third-party path:
// Content-Type + Bearer via newRequest (the path carries a JSON body), the
// Freebuff-CLI product UA override, and the X-Freebuff-Event-Id echoed from
// the body's clientEventId. clientEventId rides EVERY legacy impression
// (use-gravity-ad.ts:458 — only renderDelayMs is optional there), so the
// transport mints one when the caller forgot it instead of sending a
// headerless event the server cannot join to its auction.
// Best-effort transport like the rest of the chain: the caller logs and
// swallows errors so ad failures never fail admission.
// On success it returns the server's creditsGranted grant, if any
// (reference use-gravity-ad.ts recordImpressionOnce: the impression
// response carries {creditsGranted}, applied when >0); 0 when the server
// sent none.
func (c *Client) postAdEvent(ctx context.Context, path string, payload map[string]any) (float64, error) {
	body, _ := json.Marshal(payload)
	req, err := c.newRequest(ctx, http.MethodPost, path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", freebuffCliUA)
	eventID, _ := payload["clientEventId"].(string)
	if eventID == "" {
		eventID = mintAdsUUID()
		payload["clientEventId"] = eventID
		body, _ = json.Marshal(payload)
		req, err = c.newRequest(ctx, http.MethodPost, path, body)
		if err != nil {
			return 0, err
		}
		req.Header.Set("User-Agent", freebuffCliUA)
	}
	req.Header.Set(adEventIDHeader, eventID)
	stampClientEnv(req.Header)
	resp, cancel, classErr := c.do(req, c.sessionCallTimeout)
	if classErr != nil && resp == nil {
		return 0, classErr
	}
	if cancel != nil {
		defer cancel()
	}
	defer func() { _ = resp.Body.Close() }()
	if classErr != nil {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxAdResponseRead))
		return 0, fmt.Errorf("ads event %s status %d: %s", path, resp.StatusCode, truncate(string(raw), 200))
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxAdResponseRead))
	var grant struct {
		CreditsGranted float64 `json:"creditsGranted"`
	}
	_ = json.Unmarshal(raw, &grant)
	return grant.CreditsGranted, nil
}

// mintAdsUUID mints one random UUID string for ads traffic (auction
// session ids, event ids), mirroring the CLI's crypto.randomUUID
// (chat-store.ts:204 for chatSessionId, use-gravity-ad.ts:439 per
// impression). The timestamp fallback preserves the historical behaviour
// for this best-effort identifier.
func mintAdsUUID() string {
	id, err := newUUIDv4()
	if err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return id
}

// newAdEventID mints one UUIDv4 event id per ads event, mirroring the CLI's
// crypto.randomUUID per impression (use-gravity-ad.ts: one id per logical
// event; the header is what the server reads). Preserve the historical
// timestamp fallback for this best-effort ads identifier.
func newAdEventID() string {
	return mintAdsUUID()
}

// deviceOS maps runtime.GOOS to the ads device block's wire contract
// (macos|windows|linux, use-gravity-ad.ts getDeviceInfo platformToOs):
// Go reports "darwin" but the API only accepts "macos", and anything
// unrecognized falls back to "linux" exactly like the CLI.
func deviceOS() string {
	return deviceOSFor(runtime.GOOS)
}

func deviceOSFor(goos string) string {
	switch goos {
	case "darwin":
		return "macos"
	case "windows":
		return "windows"
	default:
		return "linux"
	}
}

// egressDeviceTimezone returns the host IANA timezone name for the ads
// device block, mirroring the CLI's
// Intl.DateTimeFormat().resolvedOptions().timeZone (use-gravity-ad.ts
// getDeviceInfo). time.Local.String() is the host zone when Go resolved a
// real IANA name; "Local" is Go's placeholder when it could not, so that
// (and anything LoadLocation rejects) falls back to the always-valid "UTC".
func egressDeviceTimezone() string {
	tz := time.Local.String()
	if tz == "" || tz == "Local" {
		return "UTC"
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return "UTC"
	}
	return tz
}

// egressDeviceLocale returns the host locale for the ads device block,
// derived from LC_ALL/LC_MESSAGES/LANG (POSIX "en_US.UTF-8" → "en-US",
// charset stripped, "_" → "-"), falling back to "en-US" — the CLI's
// Intl.DateTimeFormat().resolvedOptions().locale shape (use-gravity-ad.ts
// getDeviceInfo). "C"/"POSIX" are not real locales and are skipped.
func egressDeviceLocale() string {
	for _, env := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		raw := os.Getenv(env)
		if raw == "" {
			continue
		}
		lang := strings.SplitN(raw, ".", 2)[0]
		lang = strings.ReplaceAll(lang, "_", "-")
		if lang == "" || lang == "C" || lang == "POSIX" {
			continue
		}
		return lang
	}
	return "en-US"
}

// getStreak GETs /api/v1/freebuff/streak (reference
// cli/src/hooks/use-freebuff-streak-query.ts: the request() helper sets NO
// UA override → bun's default `Bun/<version>`). The proxy's equivalent of
// "no override" is newRequest's bunUserAgent (Bun/1.3.14, the pinned
// .bun-version), which is what this request inherits.
func (c *Client) getStreak(ctx context.Context) error {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/v1/freebuff/streak", nil)
	if err != nil {
		return err
	}
	resp, cancel, classErr := c.do(req, c.sessionCallTimeout)
	if classErr != nil && resp == nil {
		return classErr
	}
	if cancel != nil {
		defer cancel()
	}
	defer func() { _ = resp.Body.Close() }()
	if classErr != nil {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxAdResponseRead))
		return fmt.Errorf("streak status %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	return nil
}
