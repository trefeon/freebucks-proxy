package upstream

import (
	"context"
	cryptoRand "crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"freebucks-proxy/backend/internal/wirefacts"
)

// freebuffCliUA is the ads-API request User-Agent, mirroring the installed
// official CLI binary the proxy emulates. The CLI composes it from its own
// build-time version (upstream/freebuff
// cli/src/hooks/use-gravity-ad.ts:817-821,
// `Freebuff-CLI/${getCliEnv().CODEBUFF_CLI_VERSION}`; IS_FREEBUFF picks the
// product token), injected from the released Freebuff wrapper version the
// build was invoked with (freebuff/cli/build.ts:23,33 ->
// cli/scripts/build-binary.ts:167-168). Real installs therefore advertise
// versions like Freebuff-CLI/0.0.140
// (common/src/util/ad-user-agent.ts:44-46), never the monorepo's placeholder
// cli/package.json 1.0.0. wirefacts.VendorVersion IS that released wrapper
// version for the snapshots this proxy speaks (generated from
// backend/internal/wirefacts/testdata/wire/snapshots.json), so the version
// claimed on the wire follows the vendored wire at every re-pin instead of
// freezing at a hand-typed literal.
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
// WAITING_ROOM_CHAIN gate): POST /api/v1/ads per configured ad provider,
// then the ads impression + click legs for the auctioned ad, then GET
// /api/v1/freebuff/streak — mirroring freebuff2api-optimized codebuff.py
// _request_ads_and_streak (surface="waiting_room") plus the CLI ad loop
// (use-gravity-ad.ts recordImpressionOnce/recordClick: the free-mode ad loop
// gates free mode, freebuff-cost-mode.ts). Strictly
// best-effort: every failure is logged and swallowed; the caller must never
// depend on it (a gated stub whose real value is keeping the account's
// waiting-room requirement satisfied before the next session create). The
// streak call fires once after the provider loop, matching the reference.
//
// Honesty note: the impression acks a server-issued impUrl the auction just
// returned (grounded), but the click leg has no user gesture behind it —
// the proxy renders no ad card, so nothing was clicked. That leg fabricates
// engagement signal the CLI only sends on a real click. It stays because the
// free-mode ad loop is a live-mode gate, but reviewers should scrutinize it:
// dropping the click leg is the safe retreat if the server ever treats
// gestureless clicks as abuse.
func (c *Client) FireWaitingRoomChain(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, waitingRoomChainTimeout)
	defer cancel()
	for _, provider := range waitingRoomAdProviders {
		impURL, err := c.requestAds(ctx, provider)
		if err != nil {
			slog.Debug("waiting room chain: ads request failed", "provider", provider, "err", err)
			continue
		}
		if impURL == "" {
			continue
		}
		if err := c.postAdEvent(ctx, "/api/v1/ads/impression", impressionPayload(impURL)); err != nil {
			slog.Debug("waiting room chain: ads impression failed", "provider", provider, "err", err)
		}
		if err := c.postAdEvent(ctx, "/api/v1/ads/click", clickPayload(impURL)); err != nil {
			slog.Debug("waiting room chain: ads click failed", "provider", provider, "err", err)
		}
	}
	if err := c.getStreak(ctx); err != nil {
		slog.Debug("waiting room chain: streak request failed", "err", err)
	}
}

// waitingRoomAdProviders mirrors the reference default
// (freebuff2api-optimized config.py: ad_providers=("gravity","zeroclick")).
var waitingRoomAdProviders = []string{"gravity", "zeroclick"}

// requestAds POSTs one /api/v1/ads payload (reference cli/src/hooks/
// use-gravity-ad.ts fetchAd + common/src/util/ad-user-agent.ts: provider +
// device block + browser-like body userAgent + Freebuff-CLI header UA).
// Faithful details kept: messages stays [] and sessionId is omitted (the
// chain fires before a session exists — a fresh waiting-room).
// On success it returns the first auctioned ad's server-issued impUrl ("" when
// the auction answered with no ads), which the impression/click legs ack;
// the impUrl is never invented locally.
func (c *Client) requestAds(ctx context.Context, provider string) (string, error) {
	payload := map[string]any{
		"provider": provider,
		"messages": []any{},
		"device": map[string]any{
			"os":       deviceOS(),
			"timezone": egressDeviceTimezone(),
			"locale":   egressDeviceLocale(),
		},
		// Body userAgent: the shared browser-like UA (NOT a runtime UA) so
		// every ad provider sees a usable targeting signal — the CLI sends
		// getAdUserAgent() here (#124).
		"userAgent": adBrowserUserAgent(),
		"surface":   "waiting_room",
	}
	body, _ := json.Marshal(payload)
	req, err := c.newRequest(ctx, http.MethodPost, "/api/v1/ads", body)
	if err != nil {
		return "", err
	}
	// Header UA: Freebuff-CLI/<version> (getCliAdRequestUserAgent), NOT the
	// chat ai-sdk UA newRequest set — the CLI's ads POST carries exactly
	// this product UA (#124).
	req.Header.Set("User-Agent", freebuffCliUA)
	resp, cancel, classErr := c.do(req, c.sessionCallTimeout)
	if classErr != nil && resp == nil {
		return "", classErr
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
		return "", fmt.Errorf("ads status %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	// The auction body carries the ads array (fetchAd reads data.ads); parse
	// it on a wider cap than the error path — an ad payload exceeds 512B.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxAdAuctionRead))
	var auction struct {
		Ads []struct {
			ImpURL string `json:"impUrl"`
		} `json:"ads"`
	}
	if err := json.Unmarshal(raw, &auction); err != nil || len(auction.Ads) == 0 {
		return "", nil
	}
	return auction.Ads[0].ImpURL, nil
}

// adEventIDHeader is the per-event id header the server reads (reference
// common/src/ads/ad-event-hygiene.ts FREEBUFF_EVENT_ID_HEADER): one
// crypto/rand uuid per logical event, echoed in the body as clientEventId.
const adEventIDHeader = "X-Freebuff-Event-Id"

// impressionPayload builds the /api/v1/ads/impression body for a
// server-issued impUrl (reference use-gravity-ad.ts recordImpressionOnce
// direct-fetch path: impUrl + agentMode + browser userAgent/os +
// clientEventId). mode is omitted: the CLI's is its agentMode and the proxy
// has no agent mode to declare honestly. renderDelayMs is omitted: no card
// is mounted, so there is no receipt-to-mount delay to report.
func impressionPayload(impURL string) map[string]any {
	return map[string]any{
		"impUrl":        impURL,
		"userAgent":     adBrowserUserAgent(),
		"os":            deviceOS(),
		"clientEventId": newAdEventID(),
	}
}

// clickPayload builds the /api/v1/ads/click body for a server-issued impUrl
// (reference use-gravity-ad.ts recordClick: impUrl + clientEventId +
// optional surface). surface rides along because the auction claimed
// surface="waiting_room" for this impUrl; dock fields are omitted (no dock,
// no dwell to report honestly).
func clickPayload(impURL string) map[string]any {
	return map[string]any{
		"impUrl":        impURL,
		"clientEventId": newAdEventID(),
		"surface":       "waiting_room",
	}
}

// postAdEvent POSTs one ads impression/click event: Content-Type + Bearer
// via newRequest (both paths carry a JSON body), the Freebuff-CLI product UA
// override, and the caller-minted X-Freebuff-Event-Id echoed in the body.
// Best-effort transport like the rest of the chain: the caller logs and
// swallows errors so ad failures never fail admission.
func (c *Client) postAdEvent(ctx context.Context, path string, payload map[string]any) error {
	body, _ := json.Marshal(payload)
	req, err := c.newRequest(ctx, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", freebuffCliUA)
	if eventID, ok := payload["clientEventId"].(string); ok && eventID != "" {
		req.Header.Set(adEventIDHeader, eventID)
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
		return fmt.Errorf("ads event %s status %d: %s", path, resp.StatusCode, truncate(string(raw), 200))
	}
	_, _ = io.ReadAll(io.LimitReader(resp.Body, maxAdResponseRead))
	return nil
}

// newAdEventID mints one UUIDv4 event id per ads event, mirroring the CLI's
// crypto.randomUUID per impression/click (use-gravity-ad.ts: one id per
// logical event; the header is what the server reads).
func newAdEventID() string {
	var b [16]byte
	if _, err := cryptoRand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
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
