package upstream

import (
	"context"
	"errors"
	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/testutil"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// Issue #744: a connectivity-test chat issued while the model has no serving
// slot gets an upstream 503 ("The model is temporarily unavailable...").
// That 503 must surface as a clean non-fatal waiting-room error, and the
// same-session retry behind it must stay bounded — one budgeted retry behind
// a floored, jittered honor-window sleep — so neither the proxy nor a user
// hammering "retry" turns an outage into an account ban.
func TestWaitingRoom503BoundedAndClean744(t *testing.T) {
	waitingRoom := `{"error":{"message":"The model is temporarily unavailable. Please try again later.","code":503}}`
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	mock := testutil.NewMock()
	defer mock.Close()
	var calls atomic.Int32
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, waitingRoom)
	}
	// A generous transport budget: the waiting-room cap must still hold.
	client, err := New("tok-744", testConfig(mock.URL(), func(c *config.Config) { c.TransientRetries = 3 }))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = client.ChatCompletions(context.Background(), ChatOptions{Model: "m", RunID: "r"}, body)
	if !errors.Is(err, ErrWaitingRoom) {
		t.Fatalf("err = %v, want ErrWaitingRoom (clean non-fatal 503 mapping)", err)
	}
	var wr *WaitingRoomError
	if !errors.As(err, &wr) {
		t.Fatalf("err = %v, want *WaitingRoomError", err)
	}
	if errors.Is(err, ErrBanned) || errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, a waiting-room 503 must never masquerade as a ban or rate limit", err)
	}
	// Attempt cap: 1 initial POST + 1 budgeted same-session retry, even
	// with TRANSIENT_RETRIES=3. Unbounded here is the ban-inducing storm.
	if got := calls.Load(); got != 2 {
		t.Errorf("upstream chat calls = %d, want 2 (original + 1 capped waiting-room retry)", got)
	}
	if got := client.WaitingRoomRetries(); got != 1 {
		t.Errorf("WaitingRoomRetries = %d, want 1", got)
	}
	// Backoff present: the retry waits out the honor window (10s floor),
	// never a hot immediate re-POST.
	if elapsed := time.Since(start); elapsed < 9*time.Second {
		t.Errorf("waiting-room retry elapsed %v, want >= the floored honor window", elapsed)
	}
}

// TestWaitingRoomRetryWaitFlooredAndJittered744 pins the honor-window sleep
// shape: silent/small upstream windows rise to the 10s floor (no hot
// re-POST loop into a slot-less model), generous windows are honored and
// never shortened, and 0-30% jitter rides on top so concurrent proxies do
// not re-POST on a fixed grid. Bounds are by construction — no flakes.
func TestWaitingRoomRetryWaitFlooredAndJittered744(t *testing.T) {
	for range 50 {
		if got := waitingRoomRetryWait(0); got < 10*time.Second || got > 13*time.Second {
			t.Fatalf("waitingRoomRetryWait(0) = %v, want within [10s, 13s]", got)
		}
		if got := waitingRoomRetryWait(time.Second); got < 10*time.Second || got > 13*time.Second {
			t.Fatalf("waitingRoomRetryWait(1s) = %v, want floored to [10s, 13s]", got)
		}
		if got := waitingRoomRetryWait(30 * time.Second); got < 30*time.Second || got > 39*time.Second {
			t.Fatalf("waitingRoomRetryWait(30s) = %v, want honored within [30s, 39s]", got)
		}
	}
}
