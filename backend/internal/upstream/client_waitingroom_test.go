package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/testutil"
)

// TestChatCompletionsFailsFastOnWaitingRoom: a refused turn fails
// immediately (CLI parity: send-message.ts:575-631 has no 503 arm — the
// turn fails, the 30s poll resyncs, and the next message mints a fresh
// run). A 503 chat refusal must surface the typed error in exactly one
// upstream attempt even with TRANSIENT_RETRIES budget left: the old
// same-session wait-and-retry re-POST burned admissions on refunded rows
// and escalated 503-wait loops into bans.
func TestChatCompletionsFailsFastOnWaitingRoom(t *testing.T) {
	waitingRoom := `{"error":{"message":"The model is temporarily unavailable. Please try again later.","code":503}}`
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	t.Run("503 surfaces ErrWaitingRoom in one attempt", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		var calls atomic.Int32
		mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, waitingRoom)
		}
		// Generous transport budget on purpose: classified gate errors
		// must not consume it.
		client, err := New("tok-a", testConfig(mock.URL(), func(c *config.Config) { c.TransientRetries = 3 }))
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r", SessionInstanceID: "inst-1"}, body)
		if !errors.Is(err, ErrWaitingRoom) {
			t.Fatalf("err = %v, want ErrWaitingRoom", err)
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("upstream chat calls = %d, want 1 (no same-session retry)", got)
		}
		// (Retired: WaitingRoomRetries / CapacityDeferredRetries were the
		// always-0 same-session queue counters — deleted with the snapshot
		// columns. Single-attempt is pinned by the call count above.)
	})

	t.Run("503 Retry-After rides the typed error", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		var calls atomic.Int32
		mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "11")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, waitingRoom)
		}
		client, err := New("tok-b", testConfig(mock.URL(), func(c *config.Config) { c.TransientRetries = 3 }))
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, body)
		if !errors.Is(err, ErrWaitingRoom) {
			t.Fatalf("err = %v, want ErrWaitingRoom", err)
		}
		var wr *WaitingRoomError
		if !errors.As(err, &wr) {
			t.Fatalf("err = %v, want *WaitingRoomError", err)
		}
		if wr.RetryAfter != 11*time.Second {
			t.Errorf("RetryAfter = %v, want 11s (honored by the caller on the next turn, not slept in-request)", wr.RetryAfter)
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("upstream chat calls = %d, want 1 (window is carried, not waited out)", got)
		}
	})

	t.Run("zero budget still surfaces in one attempt", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		var calls atomic.Int32
		mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
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
		if got := calls.Load(); got != 1 {
			t.Errorf("upstream chat calls = %d, want 1 (retries disabled)", got)
		}
	})

	t.Run("429 queued surfaces immediately", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		var calls atomic.Int32
		mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"code":"waiting_room_queued","message":"row caught mid-admit"}}`)
		}
		client, err := New("tok-d", testConfig(mock.URL(), func(c *config.Config) { c.TransientRetries = 3 }))
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, body)
		if !errors.Is(err, ErrWaitingRoom) {
			t.Fatalf("err = %v, want ErrWaitingRoom", err)
		}
		if !isWaitingRoomQueued(err) {
			t.Errorf("isWaitingRoomQueued(err) = false, want true (transient admission race, not general capacity)")
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("upstream chat calls = %d, want 1 (no same-session retry)", got)
		}
	})
}

// TestQueueRetryAfterWindows pins the honor-window extraction: parsed
// Retry-After rides the typed error for the caller to honor on the next
// turn (fail-fast: no in-request sleep), absent means the caller's default.
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
		t.Error("capacity-deferred must NOT classify as waiting room (distinct typed error)")
	}
}
