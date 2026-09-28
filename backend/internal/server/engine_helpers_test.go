package server

// chatErrClass bucket pins: the trace error column is the dashboard's only
// signal when the downstream access line keeps its 200 default (client gone
// before anything was written), so a canceled client must not collapse into
// the generic "error" bucket.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"freebucks-proxy/backend/internal/upstream"
)

func TestChatErrClass(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"canceled", context.Canceled, "client_canceled"},
		{"wrapped canceled", fmt.Errorf("chat attempt: %w", context.Canceled), "client_canceled"},
		{"turn spend limited", &upstream.TurnSpendLimitError{Status: http.StatusTooManyRequests, Body: "x"}, "turn_spend_limited"},
		{"superseded", &upstream.SessionSupersededError{Status: http.StatusConflict}, "session_superseded"},
		{"generic", errors.New("boom"), "error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := chatErrClass(tc.err); got != tc.want {
				t.Errorf("chatErrClass(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestSystemCacheMarkerRoundTrip pins the fallback-preserve pair: detect a
// stamped system marker, and re-stamping is idempotent. Bodies without a
// system message (or with a non-system first message) report absent and
// pass through untouched.
func TestSystemCacheMarkerRoundTrip(t *testing.T) {
	withSys := []byte(`{"model":"m","messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"}]}`)
	if systemCacheMarker(withSys) {
		t.Error("unstamped body reports marker present")
	}
	stamped := stampOpenAISystemCacheMarker(withSys)
	if !systemCacheMarker(stamped) {
		t.Fatal("stamped body reports marker absent")
	}
	restamped := stampOpenAISystemCacheMarker(stamped)
	if string(restamped) != string(stamped) {
		t.Error("re-stamp not idempotent")
	}
	noSys := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if systemCacheMarker(noSys) {
		t.Error("user-first body reports marker present")
	}
	if got := string(stampOpenAISystemCacheMarker(noSys)); got != string(noSys) {
		t.Error("user-first body modified by stamp")
	}
	if got := string(stampOpenAISystemCacheMarker([]byte(`{broken`))); got != `{broken` {
		t.Error("invalid JSON not passed through")
	}
}
