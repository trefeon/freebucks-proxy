package server_test

import (
	"bytes"
	"encoding/json"
	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/testutil"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestWaitingRoom503ThenRetry(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionSequence = []string{"queued", "active"}
	mock.QueuePosition = 3
	mock.QueueDepth = 7
	mock.EstimatedWaitMs = 50
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("first request status = %d, want 503: %s", resp.StatusCode, data)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "1" {
		t.Errorf("Retry-After = %q, want 1 (ceil of ~50ms)", ra)
	}
	if !strings.Contains(string(data), "waiting_room_queued") {
		t.Errorf("body missing waiting_room_queued: %s", data)
	}

	// Wait out the queue window, then the session must advance to active.
	// Poll the retry instead of sleeping: the queued session only advances
	// after its pollAt window, so keep retrying until the queue clears.
	var data2 []byte
	eventually(t, "waiting room to clear", func() bool {
		resp2, d2 := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
		data2 = d2
		return resp2.StatusCode == http.StatusOK
	})
	if !strings.HasSuffix(string(data2), "data: [DONE]\n\n") {
		t.Errorf("retry stream must end with [DONE]: %q", data2)
	}
}

func TestChatRateLimitSurfaced(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.RateLimit = true
	ts, p := newTestServer(t, nil, mock)

	// First request: upstream 429 rate_limited → 429 + Retry-After (the
	// gateway must back off for the exact window, not hammer a 502).
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", resp.StatusCode, data)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "48550" {
		t.Errorf("Retry-After = %q, want 48550 (ceil of 48549499ms)", ra)
	}
	if !strings.Contains(string(data), `"code":"rate_limited"`) {
		t.Errorf("body missing rate_limited code: %s", data)
	}
	if !strings.Contains(string(data), "reset at 2026-08-12T07:00:00") {
		t.Errorf("body missing resetAt: %s", data)
	}

	// MASQ sticky-spill: this 429 fired on the ADMISSION path (RateLimit
	// mode 429s every route, so no session was ever admitted), and
	// admission 429s are remembered per model — the second request skips
	// the dead lane with no upstream contact yet surfaces the same 429
	// shape (never a 502). The re-hit proof inverts vs the old turn-time
	// reading: the total-request counter must NOT grow across the second
	// request, and no blanket cooldown is written (per-model memory only).
	upstreamBefore := mock.RequestsSnapshot()
	resp2, data2 := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429: %s", resp2.StatusCode, data2)
	}
	if ra := resp2.Header.Get("Retry-After"); ra != "48550" {
		t.Errorf("second Retry-After = %q, want 48550", ra)
	}
	if !strings.Contains(string(data2), `"code":"rate_limited"`) {
		t.Errorf("second body missing rate_limited code: %s", data2)
	}
	if !strings.Contains(string(data2), "reset at 2026-08-12T07:00:00") {
		t.Errorf("second body missing resetAt: %s", data2)
	}
	if got := mock.RequestsSnapshot(); got != upstreamBefore {
		t.Errorf("upstream requests = %d after second request (was %d), want no growth (remembered admission refusal, no contact)", got, upstreamBefore)
	}
	snap := p.Snapshot()[0]
	if !snap.CooldownUntil.IsZero() {
		t.Errorf("cooldown until = %v, want zero (per-model memory only, no blanket cooldown)", snap.CooldownUntil)
	}
}

// TestChatTurnTimeRateLimitRehitsUpstream pins the preserved turn-time
// policy: a 429 that fires on the CHAT path (healthy admission, refusal at
// turn time) writes no memory of any kind — every chat re-hits upstream.
// Contrast TestChatRateLimitSurfaced, where the 429 fires at admission and
// IS remembered per model. The chat path (chatAttempt) releases the lease
// with no cooldown write on ErrRateLimited, so the proof is counter growth
// across the second chat plus a zero blanket cooldown.
func TestChatTurnTimeRateLimitRehitsUpstream(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// Healthy admission; only the chat turn refuses, with the exact
	// RateLimit-mode shape (retryAfterMs 48549499, resetAt
	// 2026-08-12T07:00:00.000Z).
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		writeRawJSON(w, http.StatusTooManyRequests, `{"model":"deepseek/deepseek-v4-flash","entitlementBreakdown":{"base":3,"referral":0,"streak":0},"limit":3,"period":"pacific_day","resetTimeZone":"America/Los_Angeles","resetAt":"2026-08-12T07:00:00.000Z","windowHours":24,"recentCount":3.6,"status":"rate_limited","accessTier":"limited","retryAfterMs":48549499}`)
	}
	ts, p := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("first status = %d, want 429: %s", resp.StatusCode, data)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "48550" {
		t.Errorf("first Retry-After = %q, want 48550 (ceil of 48549499ms)", ra)
	}
	if !strings.Contains(string(data), `"code":"rate_limited"`) {
		t.Errorf("first body missing rate_limited code: %s", data)
	}

	upstreamBefore := mock.RequestsSnapshot()
	resp2, data2 := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want 429: %s", resp2.StatusCode, data2)
	}
	if ra := resp2.Header.Get("Retry-After"); ra != "48550" {
		t.Errorf("second Retry-After = %q, want 48550", ra)
	}
	if got := mock.RequestsSnapshot(); got <= upstreamBefore {
		t.Errorf("upstream requests = %d after second chat (was %d), want growth (turn-time 429 writes no memory)", got, upstreamBefore)
	}
	snap := p.Snapshot()[0]
	if !snap.CooldownUntil.IsZero() {
		t.Errorf("cooldown until = %v, want zero (turn-time 429 writes no cooldown)", snap.CooldownUntil)
	}
}

// TestChatSessionSupersededTerminal pins #159: 409 session_superseded
// (another instance took over the account, endsTheSession:true) is TERMINAL
// for the current request — the cached session is dropped immediately and
// the error surfaces with NO in-request retry. The success canary proves the
// retry never fires: one chat attempt, one session create, 503
// session_superseded (the #119 re-admit-once behavior wasted a fresh daily
// session slot against the superseding instance and still failed).
func TestChatSessionSupersededTerminal(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// First chat attempt returns session_superseded; a SECOND attempt would
	// succeed (canary) — the response must still be 503 and the canary must
	// never fire, proving the dead instance is not re-attempted.
	callCount := 0
	originalHandler := mock.ChatHandler
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"session_superseded"}}`))
			return
		}
		if originalHandler != nil {
			originalHandler(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + chunk("cmpl-test", 1234567890, `"choices":[{"delta":{"content":"ok"},"index":0}]`) + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "session_superseded") {
		t.Errorf("body missing session_superseded: %s", data)
	}
	if got := callCount; got != 1 {
		t.Errorf("upstream chat attempts = %d, want exactly 1 (no retry on the dead instance)", got)
	}
	if got := len(mock.RecordedChatHeaders); got != 1 {
		t.Errorf("upstream chat attempts (recorded) = %d, want exactly 1", got)
	}
	if got := mock.SessionCreates; got != 1 {
		t.Errorf("session creates = %d, want exactly 1 (no re-admit against the superseding instance)", got)
	}
}

// TestChatSessionSupersededNextRequestReadmits pins #159: a superseded chat
// invalidates the cached session immediately, so the NEXT request re-admits
// fresh instead of reusing the dead row. Two requests: the first surfaces
// 503 session_superseded (one create), the second succeeds on a NEW session
// (second create — proves the cache was dropped, not reused).
func TestChatSessionSupersededNextRequestReadmits(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatStatus = http.StatusBadRequest
	mock.ChatErrorBody = `{"error":{"message":"session_superseded"}}`
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "session_superseded") {
		t.Errorf("body missing session_superseded: %s", data)
	}
	if got := mock.SessionCreates; got != 1 {
		t.Fatalf("session creates after superseded request = %d, want 1", got)
	}

	// Upstream heals; the next request must create a FRESH session (the
	// superseded row was invalidated, so no cached instance is reused).
	mock.ChatStatus = http.StatusOK
	mock.ChatErrorBody = ""
	resp2, data2 := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second request status = %d, want 200: %s", resp2.StatusCode, data2)
	}
	if got := mock.SessionCreates; got != 2 {
		t.Errorf("session creates after re-admit request = %d, want 2 (fresh session, not cache reuse)", got)
	}
	if got := len(mock.RecordedChatHeaders); got != 2 {
		t.Errorf("upstream chat attempts = %d, want 2 (1 per request — no in-request retry)", got)
	}
}

func TestChatBanSurfaced(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.Ban = true
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), `"code":"account_banned"`) {
		t.Errorf("body missing account_banned code: %s", data)
	}
	ra := resp.Header.Get("Retry-After")
	if ra == "" {
		t.Error("missing Retry-After header")
	}
}

// TestUpstreamRetryableMapsTo503 verifies a Retryable UpstreamError
// (deployment_outside_hours) surfaces as 503 upstream_retryable so clients
// back off and retry later, not a hard 502.
func TestUpstreamRetryableMapsTo503(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatStatus = http.StatusServiceUnavailable
	mock.ChatErrorBody = `{"error":"deployment_outside_hours"}`
	ts, _ := newTestServer(t, nil, mock)
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "upstream_retryable") {
		t.Errorf("body = %s, want upstream_retryable code", data)
	}
}

// TestUpstreamRetryableNotBlindRetried verifies chatAttempt does NOT retry a
// Retryable UpstreamError (deployment_outside_hours): the flag means "worth
// retrying later", not "transient", so a blind retry must not burn a second
// lease against the same wall. The mock must see exactly one chat call.
func TestUpstreamRetryableNotBlindRetried(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var chatCalls atomic.Int32
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		chatCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"deployment_outside_hours","type":"upstream_error","code":"deployment_outside_hours"}}`)
	}
	ts, _ := newTestServer(t, nil, mock)
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "upstream_retryable") {
		t.Errorf("body = %s, want upstream_retryable code", data)
	}
	if got := chatCalls.Load(); got != 1 {
		t.Errorf("upstream chat calls = %d, want 1 (Retryable errors must not be blind-retried)", got)
	}
}

// TestChatCapacityDeferredSurfaced429 verifies #105 (server half): once the
// client-side capacity-deferred budget is exhausted, the gateway surfaces the
// free tier's transient capacity queue as 429 free_mode_capacity_deferred +
// Retry-After (the upstream window) — never the old bare 502 upstream_
// unavailable or a generic 503 upstream_retryable — so downstream clients
// honor the window instead of re-POSTing immediately. The mock sees exactly
// one chat call: the typed error unwraps to a Retryable UpstreamError, so
// chatAttempt must not blind-retry it a second time.
func TestChatCapacityDeferredSurfaced429(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var chatCalls atomic.Int32
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		chatCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"free_mode_capacity_deferred","message":"Free mode is at capacity; your request will be retried automatically","retryAfterMs":7000}}`)
	}
	// TRANSIENT_RETRIES=0 = exhausted budget: the client surfaces the typed
	// CapacityDeferredError immediately (no in-place retry, no retry-after
	// sleep), so the server mapping is exercised on the first call.
	ts, _ := newTestServerCfg(t, nil, func(cfg *config.Config) { cfg.TransientRetries = 0 }, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", resp.StatusCode, data)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "7" {
		t.Errorf("Retry-After = %q, want 7 (the upstream window, ceil seconds)", ra)
	}
	if !strings.Contains(string(data), `"code":"free_mode_capacity_deferred"`) {
		t.Errorf("body missing free_mode_capacity_deferred code: %s", data)
	}
	if got := chatCalls.Load(); got != 1 {
		t.Errorf("upstream chat calls = %d, want 1 (no blind retry after budget exhaustion)", got)
	}
}

// TestChatCapacityDeferredDefaultRetryAfter verifies the 10s Retry-After
// fallback when the upstream free_mode_capacity_deferred response carries no
// retry-after window (the AI SDK's default honor window).
func TestChatCapacityDeferredDefaultRetryAfter(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"free_mode_capacity_deferred","message":"Free mode is at capacity; your request will be retried automatically"}}`)
	}
	ts, _ := newTestServerCfg(t, nil, func(cfg *config.Config) { cfg.TransientRetries = 0 }, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", resp.StatusCode, data)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "10" {
		t.Errorf("Retry-After = %q, want 10 (default window)", ra)
	}
	if !strings.Contains(string(data), `"code":"free_mode_capacity_deferred"`) {
		t.Errorf("body missing free_mode_capacity_deferred code: %s", data)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	resp, _ := doJSON(t, http.MethodGet, ts.URL+"/v1/chat/completions", nil, nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET chat status = %d, want 405", resp.StatusCode)
	}

	resp, _ = doJSON(t, http.MethodPost, ts.URL+"/v1/models", nil, nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST models status = %d, want 405", resp.StatusCode)
	}

	resp, _ = doJSON(t, http.MethodGet, ts.URL+"/v1/nope", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown path status = %d, want 404", resp.StatusCode)
	}
}

func TestChatReasoningEffort(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	ts, _ := newTestServer(t, nil, mock)

	bodyBytes, _ := json.Marshal(map[string]any{
		"model":     modelA,
		"messages":  []any{map[string]any{"role": "user", "content": "hi"}},
		"reasoning": map[string]any{"effort": "max"},
		"stream":    true,
	})

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", bodyBytes, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, data)
	}

	if len(mock.RecordedChatBodies) == 0 {
		t.Fatal("no chat requests recorded upstream")
	}
	var upstreamPayload map[string]any
	if err := json.Unmarshal([]byte(mock.RecordedChatBodies[len(mock.RecordedChatBodies)-1]), &upstreamPayload); err != nil {
		t.Fatalf("unmarshal upstream body: %v: %s", err, data)
	}
	if gotEffort := upstreamPayload["reasoning_effort"]; gotEffort != "max" {
		t.Errorf("upstream reasoning_effort = %v, want \"max\"", gotEffort)
	}
}

// TestChatModelIPLimitedMarked pins the chat-level limited_ip flow: a 409
// session_model_mismatch+limited chat error surfaces as 409 model_ip_limited
// (never session-invalid, never a session invalidation), directly with no
// failover walk and no unfit mark (Fase E).
func TestChatModelIPLimitedMarked(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatStatus = http.StatusConflict
	mock.ChatErrorBody = limitedChatBody()
	ts, _ := newTestServer(t, nil, mock)
	chatURL := ts.URL + "/v1/chat/completions"

	resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", resp.StatusCode, data)
	}
	if got := errorCode(t, data); got != "model_ip_limited" {
		t.Errorf("code = %q, want model_ip_limited", got)
	}
}

// TestChatModelIPLimitedNoFastRefusal pins the excised fast-refusal guard:
// with no unfit registry every request runs the full path — each limited
// chat surfaces its own 409 model_ip_limited with exactly one upstream
// chat call (no retry, no entry-guard skip).
func TestChatModelIPLimitedNoFastRefusal(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var chatCalls atomic.Int32
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		chatCalls.Add(1)
		writeRawJSON(w, http.StatusConflict, limitedChatBody())
	}
	ts, _ := newTestServer(t, nil, mock)
	chatURL := ts.URL + "/v1/chat/completions"

	// First request: limited error surfaces directly after one chat call.
	resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("first request status = %d, want 409: %s", resp.StatusCode, data)
	}
	if got := chatCalls.Load(); got != 1 {
		t.Errorf("first request upstream chat calls = %d, want 1 (no retry)", got)
	}

	// Second request: no fast-refusal — a second upstream chat hit with
	// the same 409 surface.
	resp2, data2 := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), nil)
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("second request status = %d, want 409: %s", resp2.StatusCode, data2)
	}
	if got := errorCode(t, data2); got != "model_ip_limited" {
		t.Errorf("second request code = %q, want model_ip_limited", got)
	}
	if got := chatCalls.Load(); got != 2 {
		t.Errorf("second request upstream chat calls = %d, want 2 (no entry-guard skip)", got)
	}
}

// TestChatModelIPLimitedRecovery pins the recovery path with no unfit
// registry: a limited 409 surfaces directly, and once upstream serves the
// model again the next request succeeds — no marks to set or clear.
func TestChatModelIPLimitedRecovery(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var chatCalls atomic.Int32
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		// Call 1: the first request sees the limited 409. Call 2+: the
		// upstream serves the model again.
		if chatCalls.Add(1) <= 1 {
			writeRawJSON(w, http.StatusConflict, limitedChatBody())
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-u1", 1, `"choices":[{"index":0,"delta":{"content":"recovered"},"finish_reason":null}]`)))
	}
	ts, _ := newTestServer(t, nil, mock)
	chatURL := ts.URL + "/v1/chat/completions"

	// First request: limited 409 surfaces directly.
	resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("first request status = %d, want 409: %s", resp.StatusCode, data)
	}

	resp2, data2 := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second request status = %d, want 200: %s", resp2.StatusCode, data2)
	}
	if !strings.Contains(string(data2), "recovered") {
		t.Errorf("stream missing recovered content: %s", data2)
	}
}

// TestChatModelIPLimitedAdmissionPath covers the admission-path end-to-end:
// the session create itself returns 409 limited, which surfaces as 409
// model_ip_limited with no unfit mark (Fase E). The session is never
// admitted, so no chat call fires.
func TestChatModelIPLimitedAdmissionPath(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			writeRawJSON(w, http.StatusConflict, limitedChatBody())
			return
		}
		writeRawJSON(w, http.StatusNotFound, `{"error":"not found"}`)
	}
	ts, _ := newTestServer(t, nil, mock)
	chatURL := ts.URL + "/v1/chat/completions"

	resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", resp.StatusCode, data)
	}
	if got := errorCode(t, data); got != "model_ip_limited" {
		t.Errorf("code = %q, want model_ip_limited", got)
	}
}

// TestChatModelIPLimitedConcurrentRefusals pins the direct surface under
// concurrency: with no unfit gate every limited request runs the full path
// and surfaces its own 409 — concurrent refusals never collapse, gate,
// or race each other.
func TestChatModelIPLimitedConcurrentRefusals(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		writeRawJSON(w, http.StatusConflict, limitedChatBody())
	}
	ts, _ := newTestServer(t, nil, mock)
	chatURL := ts.URL + "/v1/chat/completions"
	body := chatBody(modelA)

	const n = 16
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := http.Post(chatURL, "application/json", bytes.NewReader(body))
			if err != nil {
				codes[i] = -1
				return
			}
			defer func() { _ = r.Body.Close() }()
			_, _ = io.Copy(io.Discard, r.Body)
			codes[i] = r.StatusCode
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusConflict {
			t.Errorf("request %d status = %d, want 409", i, c)
		}
	}
}

// TestChatRunFanoutSurfaced429 is the end-to-end contract for the
// free_mode_run_fanout refusal Finn hit as a turn-killing
// 502 upstream_unavailable: the exact observed upstream body must reach the
// client as 429 free_mode_run_fanout, and the mock
// must see exactly ONE chat call — re-POSTing into an anti-fanout refusal is
// what feeds upstream's ban-grade sweep counter.
func TestChatRunFanoutSurfaced429(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var chatCalls atomic.Int32
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		chatCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"free_mode_run_fanout","message":"Free mode request rejected."}`)
	}
	ts, _ := newTestServerCfg(t, nil, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (never the 502 that killed the turn): %s", resp.StatusCode, data)
	}
	// MASQ: no synthesized Retry-After — the refusal carries no upstream
	// window, so none is emitted (honest upstream window only).
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		t.Errorf("Retry-After = %q, want none (no synthesized default)", ra)
	}
	if !strings.Contains(string(data), `"code":"free_mode_run_fanout"`) {
		t.Errorf("body missing free_mode_run_fanout code: %s", data)
	}
	if got := chatCalls.Load(); got != 1 {
		t.Errorf("upstream chat calls = %d, want 1 (never re-POST into a fanout refusal)", got)
	}
}

// turn_spend_limit kills a runaway turn (per-turn spend ceiling, usually a
// stuck agent loop): it must surface as 429 turn_spend_limited with the
// upstream loop warning intact and WITHOUT the daily-quota advisory — and,
// critically, WITHOUT a Retry-After header. Upstream's retryAfterMs on this
// breaker does not clear it (live 2026-09-05: instant re-trips on every 60s
// retry for 20+ minutes), so a Retry-After would hand the harness a futile
// retry drumbeat. Never re-POST into it (same anti-sweep discipline as
// fanout refusals): exactly one upstream chat call, no pool cooldown, so a
// genuinely new turn flows immediately.
func TestChatTurnSpendLimitedSurfaced429(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var chatCalls atomic.Int32
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		chatCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"turn_spend_limit","message":"Something went wrong with this turn.","retryAfterMs":60000}`)
	}
	ts, _ := newTestServerCfg(t, nil, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", resp.StatusCode, data)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		t.Errorf("Retry-After = %q, want none (a Retry-After would loop the poisoned turn)", ra)
	}
	if !strings.Contains(string(data), `"code":"turn_spend_limited"`) {
		t.Errorf("body missing turn_spend_limited code: %s", data)
	}
	if !strings.Contains(string(data), "Something went wrong with this turn") {
		t.Errorf("body missing the upstream loop warning: %s", data)
	}
	if strings.Contains(string(data), "Daily session quota") {
		t.Errorf("body wrongly carries the daily-quota advisory: %s", data)
	}
	if got := chatCalls.Load(); got != 1 {
		t.Errorf("upstream chat calls = %d, want 1 (never re-POST into a turn-spend refusal)", got)
	}
}
