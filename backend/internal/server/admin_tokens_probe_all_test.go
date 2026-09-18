package server_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/pool"
	"freebuff-proxy/backend/internal/testutil"
)

// TestTokensTestAllHealthy: two healthy mocks → 200 JSON array of 2, both ok.
func TestTokensTestAllHealthy(t *testing.T) {
	t.Chdir(t.TempDir())
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "secret" },
		testutil.NewMock(), testutil.NewMock())
	defer ts.Close()
	cookie := authedCookie(t, ts)

	resp := postJSON(t, ts.URL, cookie, "/admin/tokens/test-all", `{}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("test-all status = %d, want 200: %s", resp.StatusCode, bodyOf(t, resp))
	}
	var out []pool.ProbeTokenOutcome
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode test-all response: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("len(outcomes) = %d, want 2", len(out))
	}
	for i, o := range out {
		if o.Status != "ok" {
			t.Errorf("outcomes[%d].Status = %q, want ok (detail %q)", i, o.Status, o.Detail)
		}
		if o.Quarantined {
			t.Errorf("outcomes[%d].Quarantined = true, want false", i)
		}
		if o.Index != i {
			t.Errorf("outcomes[%d].Index = %d, want %d", i, o.Index, i)
		}
	}
}

// TestTokensTestAllBanned: one banned mock → 200 array where the banned entry
// reports banned + quarantined, and the pool snapshot shows quarantine.
func TestTokensTestAllBanned(t *testing.T) {
	t.Chdir(t.TempDir())
	healthy := testutil.NewMock()
	banned := testutil.NewMock()
	banned.Ban = true // every session route 403 {"status":"banned"}
	ts, p := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "secret" },
		healthy, banned)
	defer ts.Close()
	cookie := authedCookie(t, ts)

	resp := postJSON(t, ts.URL, cookie, "/admin/tokens/test-all", `{}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("test-all status = %d, want 200: %s", resp.StatusCode, bodyOf(t, resp))
	}
	var out []pool.ProbeTokenOutcome
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode test-all response: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("len(outcomes) = %d, want 2", len(out))
	}
	if out[0].Status != "ok" {
		t.Errorf("outcomes[0].Status = %q, want ok", out[0].Status)
	}
	if out[1].Status != "banned" {
		t.Errorf("outcomes[1].Status = %q, want banned", out[1].Status)
	}
	if !out[1].Quarantined {
		t.Error("outcomes[1].Quarantined = false, want true")
	}
	snap := p.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("len(snapshot) = %d, want 2", len(snap))
	}
	if snap[0].Quarantined {
		t.Error("snapshot[0].Quarantined = true, want false")
	}
	if !snap[1].Quarantined {
		t.Error("snapshot[1].Quarantined = false, want true (banned quarantined)")
	}
}

// TestTokensTestAllUnauthenticated: no session cookie → login redirect,
// same gate as the existing per-token test endpoint.
func TestTokensTestAllUnauthenticated(t *testing.T) {
	t.Chdir(t.TempDir())
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "secret" },
		testutil.NewMock())
	defer ts.Close()

	resp := postJSON(t, ts.URL, "", "/admin/tokens/test-all", `{}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("unauthenticated test-all status = %d, want 302 login redirect", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/admin/login" {
		t.Errorf("redirect Location = %q, want /admin/login", loc)
	}
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "application/json") {
		if body := bodyOf(t, resp); strings.Contains(body, `"status":"ok"`) {
			t.Error("unauthenticated response leaked probe data")
		}
	}
}
