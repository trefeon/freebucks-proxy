package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/store"
)

// attachShadowStore threads a temp settings store into a server built without
// one (newReviewFixServer wires no history): the DB overlay then participates
// in loadConfig exactly like production.
func attachShadowStore(t *testing.T, s *Server) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "shadow.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s.hist = st
	s.admin.settings = st
	return st
}

// shadowLogin performs the dashboard password login and returns the session
// cookie (fb_admin only, so the double-submit CSRF check stays out of the
// way — these tests exercise handler diagnostics, not the CSRF gate).
func shadowLogin(t *testing.T, h http.Handler, password string) *http.Cookie {
	t.Helper()
	form := url.Values{"token": {password}}
	req := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("login status = %d, want 302", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "fb_admin" {
			return c
		}
	}
	t.Fatal("login did not set fb_admin cookie")
	return nil
}

// STUB (pool-only excision, Lane A): the mode switch is pooled-only and
// converges nothing. Lane B removes the dashboard call sites; the
// integration commit deletes the stubs + server_routes entries.

// TestRequireLoginConvergesOverlay: with DASHBOARD_REQUIRE_LOGIN pinned to
// true by a stale DB overlay row, the require-login toggle converges the row
// instead of 409ing — the toggle is write-through (DB-unified storage).
func TestRequireLoginConvergesOverlay(t *testing.T) {
	s := newReviewFixServer(t, "AUTH_TOKENS=tok-0\nADMIN_TOKEN=secretPass123\n", nil)
	st := attachShadowStore(t, s)
	if err := st.SetSetting(config.OverlayRowKey("DASHBOARD_REQUIRE_LOGIN"), "true"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	h := s.Handler()
	cookie := shadowLogin(t, h, "secretPass123")

	req := httptest.NewRequest(http.MethodPost, "/admin/api/require-login",
		strings.NewReader(`{"require_login":false}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	req.RemoteAddr = "127.0.0.1:4321"
	req.Host = "127.0.0.1:3457"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("require-login toggle status = %d, want 200 (stale overlay converges): %s", rec.Code, rec.Body.String())
	}
	if v, _, _ := st.GetSetting(config.OverlayRowKey("DASHBOARD_REQUIRE_LOGIN")); v != "false" {
		t.Errorf("overlay DASHBOARD_REQUIRE_LOGIN = %q, want converged %q", v, "false")
	}
	if s.admin.cfgLoad().RequireLogin() {
		t.Error("effective RequireLogin still true after toggle to false")
	}
}

func TestChangePasswordConvergesAdminTokenOverlay(t *testing.T) {
	s := newReviewFixServer(t, "AUTH_TOKENS=tok-0\nADMIN_TOKEN=secretPass123\n", nil)
	st := attachShadowStore(t, s)
	h := s.Handler()
	cookie := shadowLogin(t, h, "secretPass123")
	// Seed the stale migrated row after login: it would shadow the file on
	// the next reload, so the change must converge it instead of failing
	// its divergence guard on the migrated row.
	if err := st.SetSetting(config.OverlayRowKey("ADMIN_TOKEN"), "stale-pass"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/change-password",
		strings.NewReader(`{"current_password":"secretPass123","new_password":"rotatedPass789"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("change-password status = %d, want 200 (stale overlay converges): %s", rec.Code, rec.Body.String())
	}
	if v, _, _ := st.GetSetting(config.OverlayRowKey("ADMIN_TOKEN")); v != "rotatedPass789" {
		t.Error("overlay ADMIN_TOKEN not converged to the new credential")
	}
	if got := s.admin.cfgLoad().AdminToken; got != "rotatedPass789" {
		t.Error("effective ADMIN_TOKEN not rotated")
	}
}
