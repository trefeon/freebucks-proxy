package server_test

import (
	"freebuff-proxy/backend/internal/testutil"
	"io"
	"net/http"
	"strings"
	"testing"
)

// prodFreebucksWindowBody is the freebucks-window refusal observed in
// production on 2026-09-16: the vendor's daily freebucks ceiling. It must be
// distinguishable end to end — a distinct ledger code in /metrics alongside
// the 429 surface.
const prodFreebucksWindowBody = `{"status":"rate_limited","accessTier":"limited","model":"deepseek/deepseek-v4-flash","period":"pacific_day","windowHours":24,"resetAt":"2026-09-17T07:00:00.000Z","retryAfterMs":71766587,"freebucksShortfall":{"price":2,"balance":0,"claimable":0}}`

// TestFreebucksWindowSurfacedInMetricsAndHealthz pins the operator surfaces
// for the prod refusal: the 429 shape on the chat surface and /metrics
// counting it under its own code inside the existing
// rate_limit_events_total family. MASQ writes no cooldown memory, so there
// are no healthz cooldown fields to assert.
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
}
