package server_test

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
)

// A short-window 429 resolves to 200 in-request: the client never sees it.
func TestChatAutoRetryRateLimitedRecovers(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var chatCalls atomic.Int32
	refuse := atomic.Bool{}
	refuse.Store(true)
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		chatCalls.Add(1)
		if refuse.Load() {
			refuse.Store(false)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":"free_mode_rate_limited","message":"Free mode rate limit exceeded (1 second limit).","retryAfterMs":1000}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-retry1", 1,
			`"choices":[{"index":0,"delta":{"content":"recovered"},"finish_reason":"stop"}]`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServerCfg(t, nil, func(cfg *config.Config) { cfg.ChatAutoRetry = true }, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (429 retried in-request): %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "recovered") {
		t.Errorf("body missing recovered content: %s", data)
	}
	if got := chatCalls.Load(); got != 2 {
		t.Errorf("upstream chat calls = %d, want 2 (refusal + retry)", got)
	}
}

// A 30-minute 429 window is never waited out: it surfaces immediately.
func TestChatAutoRetryLongWindowSurfaced(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var chatCalls atomic.Int32
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		chatCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"free_mode_rate_limited","message":"wait 30 minutes","retryAfterMs":1800000}`)
	}
	ts, _ := newTestServerCfg(t, nil, func(cfg *config.Config) { cfg.ChatAutoRetry = true }, mock)

	resp, _ := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (long window never waited out)", resp.StatusCode)
	}
	if got := chatCalls.Load(); got != 1 {
		t.Errorf("upstream chat calls = %d, want 1 (no retry on 30m window)", got)
	}
}

// Run-invalid rotates to a fresh run in-request: the client gets 200.
func TestChatAutoRetryRunInvalidRecovers(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.RunIDs = []string{"run-0001", "run-0002"}
	var chatCalls atomic.Int32
	refuse := atomic.Bool{}
	refuse.Store(true)
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		chatCalls.Add(1)
		if refuse.Load() {
			refuse.Store(false)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"message":"runId Not Running: run-0001"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-retry2", 1,
			`"choices":[{"index":0,"delta":{"content":"recovered"},"finish_reason":"stop"}]`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServerCfg(t, nil, func(cfg *config.Config) { cfg.ChatAutoRetry = true }, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (run-invalid rotated in-request): %s", resp.StatusCode, data)
	}
	if got := chatCalls.Load(); got != 2 {
		t.Errorf("upstream chat calls = %d, want 2 (dead run + fresh run)", got)
	}
	if got := mock.StartedRunsSnapshot(); len(got) != 2 {
		t.Errorf("agent-run STARTs = %d, want 2 (retry mints fresh): %v", len(got), got)
	}
}

// Waiting-room queue with a short window resolves in-request.
func TestChatAutoRetryWaitingRoomRecovers(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var chatCalls atomic.Int32
	refuse := atomic.Bool{}
	refuse.Store(true)
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		chatCalls.Add(1)
		if refuse.Load() {
			refuse.Store(false)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"message":"The model is temporarily unavailable. Please try again later.","code":503}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-retry3", 1,
			`"choices":[{"index":0,"delta":{"content":"recovered"},"finish_reason":"stop"}]`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServerCfg(t, nil, func(cfg *config.Config) { cfg.ChatAutoRetry = true }, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (waiting-room waited out): %s", resp.StatusCode, data)
	}
	if got := chatCalls.Load(); got != 2 {
		t.Errorf("upstream chat calls = %d, want 2 (queue + retry)", got)
	}
}

// Terminal refusals never retry, even with the knob on.
func TestChatAutoRetryTerminalNoRetry(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var chatCalls atomic.Int32
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		chatCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"GPT-5.6 Luna is no longer available in Freebuff.","type":"invalid_request_error"}}`)
	}
	ts, _ := newTestServerCfg(t, nil, func(cfg *config.Config) { cfg.ChatAutoRetry = true }, mock)

	resp, _ := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("status = 200, want the terminal refusal surfaced (non-200)")
	}
	if got := chatCalls.Load(); got != 1 {
		t.Errorf("upstream chat calls = %d, want 1 (no retry on terminal 400)", got)
	}
}

// A refunded-purchase 409 rejoins fresh in-request: the live 2026-09-28
// shape ("This model purchase was refunded. Start a new session to try
// again." after a waiting-room 503 consumed the hold) holds no competing
// instance, so the invalidated session re-admits and the turn resolves 200.
func TestChatAutoRetryRefundedSupersededRejoins(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var chatCalls atomic.Int32
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		if chatCalls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":"session_superseded","message":"This model purchase was refunded. Start a new session to try again."}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-retry-refund", 1,
			`"choices":[{"index":0,"delta":{"content":"recovered"},"finish_reason":"stop"}]`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServerCfg(t, nil, func(cfg *config.Config) { cfg.ChatAutoRetry = true }, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (refund rejoined in-request): %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "recovered") {
		t.Errorf("body missing recovered content: %s", data)
	}
	if got := chatCalls.Load(); got != 2 {
		t.Errorf("upstream chat calls = %d, want 2 (refund + fresh rejoin)", got)
	}
	if got := mock.SessionCreatesSnapshot(); got != 2 {
		t.Errorf("session creates = %d, want 2 (dead row dropped, fresh admit)", got)
	}
}

// A takeover 409 never rejoins, even with the knob on: stealing the seat
// back risks ping-pong with the live holder.
func TestChatAutoRetryTakeoverSupersededTerminal(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var chatCalls atomic.Int32
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		chatCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":"session_superseded","message":"another CLI took over"}`)
	}
	ts, _ := newTestServerCfg(t, nil, func(cfg *config.Config) { cfg.ChatAutoRetry = true }, mock)

	resp, _ := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (takeover never rejoins)", resp.StatusCode)
	}
	if got := chatCalls.Load(); got != 1 {
		t.Errorf("upstream chat calls = %d, want 1 (no rejoin against the holder)", got)
	}
}

// A stuck queued row resolves in-request: three queue refusals drop the
func TestChatAutoRetryStuckQueueRejoins(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// Three queued turns plus the fresh-admission proof need four runs.
	mock.RunIDs = []string{"run-0001", "run-0002", "run-0003", "run-0004"}
	var chatCalls atomic.Int32
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		if chatCalls.Add(1) <= 3 {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-retry-stuck", 1,
			`"choices":[{"index":0,"delta":{"content":"recovered"},"finish_reason":"stop"}]`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServerCfg(t, nil, func(cfg *config.Config) { cfg.ChatAutoRetry = true }, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (stuck row re-admitted in-request): %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "recovered") {
		t.Errorf("body missing recovered content: %s", data)
	}
	if got := chatCalls.Load(); got != 4 {
		t.Errorf("upstream chat calls = %d, want 4 (3 queues + fresh rejoin)", got)
	}
	if got := mock.SessionCreatesSnapshot(); got != 2 {
		t.Errorf("session creates = %d, want 2 (wedged row dropped, fresh admit)", got)
	}
}
