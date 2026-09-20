package server

// The logs rollup/export/import surface sits behind the sensitive guard
// (same tier as logs/history): open-mode remote clients get 403,
// token-mode unauthenticated remote clients redirect to login, and
// loopback stays served. The test server carries no history store, so the
// loopback probes assert reachability (200 with enabled:false), not data.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"freebucks-proxy/backend/internal/config"
)

func rollupRemoteReq(method, path string) *http.Request {
	var req *http.Request
	if method == http.MethodPost {
		req = httptest.NewRequest(method, path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.RemoteAddr = "203.0.113.5:4444" // TEST-NET-3: never a loopback peer
	req.Host = "dashboard.example.com"
	return req
}

func rollupLoopReq(method, path string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = "127.0.0.1:5111"
	req.Host = "127.0.0.1:3457"
	return req
}

func TestLogsRollupSensitiveGuard(t *testing.T) {
	open := newReviewFixServer(t, "AUTH_TOKENS=tok-0\n", func(c *config.Config) { c.AdminToken = "" })
	h := open.Handler()

	// Open-mode remote: all three refuse with 403 (the POST carries no
	// CSRF pair, but the outer auth gate fires first).
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/admin/api/logs/rollup"},
		{http.MethodGet, "/admin/api/logs/export"},
		{http.MethodPost, "/admin/api/logs/import"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, rollupRemoteReq(tc.method, tc.path))
		if rec.Code != http.StatusForbidden {
			t.Errorf("open-mode remote %s %s status = %d, want 403", tc.method, tc.path, rec.Code)
		}
	}

	// Open-mode loopback: served (no history store on the test server, so
	// enabled:false — reachability, not data, is the assertion).
	for _, path := range []string{"/admin/api/logs/rollup", "/admin/api/logs/export"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, rollupLoopReq(http.MethodGet, path))
		if rec.Code != http.StatusOK {
			t.Errorf("open-mode loopback %s status = %d, want 200", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"enabled":false`) {
			t.Errorf("open-mode loopback %s body lacks enabled:false: %s", path, rec.Body.String())
		}
	}

	// Token-mode remote unauthenticated: redirected to login, not served.
	secured := newReviewFixServer(t, "AUTH_TOKENS=tok-0\nADMIN_TOKEN=secret123\n", nil)
	sh := secured.Handler()
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/admin/api/logs/rollup"},
		{http.MethodGet, "/admin/api/logs/export"},
		{http.MethodPost, "/admin/api/logs/import"},
	} {
		rec := httptest.NewRecorder()
		sh.ServeHTTP(rec, rollupRemoteReq(tc.method, tc.path))
		if rec.Code != http.StatusFound {
			t.Errorf("token-mode remote %s %s status = %d, want 302", tc.method, tc.path, rec.Code)
		}
	}
}
