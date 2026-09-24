package server

import (
	"freebucks-proxy/backend/internal/dashboard"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestStreakTouchRoutePinned pins the dashboard streak-touch endpoint's
// method and mount. The MaturityPanel Touch-now button POSTs
// /admin/tokens/streak-touch (frontend paths.js adminActions.streakTouch),
// but the row was missing from admin_manifest.json in v1.19.0 while the
// handler case (server_routes.go) and the SPA caller existed — so the mux
// answered 405 Method Not Allowed on the live dashboard ("Streak touch
// failed"). A wrong-method probe 405s exactly when the pattern IS
// registered, so DELETE 405 proves the mount without invoking auth.
func TestStreakTouchRoutePinned(t *testing.T) {
	var found *dashboard.AdminRoute
	for i, r := range dashboard.AdminRoutes {
		if r.Path == "/admin/tokens/streak-touch" {
			found = &dashboard.AdminRoutes[i]
			break
		}
	}
	if found == nil {
		t.Fatal("POST /admin/tokens/streak-touch missing from dashboard.AdminRoutes: the SPA POSTs it, an unmounted path 405s")
	}
	if found.Method != http.MethodPost {
		t.Errorf("streak-touch method = %s, want POST (the SPA caller posts)", found.Method)
	}
	if found.Auth != dashboard.AuthSensitive {
		t.Errorf("streak-touch auth = %q, want %q", found.Auth, dashboard.AuthSensitive)
	}

	s := &Server{}
	mux := http.NewServeMux()
	s.registerAdminRoutes(mux) // panics when adminHandler has no case for the row
	req := httptest.NewRequest(http.MethodDelete, "/admin/tokens/streak-touch", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /admin/tokens/streak-touch = %d, want 405 (row not mounted)", rec.Code)
	}
}
