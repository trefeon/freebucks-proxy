package upstream

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/logring"
	"freebucks-proxy/backend/internal/testutil"
)

// The freebucks-window refusal is the vendor's daily freebucks ceiling, live
// 2026-09-16: a 429 whose body carries status "rate_limited", accessTier
// "limited", windowHours 24, resetAt 2026-09-17T07:00:00Z, retryAfterMs
// 71766587 (~19h56m) and a freebucksShortfall block. Today it is
// indistinguishable from a plain rate limit in the ledger and the payload.
// These tests pin the distinction: the body's freebucks shortfall marks the
// kind, the declared window and reset instant are kept on the typed error,
// and a plain rate limit keeps today's exact code, window and cooldown.

// prodFreebucksWindowBody is the refusal shape observed in production on
// 2026-09-16 (fields verbatim, message dropped).
const prodFreebucksWindowBody = `{"status":"rate_limited","accessTier":"limited","model":"deepseek/deepseek-v4-flash","period":"pacific_day","windowHours":24,"resetAt":"2026-09-17T07:00:00.000Z","retryAfterMs":71766587,"freebucksShortfall":{"price":2,"balance":0,"claimable":0}}`

// plainRateLimitBody is a per-model quota refusal with no freebucks marker:
// the shape that must stay exactly as it is today.
const plainRateLimitBody = `{"status":"rate_limited","limit":3,"recentCount":3,"retryAfterMs":48549499}`

// TestParseRateLimitFreebucksWindowKind pins the parsed window evidence on the
// typed error: the kind, the shortfall marker, the declared window length and
// upstream's reset instant — alongside the unchanged RetryAfter (retryAfterMs
// stays preferred) and window-table value.
func TestParseRateLimitFreebucksWindowKind(t *testing.T) {
	err := parseRateLimit(prodFreebucksWindowBody, 0)
	var rle *RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("err = %v, want *RateLimitError", err)
	}
	if got := rle.WindowKind(); got != WindowKindFreebucks {
		t.Errorf("WindowKind() = %q, want %q", got, WindowKindFreebucks)
	}
	if !rle.FreebucksShortfall {
		t.Error("FreebucksShortfall = false, want true (body carried freebucksShortfall)")
	}
	if rle.WindowHours != 24 {
		t.Errorf("WindowHours = %d, want 24", rle.WindowHours)
	}
	if want := time.Date(2026, 9, 17, 7, 0, 0, 0, time.UTC); !rle.ResetAt.Equal(want) {
		t.Errorf("ResetAt = %v, want %v", rle.ResetAt, want)
	}
	if want := CooldownFromMillis(71766587); rle.RetryAfter != want {
		t.Errorf("RetryAfter = %v, want %v (retryAfterMs preferred, unchanged)", rle.RetryAfter, want)
	}
	if rle.Window != "reset" {
		t.Errorf("Window = %q, want reset (window table unchanged)", rle.Window)
	}
}

// TestParseRateLimitPlainBodyHasNoWindowKind pins backward compatibility: a
// plain per-model refusal carries no kind, no window length and no cooldown
// change at all.
func TestParseRateLimitPlainBodyHasNoWindowKind(t *testing.T) {
	err := parseRateLimit(plainRateLimitBody, 0)
	var rle *RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("err = %v, want *RateLimitError", err)
	}
	if got := rle.WindowKind(); got != "" {
		t.Errorf("WindowKind() = %q, want empty for a plain rate limit", got)
	}
	if rle.FreebucksShortfall {
		t.Error("FreebucksShortfall = true, want false")
	}
	if rle.WindowHours != 0 {
		t.Errorf("WindowHours = %d, want 0", rle.WindowHours)
	}
	if want := CooldownFromMillis(48549499); rle.RetryAfter != want {
		t.Errorf("RetryAfter = %v, want %v", rle.RetryAfter, want)
	}
	if rle.Window != "retry-after" {
		t.Errorf("Window = %q, want retry-after", rle.Window)
	}
}

// TestParseRateLimitWindowHoursAloneStaysPlain pins the boundary: a declared
// window is not by itself the freebucks ceiling (the per-model daily quota
// refusal carries windowHours too). The window length is still reported, but
// the refusal keeps the plain code.
func TestParseRateLimitWindowHoursAloneStaysPlain(t *testing.T) {
	err := parseRateLimit(`{"status":"rate_limited","windowHours":24,"resetAt":"2026-09-17T07:00:00.000Z","retryAfterMs":60000}`, 0)
	var rle *RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("err = %v, want *RateLimitError", err)
	}
	if got := rle.WindowKind(); got != "" {
		t.Errorf("WindowKind() = %q, want empty (windowHours alone is not the freebucks ceiling)", got)
	}
	if rle.WindowHours != 24 {
		t.Errorf("WindowHours = %d, want 24 (declared window still reported)", rle.WindowHours)
	}
}

// TestParseRateLimitFreebucksShortfallSnakeCase pins the snake_case spelling
// of both new fields, mirroring the parser's existing dual-casing convention
// (retryAfterMs/retry_after_ms, resetAt/reset_at).
func TestParseRateLimitFreebucksShortfallSnakeCase(t *testing.T) {
	err := parseRateLimit(`{"status":"rate_limited","window_hours":24,"freebucks_shortfall":{"price":1}}`, 0)
	var rle *RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("err = %v, want *RateLimitError", err)
	}
	if got := rle.WindowKind(); got != WindowKindFreebucks {
		t.Errorf("WindowKind() = %q, want %q", got, WindowKindFreebucks)
	}
	if rle.WindowHours != 24 {
		t.Errorf("WindowHours = %d, want 24", rle.WindowHours)
	}
}

// TestFreebucksWindowLedgerCodeIsDistinct pins the counter split: the window
// refusal lands under its own ledger code, while a plain refusal in the same
// window keeps counting under rate_limited (no silent rename).
func TestFreebucksWindowLedgerCodeIsDistinct(t *testing.T) {
	testutil.UnsetConfigEnv(t)
	client, err := New("tok-0", &config.Config{UpstreamBaseURL: "http://127.0.0.1:1", CostMode: "free"})
	if err != nil {
		t.Fatal(err)
	}
	_ = client.classify(http.StatusTooManyRequests, prodFreebucksWindowBody, http.Header{})
	_ = client.classify(http.StatusTooManyRequests, plainRateLimitBody, http.Header{})

	events := client.RateLimitEvents()
	if got := events[WindowKindFreebucks]; got != 1 {
		t.Errorf("events[%s] = %d, want 1 (all: %v)", WindowKindFreebucks, got, events)
	}
	if got := events["rate_limited"]; got != 1 {
		t.Errorf("events[rate_limited] = %d, want 1 — the plain refusal only (all: %v)", got, events)
	}
}

// TestFreebucksWindowClassificationLogLine pins the operator-facing line: the
// existing classification record now names the kind, the declared window and
// the reset instant next to the code.
func TestFreebucksWindowClassificationLogLine(t *testing.T) {
	testutil.UnsetConfigEnv(t)
	client, err := New("tok-0", &config.Config{UpstreamBaseURL: "http://127.0.0.1:1", CostMode: "free"})
	if err != nil {
		t.Fatal(err)
	}
	ring := logring.NewHandler(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}), 200)
	orig := slog.Default()
	slog.SetDefault(slog.New(ring))
	t.Cleanup(func() { slog.SetDefault(orig) })

	_ = client.classify(http.StatusTooManyRequests, prodFreebucksWindowBody, http.Header{})

	fields := entryFields(ring.Recent(100), "upstream rate limit classified")
	if fields == nil {
		t.Fatalf("no `upstream rate limit classified` line captured")
	}
	joined := strings.Join(fields, " ")
	for _, want := range []string{
		"code=" + WindowKindFreebucks,
		"kind=" + WindowKindFreebucks,
		"window_hours=24",
		"window=reset",
		"retry_after=71766",
		"reset_at=2026-09-17T07:00:00Z",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("`upstream rate limit classified` missing %q in %s", want, joined)
		}
	}
}
