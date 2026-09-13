package pool

// Acquire-time rate-limit token attribution: when every token is cooling,
// the surfaced 429 must name the binding token (shortest retry window,
// the bestRateLimit rule) plus the full limited set, without changing
// selection semantics or the errors.Is/As surface the server classifies on.

import (
	"context"
	"errors"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

// All tokens cooling on distinct windows: the wrapper names the
// shortest-window token and enumerates the whole limited set, while the
// sentinel and typed error still traverse the wrapper.
func TestAcquireRateLimitedCarriesBindingToken(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newTestPool(t, mock0, mock1)

	p.CooldownTokenRateLimit(0, &upstream.RateLimitError{Body: "rate limit", RetryAfter: 30 * time.Minute})
	p.CooldownTokenRateLimit(1, &upstream.RateLimitError{Body: "rate limit", RetryAfter: 5 * time.Minute})

	_, err := p.Acquire(context.Background(), modelA)
	if err == nil || !errors.Is(err, upstream.ErrRateLimited) {
		t.Fatalf("Acquire all-cooling = %v, want rate limit error", err)
	}
	var are *AcquireRateLimitedError
	if !errors.As(err, &are) {
		t.Fatalf("Acquire err = %T (%v), want *AcquireRateLimitedError", err, err)
	}
	if are.Token != 1 {
		t.Errorf("binding token = %d, want 1 (shortest 5m window)", are.Token)
	}
	if len(are.LimitedTokens) != 2 || are.LimitedTokens[0] != 0 || are.LimitedTokens[1] != 1 {
		t.Errorf("limited set = %v, want [0 1]", are.LimitedTokens)
	}
	var rle *upstream.RateLimitError
	if !errors.As(err, &rle) || rle == nil {
		t.Errorf("typed RateLimitError no longer traverses the wrapper: %v", err)
	}
	if err.Error() != rle.Error() {
		t.Errorf("wrapper message = %q, want binding refusal %q", err.Error(), rle.Error())
	}
}

// Identical windows on both tokens: selection still picks one binding
// token, but the limited set must keep BOTH — the old error-string dedup
// collapsed them and the trace could not enumerate the pool.
func TestAcquireRateLimitedKeepsDuplicateWindowTokens(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newTestPool(t, mock0, mock1)

	p.CooldownTokenRateLimit(0, &upstream.RateLimitError{Body: "rate limit", RetryAfter: 10 * time.Minute})
	p.CooldownTokenRateLimit(1, &upstream.RateLimitError{Body: "rate limit", RetryAfter: 10 * time.Minute})

	_, err := p.Acquire(context.Background(), modelA)
	var are *AcquireRateLimitedError
	if !errors.As(err, &are) {
		t.Fatalf("Acquire err = %T (%v), want *AcquireRateLimitedError", err, err)
	}
	if len(are.LimitedTokens) != 2 {
		t.Errorf("limited set = %v, want both tokens despite identical windows", are.LimitedTokens)
	}
}
