package server_test

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/logring"
	"freebuff-proxy/backend/internal/testutil"
)

// TestMetricsRateLimitEvents pins T7's metrics surface: a classified 429
// chat renders freebuff_proxy_rate_limit_events_total with the token label.
func TestMetricsRateLimitEvents(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatStatus = http.StatusTooManyRequests
	mock.ChatErrorBody = `{"error":"free_mode_rate_limited","message":"wait 30 minutes","retryAfterMs":1800000}`
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("chat status = %d, want 429: %s", resp.StatusCode, data)
	}

	resp, data = doJSON(t, http.MethodGet, ts.URL+"/metrics", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200: %s", resp.StatusCode, data)
	}
	body := string(data)
	for _, want := range []string{
		"# HELP freebuff_proxy_rate_limit_events_total",
		`freebuff_proxy_rate_limit_events_total{token="1",code="free_mode_rate_limited"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %s in:\n%s", want, body)
		}
	}
}

// TestMetricsRateLimitEventsLabelEscaping mirrors TestMetricsLabelEscaping
// for the code label: quotes in upstream-derived codes are escaped so the
// Prometheus text format stays parseable.
func TestMetricsRateLimitEventsLabelEscaping(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatStatus = http.StatusTooManyRequests
	mock.ChatErrorBody = `{"error":"weird\"code","message":"x"}`
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("chat status = %d, want 429: %s", resp.StatusCode, data)
	}
	resp, data = doJSON(t, http.MethodGet, ts.URL+"/metrics", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200: %s", resp.StatusCode, data)
	}
	body := string(data)
	want := `freebuff_proxy_rate_limit_events_total{token="1",code="weird\"code"} 1`
	if !strings.Contains(body, want) {
		t.Errorf("metrics missing escaped label %s in:\n%s", want, body)
	}
}

// TestMetricsLogEvents pins T20's metrics surface: handled log records
// render freebuff_proxy_log_events_total keyed by level|msg (level
// lowercased), aggregated across the ring.
func TestMetricsLogEvents(t *testing.T) {
	ring := logring.NewHandler(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}), 200)
	logger := slog.New(ring)
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServerWithLogger(t, nil, logger, ring, mock)

	logger.Warn("pool exhausted", "token", "1")
	logger.Warn("pool exhausted")
	logger.Info("request handled")

	resp, data := doJSON(t, http.MethodGet, ts.URL+"/metrics", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200: %s", resp.StatusCode, data)
	}
	body := string(data)
	for _, want := range []string{
		"# HELP freebuff_proxy_log_events_total",
		"# TYPE freebuff_proxy_log_events_total counter",
		`freebuff_proxy_log_events_total{level="warn",msg="pool exhausted"} 2`,
		`freebuff_proxy_log_events_total{level="info",msg="request handled"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %s in:\n%s", want, body)
		}
	}
}

// TestMetricsLogEventsLabelEscaping mirrors TestMetricsLabelEscaping for the
// msg label: quotes in operator log messages are escaped so the Prometheus
// text format stays parseable.
func TestMetricsLogEventsLabelEscaping(t *testing.T) {
	ring := logring.NewHandler(slog.NewTextHandler(io.Discard, nil), 100)
	logger := slog.New(ring)
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServerWithLogger(t, nil, logger, ring, mock)

	logger.Error(`upstream said "no"`)

	resp, data := doJSON(t, http.MethodGet, ts.URL+"/metrics", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200: %s", resp.StatusCode, data)
	}
	want := `freebuff_proxy_log_events_total{level="error",msg="upstream said \"no\""} 1`
	if !strings.Contains(string(data), want) {
		t.Errorf("metrics missing escaped label %s in:\n%s", want, data)
	}
}
