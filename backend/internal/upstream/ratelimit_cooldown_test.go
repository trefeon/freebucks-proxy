package upstream

// Overflow-guard regression tests: upstream-controlled retry fields
// (retryAfterMs, Retry-After, resetAt) are untrusted input and must
// saturate at parse time — before the int64-nanosecond duration multiply
// can wrap — instead of wrapping into a bogus window.

import (
	"errors"
	"math"
	"net/http"
	"testing"
	"time"
)

// saturationDuration is the parse-time overflow saturation point: the
// largest representable duration (classify.go saturates here instead of
// wrapping the ms→ns multiply).
const saturationDuration = time.Duration(math.MaxInt64)

func TestCooldownFromMillis(t *testing.T) {
	cases := []struct {
		name string
		ms   float64
		want time.Duration
	}{
		{"zero", 0, 0},
		{"negative", -5, 0},
		{"60s unchanged", 60000, 60 * time.Second},
		{"huge saturates", 1e15, saturationDuration},
		{"max int64 saturates", float64(1<<63 - 1), saturationDuration},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CooldownFromMillis(tc.ms); got != tc.want {
				t.Errorf("CooldownFromMillis(%v) = %v, want %v", tc.ms, got, tc.want)
			}
		})
	}
}

// TestParseRateLimitClampsCooldown pins the parse-time overflow guard: an
// absurd retryAfterMs must saturate (not wrap positive), a centuries-out
// resetAt must saturate too, and a normal value stays unchanged.
func TestParseRateLimitClampsCooldown(t *testing.T) {
	t.Run("max retryAfterMs clamps", func(t *testing.T) {
		err := parseRateLimit(`{"status":"rate_limited","retryAfterMs":9223372036854775807}`, 0)
		var rle *RateLimitError
		if !errors.As(err, &rle) {
			t.Fatalf("err = %v, want RateLimitError", err)
		}
		if rle.RetryAfter != saturationDuration {
			t.Errorf("RetryAfter = %v, want %v (saturated, not wrapped)", rle.RetryAfter, saturationDuration)
		}
	})
	t.Run("centuries-out resetAt clamps", func(t *testing.T) {
		err := parseRateLimit(`{"status":"rate_limited","resetAt":"2500-01-01T00:00:00Z"}`, 0)
		var rle *RateLimitError
		if !errors.As(err, &rle) {
			t.Fatalf("err = %v, want RateLimitError", err)
		}
		if rle.RetryAfter != saturationDuration {
			t.Errorf("RetryAfter = %v, want %v (resetAt-derived, saturated)", rle.RetryAfter, saturationDuration)
		}
	})
	t.Run("normal 60s unchanged", func(t *testing.T) {
		err := parseRateLimit(`{"status":"rate_limited","retryAfterMs":60000}`, 0)
		var rle *RateLimitError
		if !errors.As(err, &rle) {
			t.Fatalf("err = %v, want RateLimitError", err)
		}
		if rle.RetryAfter != 60*time.Second {
			t.Errorf("RetryAfter = %v, want 60s", rle.RetryAfter)
		}
	})
}

// TestParseIpCappedClampsCooldown pins the same saturation guard on the
// ip_capped parse path.
func TestParseIpCappedClampsCooldown(t *testing.T) {
	err := parseIpCapped(`{"status":"ip_capped","retryAfterMs":9223372036854775807}`, 0)
	var ice *IpCappedError
	if !errors.As(err, &ice) {
		t.Fatalf("err = %v, want IpCappedError", err)
	}
	if ice.RetryAfter != saturationDuration {
		t.Errorf("RetryAfter = %v, want %v (saturated, not wrapped)", ice.RetryAfter, saturationDuration)
	}

	err = parseIpCapped(`{"status":"ip_capped","retryAfterMs":60000}`, 0)
	if !errors.As(err, &ice) {
		t.Fatalf("err = %v, want IpCappedError", err)
	}
	if ice.RetryAfter != 60*time.Second {
		t.Errorf("RetryAfter = %v, want 60s", ice.RetryAfter)
	}
}

// max-int32 seconds header (68 years) passes through verbatim (it fits the
// int64-nanosecond range, so there is nothing to saturate) while a
// centuries-out HTTP date saturates; a normal value stays unchanged.
func TestParseRetryAfterClampsCooldown(t *testing.T) {
	t.Run("max int32 seconds verbatim", func(t *testing.T) {
		hdr := http.Header{"Retry-After": {"2147483647"}}
		if got, want := parseRetryAfter(hdr), time.Duration(2147483647)*time.Second; got != want {
			t.Errorf("parseRetryAfter(2147483647) = %v, want %v (verbatim, fits int64)", got, want)
		}
	})
	t.Run("centuries-out http date clamps", func(t *testing.T) {
		hdr := http.Header{"Retry-After": {"Sat, 01 Jan 2500 00:00:00 GMT"}}
		if got := parseRetryAfter(hdr); got != saturationDuration {
			t.Errorf("parseRetryAfter(year 2500 date) = %v, want %v", got, saturationDuration)
		}
	})
	t.Run("normal 60s unchanged", func(t *testing.T) {
		hdr := http.Header{"Retry-After": {"60"}}
		if got := parseRetryAfter(hdr); got != 60*time.Second {
			t.Errorf("parseRetryAfter(60) = %v, want 60s", got)
		}
	})
}

// TestClassifyLegacyLunaAgent verifies that a 502 body containing
// free_mode_legacy_luna_agent or free_mode_legacy_luna is classified as
// ErrSessionInvalid (session needs refresh).
func TestClassifyLegacyLunaAgent(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"legacy_luna_agent", `{"error":"free_mode_legacy_luna_agent","message":"session stale"}`},
		{"legacy_luna", `{"error":"free_mode_legacy_luna","message":"upgrade required"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyError(502, tc.body, http.Header{})
			if !errors.Is(err, ErrSessionInvalid) {
				t.Errorf("classifyError(502, %s) = %v, want ErrSessionInvalid", tc.name, err)
			}
		})
	}
}

// TestParseRateLimit30MinutesText verifies that a body containing
// "30 minutes limit" (without JSON retryAfterMs) yields RetryAfter ≈ 30m.
func TestParseRateLimit30MinutesText(t *testing.T) {
	body := `{"error":"free_mode_rate_limited","message":"You've hit the 30 minutes limit."}`
	err := parseRateLimit(body, 0)
	var rle *RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("err = %v, want RateLimitError", err)
	}
	if rle.RetryAfter != 30*time.Minute {
		t.Errorf("RetryAfter = %v, want 30m", rle.RetryAfter)
	}
}

// TestParseRateLimitResetAtText verifies that a body containing
// "reset at <ISO>" (without JSON resetAt field) yields the parsed ResetAt.
func TestParseRateLimitResetAtText(t *testing.T) {
	body := `{"error":"session_quota_exhausted","message":"Daily quota exceeded. Resets at 2032-08-22T07:00:00Z."}`
	err := parseRateLimit(body, 0)
	var rle *RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("err = %v, want RateLimitError", err)
	}
	want, _ := time.Parse(time.RFC3339, "2032-08-22T07:00:00Z")
	if !rle.ResetAt.Equal(want) {
		t.Errorf("ResetAt = %v, want %v", rle.ResetAt, want)
	}
	// When the reset time is in the future, RetryAfter should be derived from it.
	if rle.RetryAfter <= 0 {
		t.Errorf("RetryAfter = %v, want positive (derived from ResetAt)", rle.RetryAfter)
	}
}
