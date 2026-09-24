package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/testutil"
)

// TestChatCompletionsRetriesWaitingRoomSameSession: the proxy must wait out
// the upstream waiting room in-request (CLI parity:
// cli/src/utils/freebuff-session-api.ts:48-72 keeps polling the session on
// 503 and send-message.ts:610-619 treats waiting_room_queued as "we'll
// wait") instead of surfacing an instant 503. A 503 chat refusal whose
// Retry-After elapses to a 200 must return the stream with no error and cost
// exactly one budgeted retry.
func TestChatCompletionsRetriesWaitingRoomSameSession(t *testing.T) {
	waitingRoom := `{"error":{"message":"The model is temporarily unavailable. Please try again later.","code":503}}`
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	t.Run("503 then 200 returns stream", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		calls := 0
		mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls == 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, waitingRoom)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			_, _ = io.WriteString(w, testutil.SSEEvent(`{"id":"x","object":"chat.completion.chunk","choices":[]}`))
		}
		client, err := New("tok-a", testConfig(mock.URL(), func(c *config.Config) { c.TransientRetries = 1 }))
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		rc, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r", SessionInstanceID: "inst-1"}, body)
		if err != nil {
			t.Fatalf("ChatCompletions after waiting-room retry: %v", err)
		}
		_ = rc.Close()
		// No upstream window: the floor (10s) must have been honored before
		// the retry POST, not an immediate re-POST.
		if elapsed := time.Since(start); elapsed < 9*time.Second {
			t.Errorf("waiting-room retry elapsed %v, want >= the 10s default wait", elapsed)
		}
		if calls != 2 {
			t.Errorf("upstream chat calls = %d, want 2 (original + same-session retry)", calls)
		}
		if got := client.WaitingRoomRetries(); got != 1 {
			t.Errorf("WaitingRoomRetries = %d, want 1", got)
		}
		if got := client.CapacityDeferredRetries(); got != 0 {
			t.Errorf("CapacityDeferredRetries = %d, want 0 (waiting room must not pollute the capacity counter)", got)
		}
		if len(mock.RecordedChatBodies) != 2 {
			t.Fatalf("recorded %d chat requests, want 2", len(mock.RecordedChatBodies))
		}
		if mock.RecordedChatBodies[0] != mock.RecordedChatBodies[1] {
			t.Error("retried body differs from original (must be byte-identical)")
		}
	})

	t.Run("503 Retry-After honored", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		calls := 0
		mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "11")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, waitingRoom)
		}
		client, err := New("tok-b", testConfig(mock.URL(), func(c *config.Config) { c.TransientRetries = 1 }))
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		_, err = client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, body)
		if !errors.Is(err, ErrWaitingRoom) {
			t.Fatalf("err = %v, want ErrWaitingRoom", err)
		}
		if elapsed := time.Since(start); elapsed < 10*time.Second {
			t.Errorf("budget-exhausted elapsed %v, want >= the 11s upstream window (1 queued sleep)", elapsed)
		}
		if calls != 2 {
			t.Errorf("upstream chat calls = %d, want 2 (original + 1 budgeted retry)", calls)
		}
	})

	t.Run("zero budget never retries", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		calls := 0
		mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, waitingRoom)
		}
		client, err := New("tok-c", testConfig(mock.URL(), func(c *config.Config) { c.TransientRetries = 0 }))
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, body)
		if !errors.Is(err, ErrWaitingRoom) {
			t.Fatalf("err = %v, want ErrWaitingRoom", err)
		}
		if calls != 1 {
			t.Errorf("upstream chat calls = %d, want 1 (retries disabled)", calls)
		}
	})

	t.Run("429 queued then 200 returns stream", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		calls := 0
		mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls == 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"error":{"code":"waiting_room_queued","message":"row caught mid-admit"}}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			_, _ = io.WriteString(w, testutil.SSEEvent(`{"id":"x","object":"chat.completion.chunk","choices":[]}`))
		}
		client, err := New("tok-d", testConfig(mock.URL(), func(c *config.Config) { c.TransientRetries = 1 }))
		if err != nil {
			t.Fatal(err)
		}
		rc, err := client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, body)
		if err != nil {
			t.Fatalf("ChatCompletions after queued retry: %v", err)
		}
		_ = rc.Close()
		if calls != 2 {
			t.Errorf("upstream chat calls = %d, want 2 (original + same-session retry)", calls)
		}
		if got := client.WaitingRoomRetries(); got != 1 {
			t.Errorf("WaitingRoomRetries = %d, want 1", got)
		}
	})
}

// TestQueueRetryAfterWindows pins the honor-window extraction: parsed
// Retry-After rides to the sleep, absent means the caller's 10s default.
func TestQueueRetryAfterWindows(t *testing.T) {
	mk := func(status int, body string, retryAfter string) error {
		hdr := http.Header{}
		if retryAfter != "" {
			hdr.Set("Retry-After", retryAfter)
		}
		return classifyError(status, body, hdr)
	}
	if got := queueRetryAfter(mk(503, `{"error":{"message":"busy","code":503}}`, "")); got != 0 {
		t.Errorf("bare 503 window = %v, want 0 (caller floors to 10s)", got)
	}
	if got := queueRetryAfter(mk(503, `{"error":{"message":"busy","code":503}}`, "7")); got != 7*time.Second {
		t.Errorf("503 window = %v, want 7s", got)
	}
	def := mk(429, `{"error":{"code":"free_mode_capacity_deferred","message":"at capacity"}}`, "5")
	if got := queueRetryAfter(def); got != 5*time.Second {
		t.Errorf("deferred window = %v, want 5s", got)
	}
	if !isWaitingRoom(mk(503, `x`, "")) {
		t.Error("bare 503 must classify as waiting room")
	}
	if !isWaitingRoom(mk(429, `{"error":{"code":"waiting_room_queued"}}`, "")) {
		t.Error("429 queued must classify as waiting room")
	}
	if isWaitingRoom(mk(429, `{"error":{"code":"free_mode_capacity_deferred"}}`, "")) {
		t.Error("capacity-deferred must NOT classify as waiting room (own counter)")
	}
}
