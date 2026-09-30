package server_test

// Default-config refunded-purchase recovery. The 409 session_superseded
// refund wording ("This model purchase was refunded. Start a new session to
// try again.") is the shape a waiting-room 503 leaves behind: the purchase is
// gone and NO competing instance holds the seat, so one fresh re-admit always
// resolves the turn. That recovery must not require CHAT_AUTO_RETRY (whose
// default-off fail-fast policy exists for the takeover wording, which does
// have a live holder).

import (
	"freebuff-proxy/backend/internal/testutil"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRefundedSupersededRejoinsByDefault(t *testing.T) {
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
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-refund-default", 1,
			`"choices":[{"index":0,"delta":{"content":"recovered"},"finish_reason":"stop"}]`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	// Default config: CHAT_AUTO_RETRY is false.
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (refund rejoined without CHAT_AUTO_RETRY): %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "recovered") {
		t.Errorf("body missing recovered content: %s", data)
	}
	if got := chatCalls.Load(); got != 2 {
		t.Errorf("upstream chat calls = %d, want 2 (refund + one fresh rejoin)", got)
	}
	if got := mock.SessionCreatesSnapshot(); got != 2 {
		t.Errorf("session creates = %d, want 2 (dead row dropped, fresh admit)", got)
	}
}

func TestTakeoverSupersededTerminalByDefault(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var chatCalls atomic.Int32
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		chatCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":"session_superseded","message":"another CLI took over"}`)
	}
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (a live holder is never rejoined): %s", resp.StatusCode, data)
	}
	if got := chatCalls.Load(); got != 1 {
		t.Errorf("upstream chat calls = %d, want 1 (no rejoin against the holder)", got)
	}
}

// A refund that never clears must stay bounded: exactly one extra attempt,
// then the honest 409 surfaces — never a loop.
func TestRefundedSupersededRetryBounded(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var chatCalls atomic.Int32
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		chatCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":"session_superseded","message":"This model purchase was refunded. Start a new session to try again."}`)
	}
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 after the bounded retry: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "session_superseded") {
		t.Errorf("body missing session_superseded: %s", data)
	}
	if got := chatCalls.Load(); got != 2 {
		t.Errorf("upstream chat calls = %d, want exactly 2 (one bounded retry, no loop)", got)
	}
}
