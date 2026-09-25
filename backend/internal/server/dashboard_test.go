package server_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/dashboard"
	"freebucks-proxy/backend/internal/pool"
	"freebucks-proxy/backend/internal/registry"
	"freebucks-proxy/backend/internal/server"
	"freebucks-proxy/backend/internal/session"
	"freebucks-proxy/backend/internal/store"
	"freebucks-proxy/backend/internal/testutil"
	"freebucks-proxy/backend/internal/upstream"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// dashboardURL returns the base URL of a test server with AdminToken set.
func dashboardServer(t *testing.T, adminToken string, mut func(*config.Config)) *httptest.Server {
	t.Helper()
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) {
		c.AdminToken = adminToken
		if mut != nil {
			mut(c)
		}
	}, testutil.NewMock())
	return ts
}

// noRedirectClient never follows redirects, so tests can assert on them.
func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func get(t *testing.T, url, cookie string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func postLogin(t *testing.T, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader("token="+token))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func bodyOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The cookie's Secure flag adapts to the connection protocol dynamically:
// on plain HTTP (common in self-hosted cloud VPS environments), Secure is false
// so browsers accept the cookie without silent drops; when behind an HTTPS
// reverse proxy (X-Forwarded-Proto: https) or direct TLS, Secure is true.
func TestDashboardCookieDynamicProtocol(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)

	// 1. Plain HTTP login -> Secure is false (works out-of-the-box on VPS).
	plain := postLogin(t, ts.URL+"/admin/login", "secret")
	defer func() { _ = plain.Body.Close() }()
	var admin *http.Cookie
	for _, cc := range plain.Cookies() {
		if cc.Name == "fb_admin" {
			admin = cc
			break
		}
	}
	if admin == nil {
		t.Fatal("plain-HTTP login did not set fb_admin")
		return
	}
	if admin.Secure {
		t.Error("plain-HTTP login must set Secure=false for zero-friction self-hosted VPS login")
	}
}

// The dashboard is open when ADMIN_TOKEN is unset (legacy behavior).
func TestDashboardOpenWithoutAdminToken(t *testing.T) {
	ts := dashboardServer(t, "", nil)
	resp := get(t, ts.URL+"/admin", "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (open dashboard)", resp.StatusCode)
	}
	body := bodyOf(t, resp)
	if !strings.Contains(body, "freebucks-proxy") && !strings.Contains(body, "admin") {
		t.Error("dashboard page missing SPA content")
	}
}

// Without a session cookie the dashboard redirects to the login page.
func TestDashboardRedirectsToLogin(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	resp := get(t, ts.URL+"/admin", "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/admin/login" {
		t.Fatalf("redirect location = %q, want /admin/login", loc)
	}
}

// Login flow: wrong token rejected, right token issues a cookie that unlocks
// the dashboard, and the cookie is HttpOnly + SameSite=Strict.
func TestDashboardLoginFlow(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)

	// Wrong token: 401 with JSON error, no cookie set.
	resp := postLogin(t, ts.URL+"/admin/login", "wrong")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong-token status = %d, want 401", resp.StatusCode)
	}
	body := bodyOf(t, resp)
	if !strings.Contains(body, "Invalid password") {
		t.Error("wrong-token response missing error message")
	}
	if c := resp.Cookies(); len(c) != 0 {
		t.Errorf("wrong token set cookies: %v", c)
	}

	// Correct token: redirect to /admin plus the session cookie.
	resp = postLogin(t, ts.URL+"/admin/login", "secret")
	// Drain and close the redirect body (it has none) so the connection is
	// released back to the client pool instead of leaking per run.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login status = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/admin" {
		t.Fatalf("login redirect = %q, want /admin", loc)
	}
	// The login response carries the session cookie plus the double-submit
	// CSRF cookie (fb_csrf, readable by the SPA); the session cookie is the
	// one that must be HttpOnly + SameSite=Strict.
	var c *http.Cookie
	for _, cc := range resp.Cookies() {
		if cc.Name == "fb_admin" {
			c = cc
			break
		}
	}
	if c == nil || c.Value == "" {
		t.Fatal("login did not set the fb_admin session cookie")
		return
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie flags wrong: HttpOnly=%v SameSite=%v", c.HttpOnly, c.SameSite)
	}

	// The cookie unlocks /admin.
	authed := get(t, ts.URL+"/admin", c.Name+"="+c.Value)
	defer func() { _ = authed.Body.Close() }()
	if authed.StatusCode != http.StatusOK {
		t.Fatalf("authed status = %d, want 200", authed.StatusCode)
	}
	if !strings.Contains(bodyOf(t, authed), "freebucks-proxy") {
		t.Error("authed dashboard missing SPA content")
	}
}

// Tampered cookies are rejected (HMAC validation).
func TestDashboardRejectsTamperedCookie(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	// A syntactically valid-looking but unsigned cookie value.
	resp := get(t, ts.URL+"/admin", "fb_admin=9999999999.deadbeef")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("tampered-cookie status = %d, want 302 redirect to login", resp.StatusCode)
	}
}

// lockoutBound matches server.maxLoginFails (5), pinned by
// TestAdminAuthLockoutBound in auth_internal_test.go; this test drives the
// HTTP surface with one more attempt than the bound.
const lockoutBound = 5

// Assets are public (the login page loads them without a cookie).
func TestDashboardAssetsPublic(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	distFS := dashboard.DistFS()
	entries, err := fs.ReadDir(distFS, "assets")
	if err != nil || len(entries) == 0 {
		t.Fatalf("no assets in dist: %v", err)
	}
	assetPath := "/admin/assets/" + entries[0].Name()
	resp := get(t, ts.URL+assetPath, "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("asset status = %d, want 200 without a cookie (path: %s)", resp.StatusCode, assetPath)
	}
}

// With ADMIN_TOKEN unset (optional auth), the secret-bearing admin routes
// are loopback-only (SEC-2): a remote client gets 403, not 200, even when
// its Host header is loopback-named.
func TestDashboardConfigRemoteForbiddenWhenUnset(t *testing.T) {
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "" }, testutil.NewMock())
	req := httptest.NewRequest(http.MethodGet, "/admin/config", nil)
	req.RemoteAddr = "203.0.113.9:1234"
	req.Host = "127.0.0.1:3457"
	rec := httptest.NewRecorder()
	ts.Config.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("remote config status = %d, want 403 (loopback-only when ADMIN_TOKEN unset)", rec.Code)
	}
}

// TestDashboardConfigLoopbackAllowedWhenUnset: a genuine loopback client can
// read the config page and API and save a new .env when ADMIN_TOKEN is unset.
func TestDashboardConfigLoopbackAllowedWhenUnset(t *testing.T) {
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "" }, testutil.NewMock())

	// The SPA config editor page.
	page := httptest.NewRequest(http.MethodGet, "/admin/config", nil)
	page.RemoteAddr = "127.0.0.1:1234"
	page.Host = "127.0.0.1:3457"
	rec := httptest.NewRecorder()
	ts.Config.Handler.ServeHTTP(rec, page)
	if rec.Code != http.StatusOK {
		t.Fatalf("loopback config page status = %d, want 200", rec.Code)
	}

	// The JSON config API.
	api := httptest.NewRequest(http.MethodGet, "/admin/api/config", nil)
	api.RemoteAddr = "127.0.0.1:1234"
	api.Host = "127.0.0.1:3457"
	rec = httptest.NewRecorder()
	ts.Config.Handler.ServeHTTP(rec, api)
	if rec.Code != http.StatusOK {
		t.Fatalf("loopback config API status = %d, want 200", rec.Code)
	}

	// A save from a loopback client succeeds (temp dir: never touch a repo
	// .env). No process env override (TestMain strips ambient config env).
	t.Chdir(t.TempDir())
	body := "SAFE_MODE=false\nTRANSIENT_RETRIES=3\n"
	save := httptest.NewRequest(http.MethodPost, "/admin/config", strings.NewReader(body))
	save.Header.Set("Content-Type", "text/plain")
	save.RemoteAddr = "127.0.0.1:1234"
	save.Host = "127.0.0.1:3457"
	rec = httptest.NewRecorder()
	ts.Config.Handler.ServeHTTP(rec, save)
	if rec.Code != http.StatusOK {
		t.Fatalf("loopback config save status = %d: %s", rec.Code, rec.Body.String())
	}
	if respBody := rec.Body.String(); !strings.Contains(respBody, `"ok":true`) {
		t.Errorf("loopback config save body = %q, want ok:true", respBody)
	}
}

// TestDashboardConfigDNSRebindForbidden: a DNS-rebinding page resolves an
// attacker domain to 127.0.0.1, so it arrives with a loopback RemoteAddr —
// the loopback-named Host check must still reject it (SEC-2).
func TestDashboardConfigDNSRebindForbidden(t *testing.T) {
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "" }, testutil.NewMock())
	req := httptest.NewRequest(http.MethodGet, "/admin/config", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Host = "evil.example"
	rec := httptest.NewRecorder()
	ts.Config.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("DNS-rebinding config status = %d, want 403 (loopback addr + attacker Host)", rec.Code)
	}
}

// TestDashboardConfigIPv6LoopbackAllowed: the IPv6 loopback form
// ([::1]:port) passes the gate on both RemoteAddr and Host.
func TestDashboardConfigIPv6LoopbackAllowed(t *testing.T) {
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "" }, testutil.NewMock())
	req := httptest.NewRequest(http.MethodGet, "/admin/config", nil)
	req.RemoteAddr = "[::1]:1234"
	req.Host = "[::1]:3457"
	rec := httptest.NewRecorder()
	ts.Config.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("IPv6 loopback config status = %d, want 200", rec.Code)
	}
}

// TestDashboardLogsRemoteForbiddenWhenUnset: the logs API carries the same
// loopback-only gate as config when ADMIN_TOKEN is unset.
func TestDashboardLogsRemoteForbiddenWhenUnset(t *testing.T) {
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "" }, testutil.NewMock())
	req := httptest.NewRequest(http.MethodGet, "/admin/api/logs", nil)
	req.RemoteAddr = "203.0.113.9:1234"
	req.Host = "127.0.0.1:3457"
	rec := httptest.NewRecorder()
	ts.Config.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("remote logs status = %d, want 403", rec.Code)
	}
}

// TestDashboardConfigSaveEnvOverrideReported pins the fail-loud behavior: a
// save whose keys are shadowed by real process environment variables
// (precedence env > .env > JSON) still persists the file but reports
// ok:false with the overridden key names instead of a green all-clear.
func TestDashboardConfigSaveEnvOverrideReported(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("AUTH_TOKENS", "env-token")
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "" }, testutil.NewMock())

	body := "AUTH_TOKENS=file-token\nSAFE_MODE=false\n"
	req := httptest.NewRequest(http.MethodPost, "/admin/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "text/plain")
	req.RemoteAddr = "127.0.0.1:1234" // loopback client: the open-mode gate
	req.Host = "127.0.0.1:3457"
	rec := httptest.NewRecorder()
	ts.Config.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("config save status = %d, want 200 (write succeeded): %s", rec.Code, rec.Body.String())
	}
	var out struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.OK {
		t.Fatalf("save reported ok:true, want ok:false (AUTH_TOKENS env override): %q", out.Message)
	}
	if !strings.Contains(out.Message, "AUTH_TOKENS") {
		t.Errorf("override message = %q, want it to list AUTH_TOKENS", out.Message)
	}
	if strings.Contains(out.Message, "SAFE_MODE") {
		t.Errorf("override message = %q, must not list SAFE_MODE (no env var set for it)", out.Message)
	}
	// The file is still persisted — env outranks .env, so no rollback.
	env, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	if string(env) != body {
		t.Errorf(".env after override save = %q, want %q (persisted, not rolled back)", env, body)
	}
}

// TestDashboardLogoutClearsCookie: POST /admin/logout clears the fb_admin
// cookie (MaxAge<0) and answers JSON ok:true; a session-less client is then
// back behind the cookie gate. GET /admin/logout is unregistered (SPA
// fallthrough, clears nothing): the SPA logs out via POST
// (Sidebar.svelte handleLogout). Logout must work without a valid cookie
// (expired sessions).
func TestDashboardLogoutClearsCookie(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)

	// GET /admin/logout is unregistered: it falls through to the SPA shell
	// like any other unknown /admin/* path — and, crucially, clears no
	// cookie (the logout handler is POST-only now).
	getResp := get(t, ts.URL+"/admin/logout", cookie)
	func() { _ = getResp.Body.Close() }()
	unknownResp := get(t, ts.URL+"/admin/definitely-not-a-route", cookie)
	func() { _ = unknownResp.Body.Close() }()
	if getResp.StatusCode != unknownResp.StatusCode {
		t.Fatalf("logout GET status = %d, want the SPA-fallthrough status %d", getResp.StatusCode, unknownResp.StatusCode)
	}
	for _, c := range getResp.Cookies() {
		if c.Name == "fb_admin" && c.MaxAge < 0 {
			t.Fatal("logout GET cleared the fb_admin cookie (GET must not log out)")
		}
	}

	// POST logout clears the cookie and answers JSON ok:true.
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/admin/logout", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", cookie)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logout POST status = %d, want 200", resp.StatusCode)
	}
	if b := bodyOf(t, resp); !strings.Contains(b, `"ok":true`) {
		t.Errorf("logout POST body = %q, want ok:true JSON", b)
	}
	cleared := false
	for _, c := range resp.Cookies() {
		if c.Name == "fb_admin" {
			cleared = true
			if c.MaxAge >= 0 {
				t.Errorf("logout cookie MaxAge = %d, want < 0 (expired)", c.MaxAge)
			}
		}
	}
	if !cleared {
		t.Fatal("logout response did not set an expiring fb_admin cookie")
	}

	// Without the cookie the sensitive API is behind the gate again.
	gateReq := httptest.NewRequest(http.MethodGet, "/admin/api/config", nil)
	rec := httptest.NewRecorder()
	ts.Config.Handler.ServeHTTP(rec, gateReq)
	if rec.Code != http.StatusFound {
		t.Fatalf("config after logout status = %d, want 302 login redirect", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/login" {
		t.Errorf("config-after-logout Location = %q, want /admin/login", loc)
	}

	// POST logout without a cookie (expired session) still answers ok:true.
	req2, err := http.NewRequest(http.MethodPost, ts.URL+"/admin/logout", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp2, err := noRedirectClient().Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("logout POST status = %d, want 200", resp2.StatusCode)
	}
	if b := bodyOf(t, resp2); !strings.Contains(b, `"ok":true`) {
		t.Errorf("logout POST body = %q, want ok:true JSON", b)
	}
}

func TestDashboardLoginRateLimit(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	for range lockoutBound + 1 {
		resp := postLogin(t, ts.URL+"/admin/login", "wrong")
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	resp := postLogin(t, ts.URL+"/admin/login", "secret")
	defer func() { _ = resp.Body.Close() }()
	if !strings.Contains(bodyOf(t, resp), "Too many failed attempts") {
		t.Error("rate-limited login did not show lockout message")
	}
}

// --- config editor ---

// postConfig submits .env content to the authed config endpoint.
func postConfig(t *testing.T, url, cookie, content string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/admin/config", strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Cookie", cookie)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// authedCookie logs into the test dashboard and returns the fb_admin
// session cookie (the login response also carries the non-HttpOnly
// double-submit CSRF cookie, so the session cookie is matched by name).
func authedCookie(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	resp := postLogin(t, ts.URL+"/admin/login", "secret")
	defer func() { _ = resp.Body.Close() }()
	for _, c := range resp.Cookies() {
		if c.Name == "fb_admin" {
			return c.Name + "=" + c.Value
		}
	}
	t.Fatal("login did not set the fb_admin session cookie")
	return ""
}

// Token actions: unlock/finish/test endpoints work with a session cookie.
func TestDashboardTokenActions(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)

	// Unlock a token that has no lock — the action is idempotent success.
	resp := doTokenAction(t, ts.URL, cookie, "/admin/tokens/0/unlock")
	body := bodyOf(t, resp)
	if !strings.Contains(body, "Token 0 unlocked") {
		t.Errorf("unlock response = %q, want success message", body)
	}

	// Out-of-range token fails cleanly.
	resp = doTokenAction(t, ts.URL, cookie, "/admin/tokens/9/unlock")
	if !strings.Contains(bodyOf(t, resp), "out of range") {
		t.Error("out-of-range unlock did not report failure")
	}

	// Finish runs: succeeds (mock has no runs to finish).
	resp = doTokenAction(t, ts.URL, cookie, "/admin/tokens/0/finish")
	if !strings.Contains(bodyOf(t, resp), "runs finished") {
		t.Error("finish action did not report success")
	}

	// Test: zero-cost upstream probe against the mock (no session claim).
	resp = doTokenAction(t, ts.URL, cookie, "/admin/tokens/0/test")
	if !strings.Contains(bodyOf(t, resp), "zero-cost probe succeeded") {
		t.Error("test action did not report success")
	}
}

func doTokenAction(t *testing.T, url, cookie, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", cookie)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// Runtime token management endpoints: add, remove, mode switch, persisted to
// the overlay (unified-store; isolated via t.Chdir).
func TestDashboardTokenAddRemoveMode(t *testing.T) {
	ts, _, srv, st := newStoreBackedServer(t, nil, func(c *config.Config) { c.AdminToken = "secret" },
		testutil.NewMock())
	cookie := authedCookie(t, ts)

	// Add a token: pool grows, overlay row converges.
	resp := postJSON(t, ts.URL, cookie, "/admin/tokens/add", `{"token":"cb_newtoken123"}`)
	if body := bodyOf(t, resp); !strings.Contains(body, "Token added") {
		t.Errorf("add response = %q, want success", body)
	}
	srv.FlushSettingsSpill()
	row, ok, err := st.GetSetting(config.OverlayRowKey("AUTH_TOKENS"))
	if err != nil || !ok || !strings.Contains(row, "cb_newtoken123") {
		t.Errorf("overlay AUTH_TOKENS = %q,%v,%v, want the added token", row, ok, err)
	}
}

func postJSON(t *testing.T, url, cookie, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", cookie)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// A valid .env save persists the file and reports success.
func TestDashboardConfigSave(t *testing.T) {
	t.Chdir(t.TempDir())
	// Seed a prior .env with an unrelated key to prove the editor replaces
	// the file wholesale (not merge).
	if err := os.WriteFile(".env", []byte("SAFE_MODE=false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)

	content := "# my config\nTRANSIENT_RETRIES=3\nSAFE_MODE=true\n"
	resp := postConfig(t, ts.URL, cookie, content)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("save status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(bodyOf(t, resp), "Saved and reloaded") {
		t.Error("save response missing success class")
	}
	got, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf(".env after save = %q, want %q", got, content)
	}
}

// A rejected save restores the previous .env content (rollback).
func TestDashboardConfigSaveRejectedRollsBack(t *testing.T) {
	t.Chdir(t.TempDir())
	original := "SAFE_MODE=true\n"
	if err := os.WriteFile(".env", []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)

	// LISTEN_ADDR without a port fails Validate — the save must be rejected
	// and the file restored.
	resp := postConfig(t, ts.URL, cookie, "LISTEN_ADDR=127.0.0.1\n")
	defer func() { _ = resp.Body.Close() }()
	if !strings.Contains(bodyOf(t, resp), "Configuration rejected") {
		t.Error("rejected save missing error class")
	}
	got, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf(".env after rejected save = %q, want original %q", got, original)
	}
}

// TestDashboardConfigSaveRejectedUnreadableEnv pins the rollback guard for a
// present-but-unreadable .env: os.ReadFile fails for reasons other than
// absence (permissions, ACL), so the rollback must NOT remove the file —
// deleting it would destroy the operator's .env even though writeFileAtomic
// (temp + rename) could overwrite it. The rejected save leaves the newly
// written content in place and reports the rejection. POSIX-only: chmod 000
// does not block reads on Windows.
func TestDashboardConfigSaveRejectedUnreadableEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 000 does not make a file unreadable on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits")
	}
	t.Chdir(t.TempDir())
	// Present-but-unreadable: a file ReadFile cannot open, while the
	// rename-over in writeFileAtomic still succeeds (only the directory needs
	// write permission).
	if err := os.WriteFile(".env", []byte("SAFE_MODE=true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(".env", 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(".env"); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("setup: ReadFile = %v, want a non-NotExist error", err)
		return
	}
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)

	// LISTEN_ADDR without a port fails Validate — the save is rejected after
	// the .env was overwritten; the rollback must leave the file in place.
	resp := postConfig(t, ts.URL, cookie, "LISTEN_ADDR=127.0.0.1\n")
	defer func() { _ = resp.Body.Close() }()
	if !strings.Contains(bodyOf(t, resp), "Configuration rejected") {
		t.Error("rejected save missing error class")
	}
	if _, statErr := os.Stat(".env"); statErr != nil {
		t.Errorf(".env deleted by the rollback of an unreadable original: %v", statErr)
	}
}

// The config page renders the effective values with secrets redacted.
func TestDashboardConfigPage(t *testing.T) {
	t.Chdir(t.TempDir())
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/admin/api/config", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	ts.Config.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("config api status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`"has_env_file"`, `"effective"`, `"LISTEN_ADDR"`, `"AUTH_TOKENS"`, `"env_content"`} {
		if !strings.Contains(body, want) {
			t.Errorf("config api missing %q in: %s", want, body)
		}
	}
}

// A CRLF .env stays byte-identical after a token add (unified-store I4):
// mutations never rewrite the seed file, so a Windows-edited file keeps
// its line endings trivially — while the added token lands in the overlay.
func TestDashboardTokenAddPreservesCRLF(t *testing.T) {
	ts, _, srv, st := newStoreBackedServer(t, nil, func(c *config.Config) { c.AdminToken = "secret" },
		testutil.NewMock())
	cookie := authedCookie(t, ts)
	seed := "SAFE_MODE=true\r\nTRANSIENT_RETRIES=3\r\n"
	if err := os.WriteFile(".env", []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	resp := postJSON(t, ts.URL, cookie, "/admin/tokens/add", `{"token":"cb_crlf_token"}`)
	if body := bodyOf(t, resp); !strings.Contains(body, "Token added") {
		t.Errorf("add response = %q, want success", body)
	}

	env, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	if string(env) != seed {
		t.Errorf("token add rewrote the seed file:\n%q\nwant byte-identical %q", env, seed)
	}
	srv.FlushSettingsSpill()
	row, ok, err := st.GetSetting(config.OverlayRowKey("AUTH_TOKENS"))
	if err != nil || !ok || !strings.Contains(row, "cb_crlf_token") {
		t.Errorf("overlay AUTH_TOKENS = %q,%v,%v, want the added token", row, ok, err)
	}
}

// Concurrent token adds must not lose updates: adminSaveMu serializes the
// cfg read + overlay persist + swap, so every added token lands in the
// overlay, the live pool, and the swapped config — a lost pool token with
// a correct overlay (or vice versa) must fail here.
func TestDashboardConcurrentTokenAdds(t *testing.T) {
	t.Chdir(t.TempDir())
	mock := testutil.NewMock()
	defer mock.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "conc.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	cfg := &config.Config{
		AuthTokens:         []string{"tok-0"},
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    mock.URL(),
		AdminToken:         "secret",
		DashboardEnabled:   true,
	}
	clientCfg := *cfg
	clientCfg.UpstreamBaseURL = mock.URL()
	client, err := upstream.New(cfg.AuthTokens[0], &clientCfg)
	if err != nil {
		t.Fatal(err)
	}
	sessions := []*session.Manager{session.NewManager(client)}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, []*upstream.Client{client}, sessions, reg)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(cfg, p, reg, nil, nil, "", server.WithHistory(st))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	defer func() { _ = srv.Close() }()
	defer func() { _ = st.Close() }()
	cookie := authedCookie(t, ts)

	const n = 8
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			token := fmt.Sprintf("cb_conc_%d", i)
			resp := postJSON(t, ts.URL, cookie, "/admin/tokens/add", `{"token":"`+token+`"}`)
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}(i)
	}
	wg.Wait()

	// All 1+8 tokens must be in the live pool (synchronously swapped)...
	if got := p.TokenCount(); got != n+1 {
		t.Errorf("pool TokenCount = %d, want %d", got, n+1)
	}
	// ...and, after the spill drains, the overlay must agree with both
	// (the overlay is the source of truth for cfg after each add's swap).
	srv.FlushSettingsSpill()
	rows, err := st.ListSettings()
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		want := fmt.Sprintf("cb_conc_%d", i)
		if !strings.Contains(rows[config.OverlayRowKey("AUTH_TOKENS")], want) {
			t.Errorf("token %q lost from overlay after concurrent adds: %q", want, rows[config.OverlayRowKey("AUTH_TOKENS")])
		}
	}
	rebooted, err := config.LoadOpts("", config.LoadOptions{Overlay: config.OverlayFromRows(rows)})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(rebooted.AuthTokens); got != n+1 {
		t.Errorf("rebooted AUTH_TOKENS = %d, want %d", got, n+1)
	}
}

// postForm submits a urlencoded form to an admin POST endpoint (the browser
// dashboard's native wire format).
func postForm(t *testing.T, url, cookie, path string, form url.Values) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// A browser urlencoded save must decode the textarea's "content" field; a
// raw urlencoded body written verbatim as .env would destroy the file
// ("content=KEY=VALUE..."). The reload must also succeed on the decoded
// values.
func TestDashboardConfigSaveURLEncoded(t *testing.T) {
	t.Chdir(t.TempDir())
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)

	content := "# my config\nTRANSIENT_RETRIES=3\nSAFE_MODE=true\n"
	resp := postForm(t, ts.URL, cookie, "/admin/config", url.Values{"content": {content}})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("save status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(bodyOf(t, resp), "Saved and reloaded") {
		t.Error("urlencoded save response missing success class")
	}
	got, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf(".env after urlencoded save = %q, want decoded %q (not \"content=...\")", got, content)
	}
}

// TestDashboardConfigSaveSyncsPerDayToLiveTokens proves that saving a live
// knob via POST /admin/config immediately updates both the full
// /admin/api/tokens and the hot-poll /admin/api/tokens?view=live endpoints
// with the per-day display.
func TestDashboardConfigSaveSyncsPerDayToLiveTokens(t *testing.T) {
	t.Chdir(t.TempDir())
	ts := dashboardServer(t, "secret", func(c *config.Config) {
		c.AuthTokens = []string{"tok-0"}
	})
	cookie := authedCookie(t, ts)
	respPre := get(t, ts.URL+"/admin/api/tokens?view=live", cookie)
	if respPre.StatusCode != http.StatusOK {
		t.Fatalf("pre-save status = %d", respPre.StatusCode)
	}
	_ = respPre.Body.Close()

	content := "AUTH_TOKENS=tok-0\nADMIN_TOKEN=secret\nSLOTS_PER_ACCOUNT=1\n"
	saveResp := postForm(t, ts.URL, cookie, "/admin/config", url.Values{"content": {content}})
	if saveResp.StatusCode != http.StatusOK {
		t.Fatalf("save status = %d", saveResp.StatusCode)
	}
	_ = saveResp.Body.Close()

	respLive := get(t, ts.URL+"/admin/api/tokens?view=live", cookie)
	if respLive.StatusCode != http.StatusOK {
		t.Fatalf("live status = %d", respLive.StatusCode)
	}
	var live map[string]any
	if err := json.Unmarshal([]byte(bodyOf(t, respLive)), &live); err != nil {
		t.Fatal(err)
	}
	liveToks := live["tokens"].([]any)
	if len(liveToks) != 1 {
		t.Fatalf("live tokens len = %d, want 1", len(liveToks))
	}
	if _, ok := liveToks[0].(map[string]any)["requests_per_day"]; !ok {
		t.Error("live card missing requests_per_day")
	}
}

// The smoke form posts urlencoded model=&prompt=; the handler must read the
func TestDashboardSmokeFormAndJSON(t *testing.T) {
	t.Chdir(t.TempDir())
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)

	resp := postForm(t, ts.URL, cookie, "/admin/smoke", url.Values{"model": {modelA}, "prompt": {"ping"}})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("smoke form status = %d, want 200", resp.StatusCode)
	}
	if body := bodyOf(t, resp); !strings.Contains(body, `"ok":true`) && !strings.Contains(body, `"ok": true`) {
		t.Errorf("smoke form response = %q, want JSON ok:true", body)
	}

	resp = postJSON(t, ts.URL, cookie, "/admin/smoke", `{"model":"`+modelA+`","prompt":"ping"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("smoke JSON status = %d, want 200", resp.StatusCode)
	}
	if body := bodyOf(t, resp); !strings.Contains(body, `"ok":true`) && !strings.Contains(body, `"ok": true`) {
		t.Errorf("smoke JSON response = %q, want JSON ok:true", body)
	}
}

// Cross-origin admin POSTs must be rejected (CSRF): a browser on another
// origin sends Origin (and/or Sec-Fetch-Site) on every request, while curl
// and API clients send neither. Matching Origin and no-header requests pass.
func TestDashboardCSRF(t *testing.T) {
	t.Chdir(t.TempDir())
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)

	// Cross-origin Origin → 403 with the rejection fragment.
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/admin/tokens/add", strings.NewReader("token=cb_csrf_evil"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Origin", "http://evil.example")
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := bodyOf(t, resp)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin POST status = %d, want 403", resp.StatusCode)
	}
	if !strings.Contains(body, "Cross-origin request rejected.") {
		t.Errorf("cross-origin body = %q, want rejection message", body)
	}

	// Cross-site Sec-Fetch-Site (no Origin) → 403.
	req, err = http.NewRequest(http.MethodPost, ts.URL+"/admin/tokens/add", strings.NewReader("token=cb_csrf_evil2"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err = noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site Sec-Fetch-Site POST status = %d, want 403", resp.StatusCode)
	}

	// Matching Origin (the proxy's own authority) → passes.
	req, err = http.NewRequest(http.MethodPost, ts.URL+"/admin/tokens/add", strings.NewReader("token=cb_csrf_same"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Origin", ts.URL)
	resp, err = noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if body := bodyOf(t, resp); !strings.Contains(body, "Token added") {
		t.Errorf("same-origin POST body = %q, want success (matching Origin must pass)", body)
	}

	// No Origin/Sec-Fetch-Site headers (curl, API clients) → passes.
	resp = postJSON(t, ts.URL, cookie, "/admin/tokens/add", `{"token":"cb_csrf_curl"}`)
	if body := bodyOf(t, resp); !strings.Contains(body, "Token added") {
		t.Errorf("no-header POST body = %q, want success (header-less clients must pass)", body)
	}
}

// TestDashboardTokenRemoveRejectsShorterDivergence pins the divergence guard
// on the direction the pool cannot reconcile: SetConfig adopts tokens ADDED
// to AUTH_TOKENS (a config-editor save extends the pool), but it never
// REMOVES entries — removal belongs to RemoveLastToken/RemoveAllTokens. So
// when a config-editor save shrinks AUTH_TOKENS below the live pool, removing
// "the last token" from the stale list must be rejected instead of persisting
// a wrong .env.
func TestDashboardTokenRemoveRejectsShorterDivergence(t *testing.T) {
	t.Chdir(t.TempDir())
	ts := dashboardServer(t, "secret", nil) // 1 pooled token
	cookie := authedCookie(t, ts)

	// Grow the live pool to two entries through the token-add path.
	if resp := postJSON(t, ts.URL, cookie, "/admin/tokens/add", `{"token":"cb-extra"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("token add status = %d, want 200", resp.StatusCode)
	}

	// Config editor lists one token while the pool holds two.
	resp := postConfig(t, ts.URL, cookie, "AUTH_TOKENS=tok-0\nSAFE_MODE=true\n")
	if body := bodyOf(t, resp); !strings.Contains(body, "Saved and reloaded") {
		t.Fatalf("config save failed: %s", body)
	}

	resp = doTokenAction(t, ts.URL, cookie, "/admin/tokens/remove")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("remove status = %d, want 400 with rejection message", resp.StatusCode)
	}
	body := bodyOf(t, resp)
	if !strings.Contains(body, "differs from the live pool") {
		t.Errorf("remove response = %q, want divergence rejection", body)
	}
	env, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(env), "AUTH_TOKENS=") && !strings.Contains(string(env), "AUTH_TOKENS=tok-0\n") {
		t.Errorf("diverged .env content = %q, want untouched AUTH_TOKENS=tok-0", env)
	}
}

// TestDashboardTokenRemoveAfterEditorExtends pins the adopted-token
// direction: SetConfig reconciles appended AUTH_TOKENS entries into the
// pool, so after a config-editor save with two tokens the remove legitimately
// succeeds and persists only the remainder.
func TestDashboardTokenRemoveAfterEditorExtends(t *testing.T) {
	ts, _, srv, st := newStoreBackedServer(t, nil, func(c *config.Config) { c.AdminToken = "secret" },
		testutil.NewMock()) // 1 pooled token
	cookie := authedCookie(t, ts)

	// Config editor adopts an extra token (SetConfig appends the entry).
	editorContent := "AUTH_TOKENS=tok-0,extra-token\nSAFE_MODE=true\n"
	resp := postConfig(t, ts.URL, cookie, editorContent)
	if body := bodyOf(t, resp); !strings.Contains(body, "Saved and reloaded") {
		t.Fatalf("config save failed: %s", body)
	}

	resp = doTokenAction(t, ts.URL, cookie, "/admin/tokens/remove")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("remove status = %d, want 200 (pool adopted the editor list)", resp.StatusCode)
	}
	body := bodyOf(t, resp)
	if !strings.Contains(body, "removed and persisted") {
		t.Errorf("remove response = %q, want success message", body)
	}
	// The remainder persists to the overlay; the editor's seed file is
	// untouched by the remove (unified-store: no export leg).
	srv.FlushSettingsSpill()
	row, ok, err := st.GetSetting(config.OverlayRowKey("AUTH_TOKENS"))
	if err != nil || !ok || row != "tok-0" {
		t.Errorf("overlay AUTH_TOKENS = %q,%v,%v, want tok-0 only", row, ok, err)
	}
	if env, err := os.ReadFile(".env"); err != nil {
		t.Fatal(err)
	} else if string(env) != editorContent {
		t.Errorf(".env after remove = %q, want the editor bytes untouched", env)
	}
}

// A rejected overlay write after removal must roll the pool back
// (mirroring handleTokenAdd): with AUTH_TOKENS pinned by the process
// environment the verify guard rejects the derived config, nothing is
// enqueued or swapped, and the token is re-added so pool/overlay/cfg stay
// consistent.
func TestDashboardTokenRemoveRollsBackOnPersistFailure(t *testing.T) {
	t.Chdir(t.TempDir())
	// Env-pinned pool: the post-removal derive cannot move the effective
	// list, so overlayWrite's verify rejects.
	t.Setenv("AUTH_TOKENS", "env-pinned-token")

	mock := testutil.NewMock()
	defer mock.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "rollback.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	cfg := &config.Config{
		AuthTokens:         []string{"tok-0"},
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    mock.URL(),
		AdminToken:         "secret",
		DashboardEnabled:   true,
	}
	clientCfg := *cfg
	clientCfg.UpstreamBaseURL = mock.URL()
	client, err := upstream.New(cfg.AuthTokens[0], &clientCfg)
	if err != nil {
		t.Fatal(err)
	}
	sessions := []*session.Manager{session.NewManager(client)}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, []*upstream.Client{client}, sessions, reg)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(cfg, p, reg, nil, nil, "", server.WithHistory(st))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	defer func() { _ = srv.Close() }()
	defer func() { _ = st.Close() }()
	cookie := authedCookie(t, ts)

	resp := doTokenAction(t, ts.URL, cookie, "/admin/tokens/remove")
	body := bodyOf(t, resp)
	if !strings.Contains(body, "overridden by environment") {
		t.Errorf("remove response = %q, want the env-divergence rejection", body)
	}
	if got := p.TokenCount(); got != 1 {
		t.Errorf("pool TokenCount after rejected remove = %d, want 1 (rollback re-added the token)", got)
	}
	// Nothing was enqueued: the overlay holds no pool row.
	srv.FlushSettingsSpill()
	if v, ok, _ := st.GetSetting(config.OverlayRowKey("AUTH_TOKENS")); ok {
		t.Errorf("overlay AUTH_TOKENS = %q after rejected remove, want absent", v)
	}
}

// handleDiag must not append ":443" to an UpstreamBaseURL that already
// carries a port: the mock URL (http://127.0.0.1:PORT) is dialed as-is and
// reported reachable (a host that already has a port must never become
// "host:PORT:443"). The DNS row must also strip the port: LookupHost of
// "127.0.0.1:PORT" would treat the whole string as a DNS name and NXDOMAIN,
// a false red row next to a green TCP row (regression).
func TestDashboardDiagPortHandling(t *testing.T) {
	t.Chdir(t.TempDir())
	mock := testutil.NewMock()
	defer mock.Close()

	cfg := &config.Config{
		AuthTokens:         []string{"tok-0"},
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    mock.URL(),
		AdminToken:         "secret",
		DashboardEnabled:   true,
	}
	clientCfg := *cfg
	clientCfg.UpstreamBaseURL = mock.URL()
	client, err := upstream.New(cfg.AuthTokens[0], &clientCfg)
	if err != nil {
		t.Fatal(err)
	}
	sessions := []*session.Manager{session.NewManager(client)}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, []*upstream.Client{client}, sessions, reg)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(cfg, p, reg, nil, nil, "")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	cookie := authedCookie(t, ts)

	resp := postJSON(t, ts.URL, cookie, "/admin/diag", "{}")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("diag status = %d, want 200", resp.StatusCode)
	}
	body := bodyOf(t, resp)
	if !strings.Contains(body, "TCP reachable 127.0.0.1:") {
		t.Errorf("diag did not dial the ported mock host:\n%s", body)
	}
	if !strings.Contains(body, "DNS resolves 127.0.0.1") {
		t.Errorf("diag DNS row used the host:port as a DNS name:\n%s", body)
	}
	if strings.Contains(body, "DNS lookup failed") {
		t.Errorf("diag DNS row failed for a host with an explicit port:\n%s", body)
	}
	// The dial target must be the ported mock host as-is — never
	// "host:PORT:443". (A bare ":443" substring check would be unsound: the
	// mock's ephemeral port can itself contain ":443", e.g. 44375.)
	mockHost := strings.TrimPrefix(mock.URL(), "http://")
	if !strings.Contains(body, "TCP reachable "+mockHost) {
		t.Errorf("diag did not dial the ported mock host as-is (want %q):\n%s", "TCP reachable "+mockHost, body)
	}
}
