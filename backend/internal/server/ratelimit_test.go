package server_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/testutil"
)

func TestClientRateLimiterHTTP429(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	// Configure server with RATE_LIMIT_PER_IP=1, RATE_LIMIT_BURST=2
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) {
		c.RateLimitPerIP = 1.0
		c.RateLimitBurst = 2
	}, mock)

	// First 2 requests should be permitted (burst capacity 2)
	for i := range 2 {
		resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d should succeed, got status %d: %s", i+1, resp.StatusCode, data)
		}
	}

	// 3rd request should be rejected with 429 Too Many Requests
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("3rd request should return 429, got status %d: %s", resp.StatusCode, data)
	}

	retryAfterStr := resp.Header.Get("Retry-After")
	if retryAfterStr == "" {
		t.Error("expected Retry-After header on 429 response")
	} else if sec, err := strconv.Atoi(retryAfterStr); err != nil || sec < 1 {
		t.Errorf("expected Retry-After >= 1, got %q", retryAfterStr)
	}

	var errResp struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &errResp); err != nil {
		t.Fatalf("error response is not valid JSON: %v: %s", err, data)
	}
	if errResp.Error.Type != "rate_limit_exceeded" || errResp.Error.Code != "rate_limit_exceeded" {
		t.Errorf("error type/code = (%q, %q), want (rate_limit_exceeded, rate_limit_exceeded)",
			errResp.Error.Type, errResp.Error.Code)
	}
	if !strings.Contains(errResp.Error.Message, "rate limit exceeded") {
		t.Errorf("error message = %q, want rate limit exceeded note", errResp.Error.Message)
	}

	// Check metrics endpoint reflects the rejection
	mResp, mData := doJSON(t, http.MethodGet, ts.URL+"/metrics", nil, nil)
	if mResp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d: %s", mResp.StatusCode, mData)
	}
	metricsBody := string(mData)
	if !strings.Contains(metricsBody, "freebucks_proxy_rate_limit_rejected_total 1") {
		t.Errorf("metrics missing rejected counter = 1; got:\n%s", metricsBody)
	}
}

func TestClientRateLimiterDisabledByDefault(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	// Default config has RateLimitPerIP = 0 (disabled)
	ts, _ := newTestServer(t, nil, mock)

	for i := range 10 {
		resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d should succeed when rate limiter is disabled, got %d: %s", i+1, resp.StatusCode, data)
		}
	}
}
