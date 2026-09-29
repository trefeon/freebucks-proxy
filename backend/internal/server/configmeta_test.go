package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"freebuff-proxy/backend/internal/config"
)

// TestDashboardConfigMetaEndpoint pins GET /admin/api/config/meta: with a
// dashboard session cookie it returns the config catalog as a JSON array
// (ordered, complete, snake_case fields); without the cookie the dashboard
// auth gate redirects to the login page.
func TestDashboardConfigMetaEndpoint(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	defer ts.Close()

	// Authenticated: 200 + JSON array of catalog entries.
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/admin/api/config/meta", nil)
	if err != nil {
		t.Fatal(err)
	}
	cookie := authedCookie(t, ts)
	req.Header.Set("Cookie", cookie)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("meta status = %d, want 200", resp.StatusCode)
	}
	var catalog []config.KeyDef
	if err := json.Unmarshal([]byte(bodyOf(t, resp)), &catalog); err != nil {
		t.Fatalf("meta response is not a JSON array: %v", err)
	}
	if len(catalog) == 0 {
		t.Fatal("meta response is an empty array")
	}
	first := catalog[0]
	if first.Key == "" || first.Group == "" || first.Kind == "" || first.Description == "" {
		t.Errorf("first catalog entry incomplete: %+v", first)
	}
	if !hasKey(catalog, "LOG_LEVEL") {
		t.Error("catalog missing LOG_LEVEL")
	}
	if !isSecret(catalog, "ADMIN_TOKEN") {
		t.Error("catalog does not flag ADMIN_TOKEN as secret")
	}
	// Unauthenticated: 302 login redirect behind dashboardAuth.
	req2, err := http.NewRequest(http.MethodGet, ts.URL+"/admin/api/config/meta", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp2 := httptest.NewRecorder()
	ts.Config.Handler.ServeHTTP(resp2, req2)
	if resp2.Code != http.StatusFound {
		t.Fatalf("meta without cookie status = %d, want 302", resp2.Code)
	}
	if loc := resp2.Header().Get("Location"); loc != "/admin/login" {
		t.Errorf("meta without cookie Location = %q, want /admin/login", loc)
	}
}

func hasKey(catalog []config.KeyDef, key string) bool {
	for _, def := range catalog {
		if def.Key == key {
			return true
		}
	}
	return false
}

func isSecret(catalog []config.KeyDef, key string) bool {
	for _, def := range catalog {
		if def.Key == key && def.Secret {
			return true
		}
	}
	return false
}
