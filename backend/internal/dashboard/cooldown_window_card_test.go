package dashboard_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"freebucks-proxy/backend/internal/upstream"
)

// The dashboard tokens payload is what the SPA renders: a freebucks-window
// cooldown must say WHY and WHEN on both the full fetch and the hot-poll
// view, while a plain cooldown keeps exactly the old keys.

// tokenFromPage fetches a dashboard page and returns tokens[0] as a map.
func tokenFromPage(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var data map[string]any
	if err := json.Unmarshal(mustReadAll(t, resp), &data); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	tokens, _ := data["tokens"].([]any)
	if len(tokens) == 0 {
		t.Fatalf("no tokens in %s response", url)
	}
	token, _ := tokens[0].(map[string]any)
	return token
}

// TestTokensPayloadNamesFreebucksWindowCooldown pins the additive payload
// fields on both views: the full /tokens fetch and the ?view=live hot poll
// the account card refreshes from.
func TestTokensPayloadNamesFreebucksWindowCooldown(t *testing.T) {
	ts, p := pageServer(t, 1, "tokens", nil, nil)
	p.CooldownTokenRateLimit(0, &upstream.RateLimitError{
		Status:             "rate_limited",
		RetryAfter:         30 * time.Minute,
		ResetAt:            time.Date(2026, 9, 17, 7, 0, 0, 0, time.UTC),
		WindowHours:        24,
		FreebucksShortfall: true,
	})

	for _, view := range []string{"/tokens", "/tokens?view=live"} {
		token := tokenFromPage(t, ts.URL+view)
		if token["cooldown_active"] != true {
			t.Errorf("%s cooldown_active = %v, want true", view, token["cooldown_active"])
		}
		if token["cooldown_kind"] != "freebucks_window" {
			t.Errorf("%s cooldown_kind = %v, want freebucks_window", view, token["cooldown_kind"])
		}
		if got, _ := token["cooldown_resets_at"].(string); got != "2026-09-17T07:00:00Z" {
			t.Errorf("%s cooldown_resets_at = %v, want 2026-09-17T07:00:00Z", view, token["cooldown_resets_at"])
		}
		if got, _ := token["cooldown_window_hours"].(float64); got != 24 {
			t.Errorf("%s cooldown_window_hours = %v, want 24", view, token["cooldown_window_hours"])
		}
	}
}

// TestTokensPayloadPlainCooldownKeepsOldShape pins the compatibility half: a
// plain retry-after cooldown adds no new keys at all.
func TestTokensPayloadPlainCooldownKeepsOldShape(t *testing.T) {
	ts, p := pageServer(t, 1, "tokens", nil, nil)
	p.CooldownTokenRateLimit(0, &upstream.RateLimitError{Status: "rate_limited", RetryAfter: 30 * time.Minute})

	for _, view := range []string{"/tokens", "/tokens?view=live"} {
		token := tokenFromPage(t, ts.URL+view)
		if token["cooldown_active"] != true {
			t.Errorf("%s cooldown_active = %v, want true", view, token["cooldown_active"])
		}
		for _, key := range []string{"cooldown_kind", "cooldown_resets_at", "cooldown_window_hours"} {
			if _, present := token[key]; present {
				t.Errorf("%s payload has %s = %v for a plain rate limit, want the key absent", view, key, token[key])
			}
		}
	}
}
