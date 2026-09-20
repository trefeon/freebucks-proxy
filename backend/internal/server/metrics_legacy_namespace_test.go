package server_test

import (
	"freebucks-proxy/backend/internal/testutil"
	"net/http"
	"strings"
	"testing"
)

// legacyNamespace rewrites the current metric prefix back to the pre-rename
// product namespace. Derived, not duplicated: the alias section in health.go
// computes its prefix the same way, so the two cannot drift apart.
var legacyNamespace = strings.Replace("freebucks_proxy_", "freebucks", "freebuff", 1)

// TestMetricsLegacyNamespaceAliases pins the one-release compatibility promise:
// /metrics keeps serving the pre-rename product namespace next to the current
// one, so scrapers, dashboards and alerts configured before the rename keep
// resolving instead of silently going empty. Remove with the alias section.
func TestMetricsLegacyNamespaceAliases(t *testing.T) {
	testutil.UnsetConfigEnv(t)
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodGet, ts.URL+"/metrics", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200: %s", resp.StatusCode, data)
	}
	metrics := string(data)

	for _, family := range []string{"uptime_seconds", "tokens_total", "models_total", "rate_limit_rejected_total"} {
		current := "freebucks_proxy_" + family
		alias := legacyNamespace + family
		if !strings.Contains(metrics, current) {
			t.Errorf("metrics missing current family %s", current)
		}
		if !strings.Contains(metrics, alias) {
			t.Errorf("metrics missing legacy alias %s", alias)
		}
	}

	// The alias section must re-emit real samples, not just HELP/TYPE headers.
	if !strings.Contains(metrics, legacyNamespace+"uptime_seconds ") {
		t.Errorf("legacy alias section carries no sample line for %suptime_seconds", legacyNamespace)
	}
}
