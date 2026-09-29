package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"freebuff-proxy/backend/internal/dashboard"
)

// TestAdminRoutesAllRegister pins the admin route table to the mux: every
// row must be mountable and reachable with its registered method pattern,
// and no row may be orphaned (an unmounted row would silently 404, an
// unimplemented row panics registerAdminRoutes). A wrong-method probe
// returns 405 exactly when the path's pattern is registered (ServeMux's
// documented behavior), without invoking the auth middlewares.
func TestAdminRoutesAllRegister(t *testing.T) {
	s := &Server{} // registration only; handlers are never invoked here
	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)

	for _, r := range dashboard.AdminRoutes {
		probe := http.MethodDelete
		if r.Method == http.MethodDelete {
			probe = http.MethodPatch
		}
		req := httptest.NewRequest(probe, r.Path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: wrong-method probe status = %d, want 405 (row not registered)", r.Method, r.Path, rec.Code)
		}
	}
}
