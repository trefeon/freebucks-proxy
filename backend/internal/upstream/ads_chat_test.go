package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// chatAdFakeUpstream is a scriptable stand-in for the ads + chat routes the
// chat ad loop touches. It reuses recordedReq/writeBodyJSON from the
// signal-guard tests and additionally counts click-leg hits, which must
// stay zero on every path (click leg retired: no gestureless clicks).
type chatAdFakeUpstream struct {
	srv         *httptest.Server
	mu          sync.Mutex
	auctions    []recordedReq
	impressions []recordedReq
	clicks      int
	chats       int
	// auctionBody answers one auction body per provider name.
	auctionBody func(provider string) string
	// auctionStatus answers one auction status per provider (nil = 200).
	auctionStatus func(provider string) int
	// impressionBody answers the impression route ("" = {"ok":true}).
	impressionBody string
	// impressionStatus answers the impression route (0 = 200).
	impressionStatus int
}

func newChatAdFakeUpstream() *chatAdFakeUpstream {
	u := &chatAdFakeUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(u.handle))
	return u
}

func (u *chatAdFakeUpstream) URL() string { return u.srv.URL }
func (u *chatAdFakeUpstream) Close()      { u.srv.Close() }

func (u *chatAdFakeUpstream) handle(w http.ResponseWriter, r *http.Request) {
	rawBody, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	rec := recordedReq{method: r.Method, path: r.URL.Path, body: string(rawBody), header: r.Header.Clone()}
	u.mu.Lock()
	switch {
	case r.URL.Path == "/api/v1/ads" && r.Method == http.MethodPost:
		u.auctions = append(u.auctions, rec)
		u.mu.Unlock()
		var payload struct {
			Provider string `json:"provider"`
		}
		_ = json.Unmarshal(rawBody, &payload)
		body := `{"ads":[]}`
		status := http.StatusOK
		if u.auctionBody != nil {
			body = u.auctionBody(payload.Provider)
		}
		if u.auctionStatus != nil {
			status = u.auctionStatus(payload.Provider)
		}
		writeBodyJSON(w, status, body)
		return
	case r.URL.Path == "/api/v1/ads/impression" && r.Method == http.MethodPost:
		u.impressions = append(u.impressions, rec)
		status := u.impressionStatus
		if status == 0 {
			status = http.StatusOK
		}
		u.mu.Unlock()
		body := u.impressionBody
		if body == "" {
			body = `{"ok":true}`
		}
		writeBodyJSON(w, status, body)
		return
	case r.URL.Path == "/api/v1/ads/click" && r.Method == http.MethodPost:
		u.clicks++
		u.mu.Unlock()
		writeBodyJSON(w, 200, `{"ok":true}`)
		return
	case r.URL.Path == "/api/v1/chat/completions" && r.Method == http.MethodPost:
		u.chats++
		u.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"id\":\"x\"}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		return
	}
	u.mu.Unlock()
	writeBodyJSON(w, 404, `{"error":"not found"}`)
}

func (u *chatAdFakeUpstream) counts() (auctions, impressions, clicks, chats int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.auctions), len(u.impressions), u.clicks, u.chats
}

// chatAdLegSink captures ledger emissions for assertions.
type chatAdLegSink struct {
	mu   sync.Mutex
	legs []ChatAdLeg
}

func captureChatAdLegs() *chatAdLegSink {
	s := &chatAdLegSink{}
	SetRecordChatAdLeg(func(l ChatAdLeg) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.legs = append(s.legs, l)
	})
	return s
}

func (s *chatAdLegSink) all() []ChatAdLeg {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ChatAdLeg(nil), s.legs...)
}

func decodeChatAdBody(t *testing.T, raw string) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	return body
}

func assertNoChatAdClick(t *testing.T, u *chatAdFakeUpstream) {
	t.Helper()
	_, _, clicks, _ := u.counts()
	if clicks != 0 {
		t.Fatalf("recorded %d POST /api/v1/ads/click without a user gesture (click leg retired)", clicks)
	}
}

// TestChatAdRoundFiresAuctionAndImpression pins the chat-surface round: one
// gravity auction on surface cli_chat, one impression acking the
// server-issued impUrl, ledger events for both legs (titles/brands only,
// credits from the impression grant), and no click anywhere. The auction
// carries the stable per-session id (ad-request.ts:153) and empty messages
// (no transcript supplied on this call).
func TestChatAdRoundFiresAuctionAndImpression(t *testing.T) {
	resetChatAdTrackerForTest()
	defer resetChatAdTrackerForTest()
	sink := captureChatAdLegs()
	defer func() { SetRecordChatAdLeg(nil) }()

	srv := newChatAdFakeUpstream()
	defer srv.Close()
	srv.auctionBody = func(provider string) string {
		if provider == "gravity" {
			return `{"ads":[{"impUrl":"https://gravity.example/imp/chat-1","title":"T","brand":"B"}],"provider":"gravity"}`
		}
		return `{"ads":[]}`
	}
	srv.impressionBody = `{"creditsGranted":5}`

	client, err := New("tok-a", testConfig(srv.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	client.fireChatAdRound(context.Background(), nil)

	auctions, impressions, _, _ := srv.counts()
	if auctions != 1 {
		t.Fatalf("auctions = %d, want 1 (gravity only; fallback not reached)", auctions)
	}
	if impressions != 1 {
		t.Fatalf("impressions = %d, want 1", impressions)
	}
	assertNoChatAdClick(t, srv)

	srv.mu.Lock()
	auction := srv.auctions[0]
	imp := srv.impressions[0]
	srv.mu.Unlock()
	auctionBody := decodeChatAdBody(t, auction.body)
	if got := auctionBody["surface"]; got != chatAdSurface {
		t.Errorf("auction surface = %v, want %q", got, chatAdSurface)
	}
	if got := auctionBody["provider"]; got != "gravity" {
		t.Errorf("auction provider = %v, want gravity", got)
	}
	if got := auction.header.Get("User-Agent"); got != freebuffCliUA {
		t.Errorf("auction User-Agent = %q, want the CLI product UA %q", got, freebuffCliUA)
	}
	if msgs, _ := auctionBody["messages"].([]any); len(msgs) != 0 {
		t.Errorf("auction messages = %v, want [] (no transcript supplied)", msgs)
	}
	if sid, _ := auctionBody["sessionId"].(string); sid == "" {
		t.Error("auction missing sessionId, want the stable per-session id (ad-request.ts:153)")
	}
	impBody := decodeChatAdBody(t, imp.body)
	if got := impBody["impUrl"]; got != "https://gravity.example/imp/chat-1" {
		t.Errorf("impression impUrl = %v, want the auction-issued impUrl (never invented)", got)
	}
	eventID, _ := impBody["clientEventId"].(string)
	if eventID == "" {
		t.Error("impression missing clientEventId")
	} else if got := imp.header.Get("X-Freebuff-Event-Id"); got != eventID {
		t.Errorf("X-Freebuff-Event-Id = %q, want echoed body clientEventId %q", got, eventID)
	}

	legs := sink.all()
	if len(legs) != 2 {
		t.Fatalf("ledger legs = %d, want 2 (auction + impression)", len(legs))
	}
	if legs[0].Leg != "auction" || legs[0].Provider != "gravity" {
		t.Errorf("leg[0] = %+v, want gravity auction", legs[0])
	}
	if legs[0].Title != "T" || legs[0].Brand != "B" {
		t.Errorf("leg[0] title/brand = %q/%q, want T/B", legs[0].Title, legs[0].Brand)
	}
	if legs[1].Leg != "impression" || legs[1].Provider != "gravity" {
		t.Errorf("leg[1] = %+v, want gravity impression", legs[1])
	}
	if legs[1].Credits == nil || *legs[1].Credits != 5 {
		t.Errorf("leg[1] credits = %+v, want 5 (impression grant)", legs[1].Credits)
	}
	for i, l := range legs {
		if l.Surface != chatAdSurface {
			t.Errorf("leg[%d] surface = %q, want %q", i, l.Surface, chatAdSurface)
		}
		if l.TS.IsZero() {
			t.Errorf("leg[%d] missing timestamp", i)
		}
		if l.Error != "" {
			t.Errorf("leg[%d] error = %q, want empty on success", i, l.Error)
		}
	}
}

// TestChatAdRoundDedupesImpUrl mirrors claimAdImpression: repeat auctions of
// the same creative auction again but never re-ack.
func TestChatAdRoundDedupesImpUrl(t *testing.T) {
	resetChatAdTrackerForTest()
	defer resetChatAdTrackerForTest()
	sink := captureChatAdLegs()
	defer func() { SetRecordChatAdLeg(nil) }()

	srv := newChatAdFakeUpstream()
	defer srv.Close()
	srv.auctionBody = func(provider string) string {
		return `{"ads":[{"impUrl":"https://gravity.example/imp/same"}]}`
	}

	client, err := New("tok-a", testConfig(srv.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	client.fireChatAdRound(context.Background(), nil)
	client.fireChatAdRound(context.Background(), nil)

	auctions, impressions, _, _ := srv.counts()
	if auctions != 2 {
		t.Fatalf("auctions = %d, want 2 (every round re-auctions)", auctions)
	}
	if impressions != 1 {
		t.Fatalf("impressions = %d, want 1 (second impUrl already acked)", impressions)
	}
	assertNoChatAdClick(t, srv)

	legs := sink.all()
	if len(legs) != 3 {
		t.Fatalf("ledger legs = %d, want 3 (auction, impression, auction)", len(legs))
	}
	if legs[2].Leg != "auction" {
		t.Errorf("leg[2] = %+v, want the second auction (no second impression leg)", legs[2])
	}
}

// TestChatAdRoundSingleAuctionOnError pins the single-auction rail: a failed
// gravity auction emits one error leg and the round ends — no second
// auction. The CLI fires exactly one buildAdAuctionRequest per tick
// (use-gravity-ad.ts:535-543); the server owns fallback ordering
// (landing-screen.tsx:479), so a proxy-side retry on another provider
// would be traffic the genuine client never sends.
func TestChatAdRoundSingleAuctionOnError(t *testing.T) {
	resetChatAdTrackerForTest()
	defer resetChatAdTrackerForTest()
	sink := captureChatAdLegs()
	defer func() { SetRecordChatAdLeg(nil) }()

	srv := newChatAdFakeUpstream()
	defer srv.Close()
	srv.auctionBody = func(provider string) string { return `{"error":"boom"}` }
	srv.auctionStatus = func(provider string) int { return http.StatusInternalServerError }

	client, err := New("tok-a", testConfig(srv.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	client.fireChatAdRound(context.Background(), nil)

	auctions, impressions, _, _ := srv.counts()
	if auctions != 1 {
		t.Fatalf("auctions = %d, want 1 (single gravity auction, never a provider chain)", auctions)
	}
	if impressions != 0 {
		t.Fatalf("impressions = %d, want 0 (failed auction acks nothing)", impressions)
	}
	assertNoChatAdClick(t, srv)

	legs := sink.all()
	if len(legs) != 1 {
		t.Fatalf("ledger legs = %d, want 1 (gravity auction error)", len(legs))
	}
	if legs[0].Leg != "auction" || legs[0].Provider != "gravity" || legs[0].Error == "" {
		t.Errorf("leg[0] = %+v, want gravity auction with error", legs[0])
	}
}

// TestChatAdRoundSingleAuctionOnEmpty pins the zero-fill rail: a gravity
// auction answered with no ads ends the round — no second auction. The
// CLI's tick falls back to its display cache here (use-gravity-ad.ts:610-619),
// which the proxy has none of, so the round ends instead of spending an
// auction the genuine client never sends.
func TestChatAdRoundSingleAuctionOnEmpty(t *testing.T) {
	resetChatAdTrackerForTest()
	defer resetChatAdTrackerForTest()
	sink := captureChatAdLegs()
	defer func() { SetRecordChatAdLeg(nil) }()

	srv := newChatAdFakeUpstream()
	defer srv.Close()
	srv.auctionBody = func(provider string) string { return `{"ads":[]}` }

	client, err := New("tok-a", testConfig(srv.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	client.fireChatAdRound(context.Background(), nil)

	auctions, impressions, _, _ := srv.counts()
	if auctions != 1 {
		t.Fatalf("auctions = %d, want 1 (zero fills never trigger a second auction)", auctions)
	}
	if impressions != 0 {
		t.Fatalf("impressions = %d, want 0 (nothing served, nothing acked)", impressions)
	}
	assertNoChatAdClick(t, srv)

	legs := sink.all()
	if len(legs) != 1 {
		t.Fatalf("ledger legs = %d, want 1 (clean gravity auction, no fills)", len(legs))
	}
	if legs[0].Leg != "auction" || legs[0].Provider != "gravity" || legs[0].Error != "" {
		t.Errorf("leg[0] = %+v, want clean gravity auction", legs[0])
	}
}

// TestChatAdRoundAcksServerFallbackCreative pins the zeroclick cache rule:
// a non-gravity creative ANSWERED on the single gravity auction is still
// acked for the live request — the zeroclick exclusion
// (use-gravity-ad.ts:151-154) only keeps such offers out of the display
// rotation cache, which the proxy has none of. The ledger records the
// SERVED provider (use-gravity-ad.ts:575), and still exactly one auction
// fires.
func TestChatAdRoundAcksServerFallbackCreative(t *testing.T) {
	resetChatAdTrackerForTest()
	defer resetChatAdTrackerForTest()
	sink := captureChatAdLegs()
	defer func() { SetRecordChatAdLeg(nil) }()

	srv := newChatAdFakeUpstream()
	defer srv.Close()
	srv.auctionBody = func(provider string) string {
		return `{"ads":[{"impUrl":"https://zc.example/imp/2","title":"Z","brand":"ZC"}],"provider":"zeroclick"}`
	}

	client, err := New("tok-a", testConfig(srv.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	client.fireChatAdRound(context.Background(), nil)

	auctions, impressions, _, _ := srv.counts()
	if auctions != 1 {
		t.Fatalf("auctions = %d, want 1 (server fallback arrives on the gravity auction)", auctions)
	}
	if impressions != 1 {
		t.Fatalf("impressions = %d, want 1 (served creative acked for the live request)", impressions)
	}
	assertNoChatAdClick(t, srv)

	srv.mu.Lock()
	auction := srv.auctions[0]
	imp := srv.impressions[0]
	srv.mu.Unlock()
	if got := decodeChatAdBody(t, auction.body)["provider"]; got != "gravity" {
		t.Errorf("auction provider = %v, want gravity (server falls back itself)", got)
	}
	if got := decodeChatAdBody(t, imp.body)["impUrl"]; got != "https://zc.example/imp/2" {
		t.Errorf("impression impUrl = %v, want the served zeroclick impUrl", got)
	}

	legs := sink.all()
	if len(legs) != 2 {
		t.Fatalf("ledger legs = %d, want 2 (auction + impression)", len(legs))
	}
	if legs[0].Leg != "auction" || legs[0].Provider != "zeroclick" {
		t.Errorf("leg[0] = %+v, want zeroclick auction (served provider recorded)", legs[0])
	}
	if legs[1].Leg != "impression" || legs[1].Provider != "zeroclick" {
		t.Errorf("leg[1] = %+v, want zeroclick impression", legs[1])
	}
}

// TestChatAdImpressionFailureIsBestEffort pins that a 500 on the impression
// leg surfaces nowhere: the round ends, an error leg is emitted, no click —
// and no second auction fires (the failed ack is not chased with another
// provider; the single-auction rail ends here).
func TestChatAdImpressionFailureIsBestEffort(t *testing.T) {
	resetChatAdTrackerForTest()
	defer resetChatAdTrackerForTest()
	sink := captureChatAdLegs()
	defer func() { SetRecordChatAdLeg(nil) }()

	srv := newChatAdFakeUpstream()
	defer srv.Close()
	srv.auctionBody = func(provider string) string {
		return `{"ads":[{"impUrl":"https://gravity.example/imp/9"}],"provider":"gravity"}`
	}
	srv.impressionStatus = http.StatusInternalServerError

	client, err := New("tok-a", testConfig(srv.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	client.fireChatAdRound(context.Background(), nil)

	auctions, impressions, _, _ := srv.counts()
	if auctions != 1 {
		t.Fatalf("auctions = %d, want 1 (no fallback auction after the failed ack)", auctions)
	}
	if impressions != 1 {
		t.Fatalf("impressions = %d, want 1 attempt (failure swallowed, not retried)", impressions)
	}
	assertNoChatAdClick(t, srv)

	legs := sink.all()
	if len(legs) != 2 {
		t.Fatalf("legs = %+v, want 2 (gravity auction, impression error)", legs)
	}
	if legs[0].Leg != "auction" || legs[0].Provider != "gravity" || legs[0].Error != "" {
		t.Errorf("leg[0] = %+v, want the clean gravity auction leg", legs[0])
	}
	if legs[1].Leg != "impression" || legs[1].Provider != "gravity" || legs[1].Error == "" {
		t.Errorf("leg[1] = %+v, want the gravity impression error leg", legs[1])
	}
}

// TestChatAdBurstCap pins the MAX_ADS_AFTER_ACTIVITY cadence on the gate:
// at most 3 rounds per activity burst, and a 30s idle gap opens a new one.
func TestChatAdBurstCap(t *testing.T) {
	resetChatAdTrackerForTest()
	defer resetChatAdTrackerForTest()
	cur := time.Now()
	chatAds.mu.Lock()
	chatAds.now = func() time.Time { return cur }
	chatAds.mu.Unlock()

	for i := 0; i < 3; i++ {
		if !chatAds.servedAndClaim() {
			t.Fatalf("round %d denied inside a fresh burst, want 3 allowed", i+1)
		}
	}
	if chatAds.servedAndClaim() {
		t.Fatal("4th round allowed in one burst, want cap at 3")
	}
	// Still idle-hot 29s later: denied.
	cur = cur.Add(29 * time.Second)
	if chatAds.servedAndClaim() {
		t.Fatal("round allowed 29s after burst start with no idle gap, want denied")
	}
	// Past the 30s idle threshold (measured from the last served turn,
	// denied turns stamp activity too): new burst, allowed again.
	cur = cur.Add(31 * time.Second)
	if !chatAds.servedAndClaim() {
		t.Fatal("round denied after a 30s idle gap, want a new burst")
	}
}

// TestChatCompletionsSuccessFiresChatAdRound is the end-to-end pin: a
// served chat turn (2xx) fires the chat-surface round in the background
// while the chat response itself streams back untouched.
func TestChatCompletionsSuccessFiresChatAdRound(t *testing.T) {
	resetChatAdTrackerForTest()
	defer resetChatAdTrackerForTest()
	_ = captureChatAdLegs()
	defer func() { SetRecordChatAdLeg(nil) }()

	srv := newChatAdFakeUpstream()
	defer srv.Close()
	srv.auctionBody = func(provider string) string {
		if provider == "gravity" {
			return `{"ads":[{"impUrl":"https://gravity.example/imp/live"}]}`
		}
		return `{"ads":[]}`
	}

	client, err := New("tok-a", testConfig(srv.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"},
		[]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	body, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("chat body: %v", err)
	}
	if !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("chat body = %q, want the streamed completion untouched", body)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		auctions, impressions, _, _ := srv.counts()
		if auctions >= 1 && impressions >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no chat-surface round after a served turn: auctions=%d impressions=%d", auctions, impressions)
		}
		time.Sleep(10 * time.Millisecond)
	}
	assertNoChatAdClick(t, srv)

	srv.mu.Lock()
	auction := srv.auctions[0]
	srv.mu.Unlock()
	if got := decodeChatAdBody(t, auction.body)["surface"]; got != chatAdSurface {
		t.Errorf("auction surface = %v, want %q", got, chatAdSurface)
	}
}

// TestChatCompletionsFailureFiresNothing pins the traffic key: a failed
// chat turn (no 2xx) stamps no activity and fires no round.
func TestChatCompletionsFailureFiresNothing(t *testing.T) {
	resetChatAdTrackerForTest()
	defer resetChatAdTrackerForTest()
	_ = captureChatAdLegs()
	defer func() { SetRecordChatAdLeg(nil) }()

	srv := newChatAdFakeUpstream()
	defer srv.Close()

	client, err := New("tok-a", testConfig(srv.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	// Unknown model shape with no chat route hit: force a failure by
	// closing the server first is racy; instead call with a cancelled
	// context so ChatCompletions returns before any 2xx.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = client.ChatCompletions(ctx, ChatOptions{Model: "m", RunID: "r"}, []byte(`{"model":"m"}`))

	time.Sleep(200 * time.Millisecond)
	auctions, impressions, _, _ := srv.counts()
	if auctions != 0 || impressions != 0 {
		t.Fatalf("round fired without a served turn: auctions=%d impressions=%d", auctions, impressions)
	}
	assertNoChatAdClick(t, srv)
}

// TestAuctionBodyPinsHonestSignals pins the R6 auction body shape on both
// surfaces: the honestly-known fields are sent (provider, messages,
// sessionId, device block, browser-like userAgent, surface) under the CLI
// product UA, and every render/project signal the proxy cannot honestly
// know is absent (traceContext, sponsoredCapability, capabilityInspection,
// placementId, placementIds — the server falls back for each). sessionId
// is ALWAYS sent (ad-request.ts:153), and cliDockArm is absent only while
// the dock policy is unresolved (pinned separately). Reference:
// buildAdAuctionRequest, upstream/freebuff cli/src/ads/ad-request.ts:141-178.
func TestAuctionBodyPinsHonestSignals(t *testing.T) {
	for _, surface := range []string{"waiting_room", chatAdSurface} {
		t.Run("surface="+surface, func(t *testing.T) {
			resetAdsSessionIDsForTest()
			resetAdsPolicyForTest()
			srv := newChatAdFakeUpstream()
			defer srv.Close()

			client, err := New("tok-ads-shape-"+surface, testConfig(srv.URL(), nil))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.requestAdsForSurface(context.Background(), "gravity", surface, nil); err != nil {
				t.Fatal(err)
			}

			auctions, _, _, _ := srv.counts()
			if auctions != 1 {
				t.Fatalf("auctions = %d, want 1", auctions)
			}
			srv.mu.Lock()
			rec := srv.auctions[0]
			srv.mu.Unlock()

			if got := rec.header.Get("User-Agent"); got != freebuffCliUA {
				t.Errorf("auction User-Agent = %q, want CLI product UA %q (ad-client-identity.ts:41-45)", got, freebuffCliUA)
			}
			body := decodeChatAdBody(t, rec.body)
			if got := body["provider"]; got != "gravity" {
				t.Errorf("provider = %v, want gravity", got)
			}
			if got := body["surface"]; got != surface {
				t.Errorf("surface = %v, want %q", got, surface)
			}
			if msgs, ok := body["messages"].([]any); !ok {
				t.Errorf("messages = %v (%T), want JSON array [] (Go nil slice must not encode as null)", body["messages"], body["messages"])
			} else if len(msgs) != 0 {
				t.Errorf("messages = %v, want []", msgs)
			}
			if sid, _ := body["sessionId"].(string); sid == "" {
				t.Error("auction missing sessionId, want the stable per-session id (ad-request.ts:153 sends it on every auction)")
			}
			if got, _ := body["userAgent"].(string); got != adBrowserUserAgent() {
				t.Errorf("userAgent = %q, want shared browser-like UA", got)
			}
			dev, _ := body["device"].(map[string]any)
			if dev == nil {
				t.Fatal("device block missing")
			}
			if got := dev["os"]; got != deviceOS() {
				t.Errorf("device.os = %v, want %q (must agree with body userAgent)", got, deviceOS())
			}
			if got, _ := dev["timezone"].(string); got == "" {
				t.Error("device.timezone missing")
			}
			if got, _ := dev["locale"].(string); got == "" {
				t.Error("device.locale missing")
			}
			if _, has := body["cliDockArm"]; has {
				t.Error("auction carries cliDockArm before the policy resolves, want omitted (null distinct from control, use-dock-panel.ts:48-56)")
			}
			for _, absent := range []string{"traceContext", "sponsoredCapability", "capabilityInspection", "placementId", "placementIds"} {
				if _, has := body[absent]; has {
					t.Errorf("auction carries %q, want omitted (not honestly known; server falls back)", absent)
				}
			}
		})
	}
}

// TestImpressionBodyPinsNoRenderSignals pins the R7 impression body: the
// server-issued impUrl plus the render-independent signals (browser-like
// userAgent, os, clientEventId), with mode and renderDelayMs omitted —
// nothing is rendered here, so there is no agent mode to declare and no
// receipt-to-mount delay to report. The POST carries the CLI product UA with
// the event id echoed in X-Freebuff-Event-Id. Reference: the
// recordImpressionOnce direct-fetch path, upstream/freebuff
// cli/src/hooks/use-gravity-ad.ts:437-461.
func TestImpressionBodyPinsNoRenderSignals(t *testing.T) {
	const impURL = "https://gravity.example/imp/pin-1"
	payload := impressionPayload(impURL)
	if got := payload["impUrl"]; got != impURL {
		t.Errorf("impUrl = %v, want the server-issued impUrl (never invented)", got)
	}
	if got := payload["userAgent"]; got != adBrowserUserAgent() {
		t.Errorf("userAgent = %v, want shared browser-like UA", got)
	}
	if got := payload["os"]; got != deviceOS() {
		t.Errorf("os = %v, want %q", got, deviceOS())
	}
	eventID, _ := payload["clientEventId"].(string)
	if eventID == "" {
		t.Fatal("clientEventId missing (server reads the echoed header)")
	}
	for _, absent := range []string{"mode", "renderDelayMs"} {
		if _, has := payload[absent]; has {
			t.Errorf("impression carries %q, want omitted (nothing rendered)", absent)
		}
	}

	srv := newChatAdFakeUpstream()
	defer srv.Close()
	srv.impressionBody = `{"creditsGranted":2}`
	client, err := New("tok-a", testConfig(srv.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	grant, err := client.postAdEvent(context.Background(), "/api/v1/ads/impression", payload)
	if err != nil {
		t.Fatal(err)
	}
	if grant != 2 {
		t.Errorf("creditsGranted = %v, want 2", grant)
	}
	_, impressions, _, _ := srv.counts()
	if impressions != 1 {
		t.Fatalf("impressions = %d, want 1", impressions)
	}
	srv.mu.Lock()
	rec := srv.impressions[0]
	srv.mu.Unlock()
	if got := rec.header.Get("User-Agent"); got != freebuffCliUA {
		t.Errorf("impression User-Agent = %q, want CLI product UA %q", got, freebuffCliUA)
	}
	seen := decodeChatAdBody(t, rec.body)
	if got, _ := seen["clientEventId"].(string); got != eventID {
		t.Errorf("posted clientEventId = %q, want %q (header must echo body)", got, eventID)
	}
	if got := rec.header.Get(adEventIDHeader); got != eventID {
		t.Errorf("X-Freebuff-Event-Id = %q, want echoed body clientEventId %q", got, eventID)
	}
}

// TestAuctionSessionIDStableAndRotated pins the sessionId lifecycle
// (upstream/freebuff cli/src/ads/ad-request.ts:153 sends chatSessionId on
// every auction; chat-store.ts:204-207,552-554 mints one uuid per chat and
// regenerates it at the boundary): rounds on one client share one id, and
// the waiting-room chain — the session boundary the pool fires immediately
// before the session create — rotates it, so an id is never reused across
// sessions.
func TestAuctionSessionIDStableAndRotated(t *testing.T) {
	resetChatAdTrackerForTest()
	defer resetChatAdTrackerForTest()
	resetAdsSessionIDsForTest()
	defer resetAdsSessionIDsForTest()
	resetAdsPolicyForTest()
	defer resetAdsPolicyForTest()

	srv := newChatAdFakeUpstream()
	defer srv.Close()
	srv.auctionBody = func(provider string) string {
		return `{"ads":[{"impUrl":"https://gravity.example/imp/sess"}],"provider":"gravity"}`
	}

	client, err := New("tok-ads-session", testConfig(srv.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	sessionOf := func(n int) string {
		t.Helper()
		srv.mu.Lock()
		defer srv.mu.Unlock()
		if len(srv.auctions) <= n {
			t.Fatalf("auctions = %d, want more than %d", len(srv.auctions), n)
		}
		sid, _ := decodeChatAdBody(t, srv.auctions[n].body)["sessionId"].(string)
		return sid
	}

	client.fireChatAdRound(context.Background(), nil)
	client.fireChatAdRound(context.Background(), nil)
	first, second := sessionOf(0), sessionOf(1)
	if first == "" {
		t.Fatal("first auction missing sessionId")
	}
	if second != first {
		t.Errorf("round sessionId changed %q -> %q, want stable within a session", first, second)
	}

	client.FireWaitingRoomChain(context.Background())
	rotated := sessionOf(2)
	if rotated == "" {
		t.Fatal("waiting-room auction missing sessionId")
	}
	if rotated == first {
		t.Error("waiting-room chain reused the prior sessionId, want a fresh id at the session boundary")
	}

	client.fireChatAdRound(context.Background(), nil)
	if got := sessionOf(3); got != rotated {
		t.Errorf("post-chain round sessionId = %q, want the rotated %q", got, rotated)
	}
}

// TestChatAdRoundSendsTranscriptMessages pins the cli_chat transcript
// shaping (upstream/freebuff cli/src/ads/ad-request.ts:36-57,68-91):
// user+assistant text only, system/instruction turns dropped, empties
// dropped, capped to the recent tail. The waiting-room rail keeps messages
// [] (exact match — no history exists there).
func TestChatAdRoundSendsTranscriptMessages(t *testing.T) {
	resetChatAdTrackerForTest()
	defer resetChatAdTrackerForTest()
	resetAdsSessionIDsForTest()
	defer resetAdsSessionIDsForTest()
	resetAdsPolicyForTest()
	defer resetAdsPolicyForTest()
	srv := newChatAdFakeUpstream()
	defer srv.Close()
	srv.auctionBody = func(provider string) string {
		return `{"ads":[{"impUrl":"https://gravity.example/imp/txt"}],"provider":"gravity"}`
	}

	client, err := New("tok-ads-msgs", testConfig(srv.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	msgs := adAuctionMessagesFromChatBody([]byte(`{"model":"m","messages":[
		{"role":"system","content":"You are Buffy, the strategic coding assistant."},
		{"role":"user","content":"  hello  "},
		{"role":"assistant","content":[{"type":"text","text":"hi there"},{"type":"image_url","image_url":{"url":"x"}}]},
		{"role":"user","content":"   "},
		{"role":"tool","content":"tool output"}
	]}`))
	if msgs[0] != (adAuctionMessage{Role: "user", Content: "hello"}) {
		t.Errorf("msgs[0] = %+v, want trimmed user hello", msgs[0])
	}
	if msgs[1] != (adAuctionMessage{Role: "assistant", Content: "hi there"}) {
		t.Errorf("msgs[1] = %+v, want assistant text part only", msgs[1])
	}
	client.fireChatAdRound(context.Background(), msgs)

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.auctions) != 1 {
		t.Fatalf("auctions = %d, want 1", len(srv.auctions))
	}
	var body struct {
		Messages []adAuctionMessage `json:"messages"`
	}
	if err := json.Unmarshal([]byte(srv.auctions[0].body), &body); err != nil {
		t.Fatalf("auction body not JSON: %v", err)
	}
	if len(body.Messages) != 2 || body.Messages[0] != msgs[0] || body.Messages[1] != msgs[1] {
		t.Errorf("auction messages = %+v, want the shaped transcript %+v", body.Messages, msgs)
	}
}

// TestAdAuctionMessagesTrimmedToRecent pins the documented bound: the proxy
// caps the transcript tail at maxAdAuctionMessages (the CLI sends its full
// history unbounded — ad-request.ts:68-91). The most recent turns survive.
func TestAdAuctionMessagesTrimmedToRecent(t *testing.T) {
	var parts []string
	for i := range maxAdAuctionMessages + 5 {
		parts = append(parts, `{"role":"user","content":"msg-`+strings.Repeat("a", i)+`"}`)
	}
	msgs := adAuctionMessagesFromChatBody([]byte(`{"messages":[` + strings.Join(parts, ",") + `]}`))
	if len(msgs) != maxAdAuctionMessages {
		t.Fatalf("shaped = %d messages, want cap %d", len(msgs), maxAdAuctionMessages)
	}
	if msgs[len(msgs)-1].Content != "msg-"+strings.Repeat("a", maxAdAuctionMessages+4) {
		t.Errorf("tail = %q, want the most recent turn", msgs[len(msgs)-1].Content)
	}
	if got := adAuctionMessagesFromChatBody([]byte(`{"model":"m"}`)); got != nil {
		t.Errorf("message-less body shaped to %+v, want nil (auction sends [])", got)
	}
	if got := adAuctionMessagesFromChatBody([]byte(`not json`)); got != nil {
		t.Errorf("unparseable body shaped to %+v, want nil (auction sends [])", got)
	}
}

// TestDockPolicyFailOpenAndStickyArm pins the dock policy rail
// (upstream/freebuff cli/src/hooks/use-dock-panel.ts:86-100,127-154):
// unresolved means cliDockArm omitted (null distinct from control), a
// resolved arm — expandable OR fail-open control — rides later auctions,
// and every failure mode lands on control.
func TestDockPolicyFailOpenAndStickyArm(t *testing.T) {
	resetAdsSessionIDsForTest()
	defer resetAdsSessionIDsForTest()
	resetAdsPolicyForTest()
	defer resetAdsPolicyForTest()

	var mu sync.Mutex
	policyBody := `{"dockArm":"expandable","partnerPlacementIds":[]}`
	policyStatus := http.StatusOK
	var policyHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == adsPolicyPath && r.Method == http.MethodGet:
			policyHits++
			if got := r.Header.Get("Authorization"); got == "" {
				t.Error("policy request missing Bearer auth")
			}
			writeBodyJSON(w, policyStatus, policyBody)
		case r.URL.Path == "/api/v1/ads" && r.Method == http.MethodPost:
			writeBodyJSON(w, 200, `{"ads":[]}`)
		default:
			writeBodyJSON(w, 404, `{"error":"not found"}`)
		}
	}))
	defer srv.Close()

	client, err := New("tok-ads-dock", testConfig(srv.URL, nil))
	if err != nil {
		t.Fatal(err)
	}

	// Unresolved: first auction omits cliDockArm. Drive the auction through
	// a recording server so the exact body is assertable. The server also
	// sees the background policy GET (empty body), so only auction POSTs
	// are collected.
	var auctionBodies []string
	var amu sync.Mutex
	rec := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if r.URL.Path == "/api/v1/ads" && r.Method == http.MethodPost {
			amu.Lock()
			auctionBodies = append(auctionBodies, string(raw))
			amu.Unlock()
		}
		writeBodyJSON(w, 200, `{"ads":[]}`)
	}))
	defer rec.Close()
	recClient, err := New("tok-ads-dock-unresolved", testConfig(rec.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recClient.requestAdsForSurface(context.Background(), "gravity", "waiting_room", nil); err != nil {
		t.Fatal(err)
	}
	amu.Lock()
	if len(auctionBodies) != 1 {
		t.Fatalf("auctions = %d, want 1", len(auctionBodies))
	}
	first := decodeChatAdBody(t, auctionBodies[0])
	amu.Unlock()
	if _, has := first["cliDockArm"]; has {
		t.Errorf("pre-resolve auction carries cliDockArm, want omitted (null distinct from control)")
	}

	// Resolved expandable: later auctions carry it.
	if got := client.fetchAdsPolicy(context.Background()); got != "expandable" {
		t.Fatalf("fetchAdsPolicy = %q, want expandable", got)
	}
	arm, ok := client.adsDockArm()
	if !ok || arm != "expandable" {
		t.Fatalf("adsDockArm = %q,%v, want expandable,true", arm, ok)
	}
	mu.Lock()
	hitsAfterFirst := policyHits
	mu.Unlock()
	// Sticky once resolved: ensureAdsPolicy never refetches for this token
	// (resolveAdPolicy memoizes per process — use-dock-panel.ts:86-100;
	// per token here, the multi-account analogue).
	client.ensureAdsPolicy(context.Background())
	client.ensureAdsPolicy(context.Background())
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	if policyHits != hitsAfterFirst {
		t.Errorf("policyHits = %d, want %d (resolved policy must never refetch)", policyHits, hitsAfterFirst)
	}
	mu.Unlock()

	// Fail-open: 500 lands on control (and resolves, so control IS sent).
	mu.Lock()
	policyStatus = http.StatusInternalServerError
	mu.Unlock()
	failClient, err := New("tok-ads-dock-fail", testConfig(srv.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := failClient.fetchAdsPolicy(context.Background()); got != "control" {
		t.Errorf("fetchAdsPolicy on 500 = %q, want fail-open control", got)
	}
	if arm, ok := failClient.adsDockArm(); !ok || arm != "control" {
		t.Errorf("adsDockArm after failure = %q,%v, want control,true (resolved fail-open)", arm, ok)
	}

	// Fail-open: garbage arm lands on control too.
	mu.Lock()
	policyStatus = http.StatusOK
	policyBody = `{"dockArm":"mystery"}`
	mu.Unlock()
	garbageClient, err := New("tok-ads-dock-garbage", testConfig(srv.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := garbageClient.fetchAdsPolicy(context.Background()); got != "control" {
		t.Errorf("fetchAdsPolicy on unknown arm = %q, want fail-open control", got)
	}
}

// TestAuctionCarriesResolvedDockArm pins that a resolved arm rides the
// auction as cliDockArm (ad-request.ts:139,175) — including a resolved
// 'control', which the CLI sends because it is truthy.
func TestAuctionCarriesResolvedDockArm(t *testing.T) {
	resetAdsSessionIDsForTest()
	defer resetAdsSessionIDsForTest()
	resetAdsPolicyForTest()
	defer resetAdsPolicyForTest()

	var bodies []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == adsPolicyPath {
			writeBodyJSON(w, 200, `{"dockArm":"control"}`)
			return
		}
		bodies = append(bodies, string(raw))
		writeBodyJSON(w, 200, `{"ads":[]}`)
	}))
	defer srv.Close()

	client, err := New("tok-ads-dock-arm", testConfig(srv.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := client.fetchAdsPolicy(context.Background()); got != "control" {
		t.Fatalf("fetchAdsPolicy = %q, want control", got)
	}
	if _, err := client.requestAdsForSurface(context.Background(), "gravity", chatAdSurface, nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("auctions = %d, want 1", len(bodies))
	}
	body := decodeChatAdBody(t, bodies[0])
	if got := body["cliDockArm"]; got != "control" {
		t.Errorf("cliDockArm = %v, want resolved control sent (truthy, ad-request.ts:175)", got)
	}
}

// TestFirstPartyAckRetriesWithSharedEventID pins the resilient ack
// (upstream/freebuff common/src/ads/first-party-view-ack.ts:148-236):
// three attempts max, ONE event id shared across all of them on the
// X-Freebuff-Event-Id header, no body clientEventId (header-only on this
// transport), and success on the third attempt after two 500s.
func TestFirstPartyAckRetriesWithSharedEventID(t *testing.T) {
	oldDelays := firstPartyRetryDelays
	firstPartyRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	defer func() { firstPartyRetryDelays = oldDelays }()

	var mu sync.Mutex
	var headers []http.Header
	var bodies []string
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		mu.Lock()
		defer mu.Unlock()
		attempts++
		headers = append(headers, r.Header.Clone())
		bodies = append(bodies, string(raw))
		if attempts < 3 {
			writeBodyJSON(w, 500, `{"error":"boom"}`)
			return
		}
		writeBodyJSON(w, 200, `{"creditsGranted":7}`)
	}))
	defer srv.Close()

	client, err := New("tok-ads-fp", testConfig(srv.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	grant, err := client.postFirstPartyImpression(context.Background(), "https://fp.example/imp/1")
	if err != nil {
		t.Fatalf("postFirstPartyImpression: %v", err)
	}
	if grant != 7 {
		t.Errorf("grant = %v, want 7 (third-attempt grant)", grant)
	}

	mu.Lock()
	defer mu.Unlock()
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3 (two 500s then success)", attempts)
	}
	first := headers[0].Get(adEventIDHeader)
	if first == "" {
		t.Fatal("first attempt missing X-Freebuff-Event-Id")
	}
	for i, h := range headers {
		if got := h.Get(adEventIDHeader); got != first {
			t.Errorf("attempt %d event id = %q, want shared %q", i+1, got, first)
		}
		if got := h.Get("User-Agent"); got != freebuffCliUA {
			t.Errorf("attempt %d User-Agent = %q, want CLI product UA", i+1, got)
		}
		if got := h.Get("X-Freebuff-Render-Delay-Ms"); got != "" {
			t.Errorf("attempt %d sent render-delay %q, want omitted (nothing rendered; server stores NULL)", i+1, got)
		}
		body := decodeChatAdBody(t, bodies[i])
		if _, has := body["clientEventId"]; has {
			t.Errorf("attempt %d body carries clientEventId, want header-only (first-party-view-ack.ts:160-162)", i+1)
		}
		if got := body["impUrl"]; got != "https://fp.example/imp/1" {
			t.Errorf("attempt %d impUrl = %v, want the server-issued impUrl", i+1, got)
		}
		if _, has := body["mode"]; has {
			t.Errorf("attempt %d body carries mode, want omitted (no agent mode to declare)", i+1)
		}
	}
}

// TestFirstPartyAckDedupeStops pins dedupe tolerance: a 208 answer stops
// the sequence as success with exactly one attempt (retrying a recorded
// view would mint a second event).
func TestFirstPartyAckDedupeStops(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		writeBodyJSON(w, 208, `{"acknowledgement":"deduped"}`)
	}))
	defer srv.Close()

	client, err := New("tok-ads-fp-dedupe", testConfig(srv.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.postFirstPartyImpression(context.Background(), "https://fp.example/imp/2"); err != nil {
		t.Fatalf("dedupe answer errored: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (dedupe stops the sequence)", attempts)
	}
}

// TestFirstPartyAckClientErrorTerminal pins that a 400 stops the sequence
// without retries (only server errors, timeouts and network errors retry —
// first-party-view-ack.ts:221-224).
func TestFirstPartyAckClientErrorTerminal(t *testing.T) {
	oldDelays := firstPartyRetryDelays
	firstPartyRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	defer func() { firstPartyRetryDelays = oldDelays }()

	var mu sync.Mutex
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		writeBodyJSON(w, 400, `{"error":"bad"}`)
	}))
	defer srv.Close()

	client, err := New("tok-ads-fp-400", testConfig(srv.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.postFirstPartyImpression(context.Background(), "https://fp.example/imp/3"); err == nil {
		t.Fatal("400 ack succeeded, want terminal error")
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (client errors never retry)", attempts)
	}
}

// TestFirstPartyAckTimeoutRetries pins the 2s attempt bound: an attempt
// hung past the timeout is retried with the same event id.
func TestFirstPartyAckTimeoutRetries(t *testing.T) {
	oldTimeout := firstPartyAttemptTimeout
	oldDelays := firstPartyRetryDelays
	firstPartyAttemptTimeout = 100 * time.Millisecond
	firstPartyRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	defer func() {
		firstPartyAttemptTimeout = oldTimeout
		firstPartyRetryDelays = oldDelays
	}()

	var mu sync.Mutex
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := attempts
		attempts++
		mu.Unlock()
		if n == 0 {
			time.Sleep(500 * time.Millisecond)
		}
		writeBodyJSON(w, 200, `{"ok":true}`)
	}))
	defer srv.Close()

	client, err := New("tok-ads-fp-timeout", testConfig(srv.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.postFirstPartyImpression(context.Background(), "https://fp.example/imp/4"); err != nil {
		t.Fatalf("postFirstPartyImpression after timeout retry: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 (hung first attempt retried)", attempts)
	}
}

// TestChatAdRoundUsesFirstPartyTransport pins end-to-end dispatch: a
// first_party creative served on the gravity auction acks via the resilient
// transport (header event id, no body clientEventId), and the ledger
// records the served provider — never claimed unless the server answered
// it (the fixture below is the only first_party on any rail).
func TestChatAdRoundUsesFirstPartyTransport(t *testing.T) {
	resetChatAdTrackerForTest()
	defer resetChatAdTrackerForTest()
	resetAdsSessionIDsForTest()
	defer resetAdsSessionIDsForTest()
	resetAdsPolicyForTest()
	defer resetAdsPolicyForTest()
	sink := captureChatAdLegs()
	defer func() { SetRecordChatAdLeg(nil) }()

	srv := newChatAdFakeUpstream()
	defer srv.Close()
	srv.auctionBody = func(provider string) string {
		return `{"ads":[{"impUrl":"https://fp.example/imp/9","title":"F","brand":"FP"}],"provider":"first_party"}`
	}
	srv.impressionBody = `{"creditsGranted":3}`

	client, err := New("tok-ads-fp-round", testConfig(srv.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	client.fireChatAdRound(context.Background(), nil)

	auctions, impressions, _, _ := srv.counts()
	if auctions != 1 || impressions != 1 {
		t.Fatalf("auctions=%d impressions=%d, want 1/1", auctions, impressions)
	}
	srv.mu.Lock()
	imp := srv.impressions[0]
	srv.mu.Unlock()
	if got := imp.header.Get(adEventIDHeader); got == "" {
		t.Error("first-party impression missing header event id")
	}
	if _, has := decodeChatAdBody(t, imp.body)["clientEventId"]; has {
		t.Error("first-party body carries clientEventId, want header-only")
	}
	legs := sink.all()
	if len(legs) != 2 {
		t.Fatalf("legs = %d, want 2", len(legs))
	}
	if legs[0].Provider != "first_party" || legs[1].Provider != "first_party" {
		t.Errorf("legs = %+v, want served first_party recorded", legs)
	}
	if legs[1].Credits == nil || *legs[1].Credits != 3 {
		t.Errorf("impression credits = %+v, want 3", legs[1].Credits)
	}
}

// TestPostAdEventMintsMissingClientEventID pins the legacy invariant
// (use-gravity-ad.ts:458 — only renderDelayMs is optional): even a caller
// that forgot clientEventId gets one minted, echoed in the header, so no
// headerless event the server cannot join ever goes out.
func TestPostAdEventMintsMissingClientEventID(t *testing.T) {
	srv := newChatAdFakeUpstream()
	defer srv.Close()

	client, err := New("tok-ads-legacy-id", testConfig(srv.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	grant, err := client.postAdEvent(context.Background(), "/api/v1/ads/impression", map[string]any{
		"impUrl":    "https://gravity.example/imp/noid",
		"userAgent": adBrowserUserAgent(),
		"os":        deviceOS(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if grant != 0 {
		t.Errorf("grant = %v, want 0 (fixture sends no grant)", grant)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.impressions) != 1 {
		t.Fatalf("impressions = %d, want 1", len(srv.impressions))
	}
	rec := srv.impressions[0]
	body := decodeChatAdBody(t, rec.body)
	eventID, _ := body["clientEventId"].(string)
	if eventID == "" {
		t.Fatal("transport did not mint the missing clientEventId")
	}
	if got := rec.header.Get(adEventIDHeader); got != eventID {
		t.Errorf("X-Freebuff-Event-Id = %q, want minted %q", got, eventID)
	}
}

// TestWaitingRoomChainSingleAuction pins the waiting-room rail: exactly one
// gravity auction even on zero fills (no zeroclick second auction — the
// server falls back itself, landing-screen.tsx:479), the auction carries
// the fresh session id with messages [], and the streak leg still fires.
func TestWaitingRoomChainSingleAuction(t *testing.T) {
	resetAdsSessionIDsForTest()
	defer resetAdsSessionIDsForTest()
	resetAdsPolicyForTest()
	defer resetAdsPolicyForTest()

	var mu sync.Mutex
	var auctionBodies []string
	streakHits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/api/v1/ads" && r.Method == http.MethodPost:
			raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			auctionBodies = append(auctionBodies, string(raw))
			writeBodyJSON(w, 200, `{"ads":[]}`)
		case r.URL.Path == "/api/v1/freebuff/streak":
			streakHits++
			writeBodyJSON(w, 200, `{"ok":true}`)
		default:
			writeBodyJSON(w, 404, `{"error":"not found"}`)
		}
	}))
	defer srv.Close()

	client, err := New("tok-ads-wr-single", testConfig(srv.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	client.FireWaitingRoomChain(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(auctionBodies) != 1 {
		t.Fatalf("auctions = %d, want exactly 1 (never a provider chain)", len(auctionBodies))
	}
	if streakHits != 1 {
		t.Errorf("streak hits = %d, want 1 (chain finishes after zero fills)", streakHits)
	}
	body := decodeChatAdBody(t, auctionBodies[0])
	if got := body["provider"]; got != "gravity" {
		t.Errorf("provider = %v, want the single gravity auction", got)
	}
	if got := body["surface"]; got != "waiting_room" {
		t.Errorf("surface = %v, want waiting_room", got)
	}
	if msgs, ok := body["messages"].([]any); !ok || len(msgs) != 0 {
		t.Errorf("messages = %v, want [] (exact match — no history exists)", body["messages"])
	}
	if sid, _ := body["sessionId"].(string); sid == "" {
		t.Error("waiting-room auction missing sessionId (ad-request.ts:153 sends it even here)")
	}
}
