package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
)

// prodFreebucksWindowBody is the freebucks-window refusal observed in
// production on 2026-09-16: the vendor's daily freebucks ceiling. It must be
// distinguishable end to end — a distinct ledger code in /metrics and the
// three additive cooldown fields on the tokens payload.
const prodFreebucksWindowBody = `{"status":"rate_limited","accessTier":"limited","model":"deepseek/deepseek-v4-flash","period":"pacific_day","windowHours":24,"resetAt":"2026-09-17T07:00:00.000Z","retryAfterMs":71766587,"freebucksShortfall":{"price":2,"balance":0,"claimable":0}}`

// TestFreebucksWindowSurfacedInMetricsAndHealthz pins both operator surfaces
// for the prod refusal: /metrics counts it under its own code inside the
// existing rate_limit_events_total family, and /healthz names the kind, the
// declared window and the reset instant on the cooling token.
func TestFreebucksWindowSurfacedInMetricsAndHealthz(t *testing.T) {
	testutil.UnsetConfigEnv(t)
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, prodFreebucksWindowBody)
	}
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("chat status = %d, want 429: %s", resp.StatusCode, data)
	}

	// /metrics: the same series name and label set as before, with the
	// window refusal counted under its own code.
	resp, data = doJSON(t, http.MethodGet, ts.URL+"/metrics", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200: %s", resp.StatusCode, data)
	}
	metrics := string(data)
	if want := `freebuff_proxy_rate_limit_events_total{token="1",code="freebucks_window"} 1`; !strings.Contains(metrics, want) {
		t.Errorf("metrics missing %s in:\n%s", want, metrics)
	}

	// /healthz: the cooling token states the reason and the lift instant.
	resp, data = doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Tokens []struct {
			CooldownUntil       string `json:"CooldownUntil"`
			CooldownKind        string `json:"cooldown_kind"`
			CooldownResetsAt    string `json:"cooldown_resets_at"`
			CooldownWindowHours int    `json:"cooldown_window_hours"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("healthz is not JSON: %v: %s", err, data)
	}
	if len(out.Tokens) != 1 {
		t.Fatalf("healthz tokens = %d, want 1: %s", len(out.Tokens), data)
	}
	tok := out.Tokens[0]
	if tok.CooldownKind != "freebucks_window" {
		t.Errorf("healthz cooldown_kind = %q, want freebucks_window", tok.CooldownKind)
	}
	if tok.CooldownResetsAt != "2026-09-17T07:00:00Z" {
		t.Errorf("healthz cooldown_resets_at = %q, want 2026-09-17T07:00:00Z", tok.CooldownResetsAt)
	}
	if tok.CooldownWindowHours != 24 {
		t.Errorf("healthz cooldown_window_hours = %d, want 24", tok.CooldownWindowHours)
	}
	if tok.CooldownUntil == "" {
		t.Error("healthz CooldownUntil empty, want the live proxy deadline")
	}
}
