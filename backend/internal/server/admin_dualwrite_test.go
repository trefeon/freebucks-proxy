package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/store"
)

// overlayWritePost is the loopback-authenticated JSON POST helper for the
// dual-layer persist tests (mirrors the require-login flow test).
func overlayWritePost(t *testing.T, h http.Handler, cookie *http.Cookie, path, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	req.RemoteAddr = "127.0.0.1:4321"
	req.Host = "127.0.0.1:3457"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func overlayWriteStoreRows(t *testing.T, st *store.Store) map[string]string {
	t.Helper()
	rows, err := st.ListSettings()
	if err != nil {
		t.Fatalf("ListSettings: %v", err)
	}
	return rows
}

// TestOverlayWriteRequireLoginRoundTrip pins the unified-store contract: a
// require-login toggle swaps mem synchronously and lands ONLY in the
// settings overlay (runtime truth) — the .env seed bytes stay identical —
// and a simulated reboot (ListSettings → OverlayFromRows → LoadOpts) reads
// the settings value back even when the .env seed is later edited
// underneath.
func TestOverlayWriteRequireLoginRoundTrip(t *testing.T) {
	s := newReviewFixServer(t, "AUTH_TOKENS=tok-0\nADMIN_TOKEN=secretPass123\n", nil)
	st := attachShadowStore(t, s)
	h := s.Handler()
	cookie := shadowLogin(t, h, "secretPass123")
	envBefore, err := os.ReadFile(filepath.Join(".", ".env"))
	if err != nil {
		t.Fatalf("read .env: %v", err)
	}

	rec := overlayWritePost(t, h, cookie, "/admin/api/require-login", `{"require_login":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// I1: the swap is synchronously visible, before any spill drain.
	if s.admin.cfgLoad().RequireLogin() {
		t.Fatal("effective RequireLogin not false after toggle")
	}

	// I4: the mutation leaves the .env bytes identical (no export leg).
	envAfter, err := os.ReadFile(filepath.Join(".", ".env"))
	if err != nil {
		t.Fatalf("read .env: %v", err)
	}
	if string(envAfter) != string(envBefore) {
		t.Errorf(".env mutated by overlay write: %q -> %q", envBefore, envAfter)
	}
	s.admin.flushSettingsSpill()
	rows := overlayWriteStoreRows(t, st)
	if rows[config.OverlayRowKey("DASHBOARD_REQUIRE_LOGIN")] != "false" {
		t.Errorf("settings overlay row = %q, want %q (full dump %v)",
			rows[config.OverlayRowKey("DASHBOARD_REQUIRE_LOGIN")], "false", rows)
	}

	// Simulated reboot: the overlay read back through the boot path
	// (cli_serve: ListSettings → OverlayFromRows → LoadOpts).
	ov := config.OverlayFromRows(overlayWriteStoreRows(t, st))
	rebooted, err := config.LoadOpts("", config.LoadOptions{Overlay: ov})
	if err != nil {
		t.Fatalf("reboot LoadOpts: %v", err)
	}
	if rebooted.RequireLogin() {
		t.Error("rebooted RequireLogin = true, want false (settings is runtime truth)")
	}

	// The overlay beats a later .env seed edit: rewrite the file underneath
	// and the effective config must not move.
	edited := string(envBefore) + "DASHBOARD_REQUIRE_LOGIN=true\n"
	if err := os.WriteFile(".env", []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	shadowed, err := s.admin.loadConfig()
	if err != nil {
		t.Fatalf("loadConfig after seed edit: %v", err)
	}
	if shadowed.RequireLogin() {
		t.Error("RequireLogin flipped to true after .env seed edit, want overlay (false) to win")
	}

	// The require-login path writes only its own knob: no credential row or
	// value may appear as its side effect.
	for k, v := range overlayWriteStoreRows(t, st) {
		if k == config.OverlayRowKey("AUTH_TOKENS") || k == config.OverlayRowKey("ADMIN_TOKEN") {
			t.Errorf("secret overlay row %q written by the require-login path", k)
		}
		if strings.Contains(v, "secretPass123") || strings.Contains(v, "tok-0") {
			t.Errorf("settings row %q leaks secret material: %q", k, v)
		}
	}
}

// TestOverlayWriteRequireLoginDivergenceRejects pins the divergence guard:
// when the process environment shadows the toggle, the 409 enqueues and
// swaps nothing — the .env bytes stay identical AND the settings row keeps
// its prior value (or absence), both when the row held a prior value and
// when the row did not exist before the write.
func TestOverlayWriteRequireLoginDivergenceRejects(t *testing.T) {
	newSeeded := func(t *testing.T, seedRow bool) (*Server, *store.Store) {
		// Open-mode seed (DASHBOARD_REQUIRE_LOGIN=false clears AdminToken at
		// load): no login cookie exists, so the toggle posts as an
		// unauthenticated loopback client, exactly like the open-mode
		// config-save precedent.
		s := newReviewFixServer(t, "AUTH_TOKENS=tok-0\nADMIN_TOKEN=secretPass123\nDASHBOARD_REQUIRE_LOGIN=false\n", nil)
		st := attachShadowStore(t, s)
		if seedRow {
			if err := st.SetSetting(config.OverlayRowKey("DASHBOARD_REQUIRE_LOGIN"), "false"); err != nil {
				t.Fatalf("SetSetting: %v", err)
			}
		}
		return s, st
	}

	for _, seedRow := range []bool{true, false} {
		func() {
			s, st := newSeeded(t, seedRow)
			h := s.Handler()
			envBefore, err := os.ReadFile(".env")
			if err != nil {
				t.Fatal(err)
			}
			// The environment outranks the overlay: the toggle cannot take
			// effect, so nothing persists anywhere. Scoped to this
			// iteration (not t.Setenv) so nothing leaks across seeds.
			if err := os.Setenv("DASHBOARD_REQUIRE_LOGIN", "false"); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = os.Unsetenv("DASHBOARD_REQUIRE_LOGIN") }()

			rec := overlayWritePost(t, h, nil, "/admin/api/require-login", `{"require_login":true}`)
			if rec.Code != http.StatusConflict {
				t.Fatalf("seedRow=%v toggle status = %d, want 409: %s", seedRow, rec.Code, rec.Body.String())
			}
			var res struct {
				OK      bool   `json:"ok"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if res.OK || !strings.Contains(res.Message, "rejected") {
				t.Errorf("seedRow=%v response = %+v, want ok=false with rejection text", seedRow, res)
			}

			if envAfter, err := os.ReadFile(".env"); err != nil {
				t.Fatal(err)
			} else if string(envAfter) != string(envBefore) {
				t.Errorf("seedRow=%v .env mutated by rejected toggle: %q", seedRow, envAfter)
			}
			s.admin.flushSettingsSpill()
			v, ok, err := st.GetSetting(config.OverlayRowKey("DASHBOARD_REQUIRE_LOGIN"))
			if err != nil {
				t.Fatal(err)
			}
			if seedRow {
				if !ok || v != "false" {
					t.Errorf("seedRow=true settings row = %q,%v, want %q,true (prior value kept)", v, ok, "false")
				}
			} else if ok {
				t.Errorf("seedRow=false settings row = %q, want absent (rejected write enqueued nothing)", v)
			}
		}()
	}
}

// TestTokenMarkerDelta pins the write-through mapping: pooled lists set the
// presence flag AND converge the config:AUTH_TOKENS overlay row to the same
// list; an emptied pool converges the row empty and drops the marker row.
func TestTokenMarkerDelta(t *testing.T) {
	set, del := tokenMarkerDelta([]string{"a", "b"})
	if set[tokenMarkerKey] != "true" || len(del) != 0 {
		t.Errorf("pooled delta = (%v, %v), want marker set", set, del)
	}
	if set[config.OverlayRowKey("AUTH_TOKENS")] != "a,b" {
		t.Errorf("pooled delta AUTH_TOKENS row = %q, want converged %q", set[config.OverlayRowKey("AUTH_TOKENS")], "a,b")
	}
	set, del = tokenMarkerDelta(nil)
	if set[config.OverlayRowKey("AUTH_TOKENS")] != "" || len(set) != 1 {
		t.Errorf("empty delta set = %v, want only the empty AUTH_TOKENS pin", set)
	}
	if len(del) != 1 || del[0] != tokenMarkerKey {
		t.Errorf("empty delta del = %v, want marker deleted", del)
	}
}
