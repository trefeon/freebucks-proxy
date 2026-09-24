package upstream

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Chat-surface ad loop: the proxy walks the CLI's chat ad legs the way the
// real CLI does - auction + impression, server-side only, nothing rendered
// to any client - so upstream sees the cli_chat surface on accounts that
// serve real chat traffic (operator-ordered proofing posture).
//
// Mirrored from the CLI's useGravityAd chat hook (upstream/freebuff
// cli/src/hooks/use-gravity-ad.ts, chat.tsx:200-222):
//   - surface cli_chat, provider gravity with zeroclick fallback;
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

// chatAdProviders mirrors the reference provider order
// (freebuff2api-optimized config.py ad_providers=("gravity","zeroclick")):
// gravity first, zeroclick as fallback when gravity misses or fails.
var chatAdProviders = []string{"gravity", "zeroclick"}

// ChatAdLeg is the ledger payload for one chat-surface ad leg. It carries
// the fixed ledger contract fields only: impUrl/clickUrl values NEVER reach
// the ledger or the dashboard (titles/brands only).
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

// RecordChatAdLeg emits one chat ad leg to the ads ledger. It is nil until
// the ledger owner (pool, backend/internal/pool/ads_ledger.go) wires it to
// pool.RecordAdLeg at startup; unwired legs are dropped. The hook exists
// because pool already imports upstream, so upstream cannot import pool
// back (import cycle): the ledger owns the store, this package only emits.
var RecordChatAdLeg func(ChatAdLeg)

// emitChatAdLeg drops the leg when no ledger is wired yet.
func emitChatAdLeg(leg ChatAdLeg) {
	if RecordChatAdLeg == nil {
		return
	}
	leg.Surface = chatAdSurface
	if leg.TS.IsZero() {
		leg.TS = time.Now()
	}
	RecordChatAdLeg(leg)
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
// response body are untouched (zero client-visible change).
func (c *Client) noteChatServed(ctx context.Context) {
	if !chatAds.servedAndClaim() {
		return
	}
	roundCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), waitingRoomChainTimeout)
	go func() {
		defer cancel()
		c.fireChatAdRound(roundCtx)
	}()
}

// fireChatAdRound walks one auction (+impression) round on the chat
// surface: per provider in order, auction, then the impression leg for the
// auctioned ad when its impUrl is fresh. Gravity first; zeroclick fires
// only as fallback (gravity miss or error). Best-effort throughout:
// every leg is logged at debug and swallowed, and a ledger event is
// emitted per fired leg. No click leg exists anywhere on this path.
func (c *Client) fireChatAdRound(ctx context.Context) {
	for _, provider := range chatAdProviders {
		ad, err := c.requestAdsForSurface(ctx, provider, chatAdSurface)
		if err != nil {
			slog.Debug("chat ad loop: ads request failed", "provider", provider, "err", err)
			emitChatAdLeg(ChatAdLeg{Provider: provider, Leg: "auction", Error: err.Error()})
			continue
		}
		emitChatAdLeg(ChatAdLeg{Provider: provider, Leg: "auction", Title: ad.Title, Brand: ad.Brand})
		if ad.ImpURL == "" {
			continue
		}
		if chatAds.markImpSeen(ad.ImpURL) {
			// Already acked on an earlier round: the proofing goal for
			// this creative is met, so the round ends here instead of
			// spending a fallback auction chasing a second creative.
			slog.Debug("chat ad loop: impUrl already acked, skipping impression", "provider", provider)
			return
		}
		credits, err := c.postAdEvent(ctx, "/api/v1/ads/impression", impressionPayload(ad.ImpURL))
		if err != nil {
			slog.Debug("chat ad loop: ads impression failed", "provider", provider, "err", err)
			emitChatAdLeg(ChatAdLeg{Provider: provider, Leg: "impression", Title: ad.Title, Brand: ad.Brand, Error: err.Error()})
			continue
		}
		leg := ChatAdLeg{Provider: provider, Leg: "impression", Title: ad.Title, Brand: ad.Brand}
		if credits > 0 {
			leg.Credits = &credits
		}
		emitChatAdLeg(leg)
		return
	}
}
