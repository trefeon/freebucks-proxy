package upstream

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Chat-surface ad loop: the proxy walks the CLI's chat ad legs the way the
// real CLI does - auction + impression, server-side only, nothing rendered
// to any client - so upstream sees the cli_chat surface on accounts that
// serve real chat traffic (operator-ordered proofing posture).
//
// Mirrored from the CLI's useGravityAd chat hook (upstream/freebuff
// cli/src/hooks/use-gravity-ad.ts, chat.tsx:200-222):
//   - surface cli_chat, ONE gravity auction per round (the server owns
//     fallback ordering — landing-screen.tsx:479; the CLI's fetchAd fires a
//     single buildAdAuctionRequest per tick, use-gravity-ad.ts:535-543). A
//     zeroclick creative answered on that auction is still acked for the
//     live request; the zeroclick rule (use-gravity-ad.ts:151-154) only
//     keeps such offers out of the display rotation cache, which the proxy
//     has none of (nothing renders, so no rotation exists).
//   - activity gate: a round fires only on a freshly served chat turn, at
//     most 3 rounds per activity burst (MAX_ADS_AFTER_ACTIVITY=3); a 30s
//     idle gap (ACTIVITY_THRESHOLD_MS) resets the burst. The CLI's 60s
//     rotation tick (AD_ROTATION_INTERVAL_MS) is deliberately collapsed:
//     the proxy renders no card to rotate, so firing rides the served turn
//     itself instead of a timer. Bounds are preserved (at most 3 rounds per
//     active 30s window, silence when idle); per-turn latency is not.
//   - per-impUrl impression dedupe mirroring claimAdImpression
//     (use-gravity-ad.ts:160-167): a server-issued impUrl is acked at most
//     once; repeat auctions of the same creative fire no second impression.
//
// Honesty notes (binding):
//   - Impressions fire without render: no card is mounted anywhere, so
//     there is no receipt-to-mount delay to report (renderDelayMs omitted),
//     no agent mode to declare (mode omitted), and no user gesture behind
//     any signal.
//   - The click leg (recordClick) is deliberately absent: the CLI sends it
//     only on a real CTA press, and a gestureless click would fabricate
//     engagement signal the server may treat as abuse (same retreat as the
//     waiting-room chain, ads.go).
//   - Every leg is best-effort: failures are logged at debug and swallowed;
//     ad legs never fail admission or chat, and the chat response path is
//     untouched (a round runs on a detached context in the background).
//
// R2 agentic-offer rail (requestAgenticOffer, upstream/freebuff
// cli/src/utils/sponsored-proposal-api.ts:748-771, wire contract
// common/src/ads/agentic-offer.ts:50) is deliberately NOT implemented on this
// path: the route is a per-turn eligibility question, but its body is not
// modellable without render/UI + project state. buildAgenticOfferRequest
// (sponsored-offer.ts:134-169) returns null without a sponsored capability
// (:145-149), and the capability requires project-root filesystem + git
// inspection (sponsored-cli-capability.ts:141: package manager, lockfiles,
// remote, committed head, containment floor) which a remote gateway never
// sees; conversationId is the CLI/Desktop thread id the proxy never mints
// (agentic-offer.ts:99-100); and a served offer would need a card UI plus
// local execution (accept/state-report) the gateway has no surface for —
// firing it headless would mint server-side proposal rows nobody can read or
// accept. Documented gap, not a silent miss: the 10s timeout (:141,762) and
// the Freebuff-CLI UA gate (:744-746) are transport the proxy already speaks
// on the display rails.
const (
	// chatAdSurface is the auction surface for the chat loop.
	chatAdSurface = "cli_chat"
	// chatAdMaxRoundsPerBurst caps auction rounds per activity burst,
	// mirroring MAX_ADS_AFTER_ACTIVITY=3.
	chatAdMaxRoundsPerBurst = 3
	// chatAdIdleReset is the idle gap that starts a new activity burst,
	// mirroring ACTIVITY_THRESHOLD_MS=30s.
	chatAdIdleReset = 30 * time.Second
	// chatAdSeenCap bounds the per-impUrl dedupe set; overflow clears it
	// (impUrls are server-issued and unique per creative, so a re-ack
	// after a clear is rare and still grounded in a fresh auction).
	chatAdSeenCap = 2048
)

// chatAdProvider is the single auction provider on the chat rail — gravity,
// matching the CLI's chat hook (upstream/freebuff cli/src/chat.tsx:252-265).
// The server falls back itself (landing-screen.tsx:479); the proxy never
// fires a second auction.
const chatAdProvider = "gravity"

// ChatAdLeg is the ledger payload for one chat-surface ad leg. It carries
// the fixed ledger contract fields only: impUrl/clickUrl values NEVER reach
// the ledger or the dashboard (titles/brands only). Provider is the SERVING
// provider the server answered (use-gravity-ad.ts:575), falling back to the
// requested one when the server predates the field — never a claim, always
// what the auction actually served.
type ChatAdLeg struct {
	TS       time.Time
	Surface  string
	Provider string
	Leg      string // auction | impression
	Title    string
	Brand    string
	Credits  *float64
	Error    string
}

// recordChatAdLegFn is the atomic pointer holding the registered chat ad leg callback.
var recordChatAdLegFn atomic.Pointer[func(ChatAdLeg)]

// SetRecordChatAdLeg safely configures or clears the chat ad leg sink.
func SetRecordChatAdLeg(fn func(ChatAdLeg)) {
	if fn == nil {
		recordChatAdLegFn.Store(nil)
		return
	}
	recordChatAdLegFn.Store(&fn)
}

// emitChatAdLeg drops the leg when no ledger is wired yet.
func emitChatAdLeg(leg ChatAdLeg) {
	fnPtr := recordChatAdLegFn.Load()
	if fnPtr == nil || *fnPtr == nil {
		return
	}
	leg.Surface = chatAdSurface
	if leg.TS.IsZero() {
		leg.TS = time.Now()
	}
	(*fnPtr)(leg)
}

// chatAdTracker is the activity gate + burst cap + impression dedupe set.
// Package-global: cadence is a process-wide posture like the CLI's
// single-process hook, and impUrls are globally unique server-issued ids.
type chatAdTracker struct {
	mu sync.Mutex
	// now is a test seam (nil = time.Now).
	now func() time.Time
	// lastActivity stamps the most recent served chat turn.
	lastActivity time.Time
	// shownSinceActivity counts rounds fired since the burst started.
	shownSinceActivity int
	// seen dedupes impressions per impUrl (claimAdImpression set).
	seen map[string]struct{}
}

var chatAds = &chatAdTracker{seen: make(map[string]struct{})}

func (t *chatAdTracker) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

// servedAndClaim records a freshly served chat turn and reports whether a
// chat ad round may fire: yes when fewer than chatAdMaxRoundsPerBurst
// rounds fired since the burst started. A gap longer than chatAdIdleReset
// starts a new burst (counter reset on activity, use-gravity-ad.ts:642-647).
func (t *chatAdTracker) servedAndClaim() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.clock()
	if now.Sub(t.lastActivity) > chatAdIdleReset {
		t.shownSinceActivity = 0
	}
	t.lastActivity = now
	if t.shownSinceActivity >= chatAdMaxRoundsPerBurst {
		return false
	}
	t.shownSinceActivity++
	return true
}

// markImpSeen reports whether impURL was already acked (claimAdImpression:
// true = skip the impression), marking fresh urls seen. The mark lands
// before the POST (at-most-once ack): a failed ack is not retried, so a
// flaky network never replays billable signal.
func (t *chatAdTracker) markImpSeen(impURL string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.seen[impURL]; ok {
		return true
	}
	if len(t.seen) >= chatAdSeenCap {
		t.seen = make(map[string]struct{})
	}
	t.seen[impURL] = struct{}{}
	return false
}

// resetChatAdTrackerForTest clears gate + dedupe state between tests.
func resetChatAdTrackerForTest() {
	chatAds.mu.Lock()
	defer chatAds.mu.Unlock()
	chatAds.now = nil
	chatAds.lastActivity = time.Time{}
	chatAds.shownSinceActivity = 0
	chatAds.seen = make(map[string]struct{})
}

// noteChatServed records a successfully served upstream chat turn (2xx
// headers received) and kicks one best-effort chat ad round when the burst
// cap allows. Called from ChatCompletions' success path; the round runs on
// a detached timeout context in the background so chat latency and the
// response body are untouched (zero client-visible change). The plain form
// carries no transcript (the chat.go call site threads only ctx today — see
// noteChatServedWithBody); prefer that form wherever the request body is
// in scope.
func (c *Client) noteChatServed(ctx context.Context) {
	c.noteChatServedWithBody(ctx, nil)
}

// noteChatServedWithBody is noteChatServed plus the served turn's OpenAI
// request body, shaped into the auction's transcript messages
// (ad-request.ts:68-91). Wiring note: ChatCompletions (chat.go) holds the
// enveloped body at its 2xx branch and today calls noteChatServed(ctx);
// passing the body here is a one-line change at that call site
// (c.noteChatServedWithBody(ctx, enveloped)) — outside this file's scope,
// so production rounds send messages [] until that line lands, and the
// shaping below is pinned hermetically meanwhile. [] is never fabricated
// transcript: it means "no transcript available at this layer", exactly
// what the CLI sends when its own history is empty.
func (c *Client) noteChatServedWithBody(ctx context.Context, body []byte) {
	var msgs []adAuctionMessage
	if len(body) > 0 {
		msgs = adAuctionMessagesFromChatBody(body)
	}
	if !chatAds.servedAndClaim() {
		return
	}
	roundCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), waitingRoomChainTimeout)
	go func() {
		defer cancel()
		c.fireChatAdRound(roundCtx, msgs)
	}()
}

// maxAdAuctionMessages caps the transcript tail sent on a cli_chat auction.
// The CLI sends its full converted history (ad-request.ts:68-91, no cap);
// the proxy bounds the auction body to the most recent turns instead — a
// deliberate, documented bound so one long session cannot grow an
// ad-proofing POST without limit. Targeting degrades gracefully: the most
// recent turns carry the freshest signal.
const maxAdAuctionMessages = 20

// adAuctionMessagesFromChatBody shapes an OpenAI chat request body into the
// auction's transcript messages, mirroring convertToAdMessages
// (upstream/freebuff cli/src/ads/ad-request.ts:36-57): user and assistant
// turns only, text parts only, trimmed, empties dropped. System turns are
// dropped — the OpenAI wire carries no message.tags field for the
// INSTRUCTIONS_PROMPT exclusion (:41-44) to key on, and the system role is
// the proxy wire's instruction carrier (the injected root prompt), so
// dropping the role is the honest equivalent; message text is never
// content-filtered (filtering user text by substring would mangle legit
// content the vendor keeps). Unparseable or message-less bodies yield nil,
// which the auction sends as [].
func adAuctionMessagesFromChatBody(body []byte) []adAuctionMessage {
	var payload struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || len(payload.Messages) == 0 {
		return nil
	}
	var out []adAuctionMessage
	for _, m := range payload.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		text := adAuctionText(m.Content)
		if text == "" {
			continue
		}
		out = append(out, adAuctionMessage{Role: m.Role, Content: text})
	}
	if len(out) > maxAdAuctionMessages {
		out = out[len(out)-maxAdAuctionMessages:]
	}
	return out
}

// adAuctionText extracts the text of one OpenAI message content: a plain
// string, or an array of parts keeping type=="text" text (ad-request.ts:47-52
// `.filter(c => c.type === 'text').map(c => c.text.trim())`, joined on
// blank lines). Anything else (images, tool calls, refusals) is not
// auction text and is skipped.
func adAuctionText(content any) string {
	switch v := content.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		var parts []string
		for _, p := range v {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := pm["type"].(string); t != "text" {
				continue
			}
			if s, _ := pm["text"].(string); strings.TrimSpace(s) != "" {
				parts = append(parts, strings.TrimSpace(s))
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n\n"))
	default:
		return ""
	}
}

// fireChatAdRound fires one auction (+impression) round on the chat
// surface: a SINGLE gravity auction (the server owns fallback ordering —
// landing-screen.tsx:479), then the impression leg for the auctioned ad
// when its impUrl is fresh. msgs is the shaped transcript tail (nil sends
// messages []). Best-effort throughout: every leg is logged at debug and
// swallowed, and a ledger event is emitted per fired leg. No click leg
// exists anywhere on this path.
func (c *Client) fireChatAdRound(ctx context.Context, msgs []adAuctionMessage) {
	ad, err := c.requestAdsForSurface(ctx, chatAdProvider, chatAdSurface, msgs)
	if err != nil {
		slog.Debug("chat ad loop: ads request failed", "provider", chatAdProvider, "err", err)
		emitChatAdLeg(ChatAdLeg{Provider: chatAdProvider, Leg: "auction", Error: err.Error()})
		return
	}
	served := servedAdProvider(ad, chatAdProvider)
	emitChatAdLeg(ChatAdLeg{Provider: served, Leg: "auction", Title: ad.Title, Brand: ad.Brand})
	if ad.ImpURL == "" {
		// Zero fills: the round ends. The CLI's tick falls back to its
		// display CACHE here (use-gravity-ad.ts:610-619), which the proxy
		// has none of — a second auction would be traffic the genuine
		// client never sends, so the round ends instead.
		return
	}
	if chatAds.markImpSeen(ad.ImpURL) {
		// Already acked on an earlier round (claimAdImpression,
		// use-gravity-ad.ts:186-193): the proofing goal for this creative
		// is met, so the round ends.
		slog.Debug("chat ad loop: impUrl already acked, skipping impression", "provider", served)
		return
	}
	credits, err := c.ackAuctionedAd(ctx, ad)
	if err != nil {
		slog.Debug("chat ad loop: ads impression failed", "provider", served, "err", err)
		emitChatAdLeg(ChatAdLeg{Provider: served, Leg: "impression", Title: ad.Title, Brand: ad.Brand, Error: err.Error()})
		return
	}
	leg := ChatAdLeg{Provider: served, Leg: "impression", Title: ad.Title, Brand: ad.Brand}
	if credits > 0 {
		leg.Credits = &credits
	}
	emitChatAdLeg(leg)
}
