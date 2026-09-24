package server_test

import (
	"net/http"
	"strings"
	"testing"

	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/testutil"
)

// TestChatWaitingRoomRetriedSameSession: a 503 waiting-room chat refusal
// must be waited out in-request (same session, honor-window sleep) instead
// of an instant 503 — CLI parity
// (upstream/freebuff cli/src/utils/freebuff-session-api.ts:48-72,
// send-message.ts:610-619). The mock 503s once with no Retry-After (the
// live shape) then serves: the chat must return 200 with no
// waiting_room_queued body.
func TestChatWaitingRoomRetriedSameSession(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	calls := 0
	waitingRoom := `{"error":{"message":"The model is temporarily unavailable. Please try again later.","code":503}}`
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(waitingRoom))
			return
		}
		if mock.SessionHandler != nil {
			mock.SessionHandler(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + chunk("cmpl-test", 1234567890, `"choices":[{"delta":{"content":"ok"},"index":0}]`) + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}
	// The shared test stack zero-values TRANSIENT_RETRIES (retries off),
	// so opt into the one budgeted same-session retry explicitly.
	ts, _ := newTestServerCfg(t, nil, func(cfg *config.Config) { cfg.TransientRetries = 1 }, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (queued-then-served): %s", resp.StatusCode, data)
	}
	if strings.Contains(string(data), "waiting_room_queued") {
		t.Errorf("body carries waiting_room_queued on the served path: %s", data)
	}
	if calls != 2 {
		t.Errorf("upstream chat calls = %d, want 2 (original + same-session retry)", calls)
	}
}

// TestChatWaitingRoomExpiredStill503: with TRANSIENT_RETRIES=0 (exhausted
// budget) a persistent 503 must still surface as 503 waiting_room_queued
// with a Retry-After honor window (10s floor when upstream sends none) —
// never a bare retry-now 503.
func TestChatWaitingRoomExpiredStill503(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatStatus = http.StatusServiceUnavailable
	mock.ChatErrorBody = `{"error":{"message":"The model is temporarily unavailable. Please try again later.","code":503}}`
	ts, _ := newTestServerCfg(t, nil, func(cfg *config.Config) { cfg.TransientRetries = 0 }, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "waiting_room_queued") {
		t.Errorf("body missing waiting_room_queued: %s", data)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "10" {
		t.Errorf("Retry-After = %q, want 10 (floor when upstream sends no window)", ra)
	}
}
