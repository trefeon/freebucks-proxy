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
	RecordChatAdLeg = func(l ChatAdLeg) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.legs = append(s.legs, l)
	}
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
// credits from the impression grant), and no click anywhere.
func TestChatAdRoundFiresAuctionAndImpression(t *testing.T) {
	resetChatAdTrackerForTest()
	defer resetChatAdTrackerForTest()
	sink := captureChatAdLegs()
	defer func() { RecordChatAdLeg = nil }()

	srv := newChatAdFakeUpstream()
	defer srv.Close()
	srv.auctionBody = func(provider string) string {
		if provider == "gravity" {
			return `{"ads":[{"impUrl":"https://gravity.example/imp/chat-1","title":"T","brand":"B"}]}`
		}
		return `{"ads":[]}`
	}
	srv.impressionBody = `{"creditsGranted":5}`

	client, err := New("tok-a", testConfig(srv.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	client.fireChatAdRound(context.Background())

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
		t.Errorf("auction messages = %v, want [] (no transcript text)", msgs)
	}
	if _, has := auctionBody["sessionId"]; has {
		t.Error("auction carries sessionId, want omitted (proofing only)")
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
	defer func() { RecordChatAdLeg = nil }()

	srv := newChatAdFakeUpstream()
	defer srv.Close()
	srv.auctionBody = func(provider string) string {
		return `{"ads":[{"impUrl":"https://gravity.example/imp/same"}]}`
	}

	client, err := New("tok-a", testConfig(srv.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	client.fireChatAdRound(context.Background())
	client.fireChatAdRound(context.Background())

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

// TestChatAdRoundFallsBackToZeroclick pins the provider fallback: a failed
// gravity auction emits an error leg and the round continues on zeroclick.
func TestChatAdRoundFallsBackToZeroclick(t *testing.T) {
	resetChatAdTrackerForTest()
	defer resetChatAdTrackerForTest()
	sink := captureChatAdLegs()
	defer func() { RecordChatAdLeg = nil }()

	srv := newChatAdFakeUpstream()
	defer srv.Close()
	srv.auctionBody = func(provider string) string {
		if provider == "zeroclick" {
			return `{"ads":[{"impUrl":"https://zc.example/imp/2","title":"Z","brand":"ZC"}]}`
		}
		return `{"error":"boom"}`
	}
	srv.auctionStatus = func(provider string) int {
		if provider == "gravity" {
			return http.StatusInternalServerError
		}
		return http.StatusOK
	}

	client, err := New("tok-a", testConfig(srv.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	client.fireChatAdRound(context.Background())

	auctions, impressions, _, _ := srv.counts()
	if auctions != 2 {
		t.Fatalf("auctions = %d, want 2 (gravity then zeroclick fallback)", auctions)
	}
	if impressions != 1 {
		t.Fatalf("impressions = %d, want 1 (zeroclick creative)", impressions)
	}
	assertNoChatAdClick(t, srv)

	srv.mu.Lock()
	second := srv.auctions[1]
	imp := srv.impressions[0]
	srv.mu.Unlock()
	if got := decodeChatAdBody(t, second.body)["provider"]; got != "zeroclick" {
		t.Errorf("second auction provider = %v, want zeroclick", got)
	}
	if got := decodeChatAdBody(t, imp.body)["impUrl"]; got != "https://zc.example/imp/2" {
		t.Errorf("impression impUrl = %v, want the zeroclick impUrl", got)
	}

	legs := sink.all()
	if len(legs) != 3 {
		t.Fatalf("ledger legs = %d, want 3 (gravity error, zeroclick auction, impression)", len(legs))
	}
	if legs[0].Leg != "auction" || legs[0].Provider != "gravity" || legs[0].Error == "" {
		t.Errorf("leg[0] = %+v, want gravity auction with error", legs[0])
	}
	if legs[1].Leg != "auction" || legs[1].Provider != "zeroclick" || legs[1].Error != "" {
		t.Errorf("leg[1] = %+v, want clean zeroclick auction", legs[1])
	}
	if legs[2].Leg != "impression" || legs[2].Provider != "zeroclick" {
		t.Errorf("leg[2] = %+v, want zeroclick impression", legs[2])
	}
	if legs[2].Credits != nil {
		t.Errorf("leg[2] credits = %+v, want nil (server sent no grant)", legs[2].Credits)
	}
}

// TestChatAdImpressionFailureIsBestEffort pins that a 500 on the impression
// leg surfaces nowhere: the round ends, an error leg is emitted, no click.
func TestChatAdImpressionFailureIsBestEffort(t *testing.T) {
	resetChatAdTrackerForTest()
	defer resetChatAdTrackerForTest()
	sink := captureChatAdLegs()
	defer func() { RecordChatAdLeg = nil }()

	srv := newChatAdFakeUpstream()
	defer srv.Close()
	srv.auctionBody = func(provider string) string {
		if provider == "gravity" {
			return `{"ads":[{"impUrl":"https://gravity.example/imp/9"}]}`
		}
		return `{"ads":[]}`
	}
	srv.impressionStatus = http.StatusInternalServerError

	client, err := New("tok-a", testConfig(srv.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	client.fireChatAdRound(context.Background())

	_, impressions, _, _ := srv.counts()
	if impressions != 1 {
		t.Fatalf("impressions = %d, want 1 attempt (failure swallowed, not retried)", impressions)
	}
	assertNoChatAdClick(t, srv)

	auctions, _, _, _ := srv.counts()
	if auctions != 2 {
		t.Fatalf("auctions = %d, want 2 (gravity then zeroclick fallback after the failed ack)", auctions)
	}
	legs := sink.all()
	if len(legs) != 3 {
		t.Fatalf("legs = %+v, want 3 (gravity auction, impression error, zeroclick auction)", legs)
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
	defer func() { RecordChatAdLeg = nil }()

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
	defer func() { RecordChatAdLeg = nil }()

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
