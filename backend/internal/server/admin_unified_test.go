package server

// Unified-store behavioral invariants (ADR docs/decisions/unified-store.md):
// every lane proves all four. Config plane owns the settings/token/
// password mutations (overlay + sync mem swap + WAL spill).

import (
	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/store"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// unifiedServer builds a single-token server with a temp .env seed and an
// attached settings store, returning the server, the store, and the seed
// bytes for I4 comparisons.
func unifiedServer(t *testing.T, seed string) (*Server, *store.Store, []byte) {
	t.Helper()
	s := newReviewFixServer(t, seed, nil)
	st := attachShadowStore(t, s)
	before, err := os.ReadFile(filepath.Join(".", ".env"))
	if err != nil {
		t.Fatalf("read seed .env: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, st, before
}

func unifiedPost(t *testing.T, h http.Handler, cookie *http.Cookie, path, payload string) *httptest.ResponseRecorder {
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

// TestUnifiedI1_MutationVisibleSynchronously: a settings POST is visible to
// the next read synchronously — mem swapped in the same handler, BEFORE any
// spill drain (no flush anywhere in this test).
func TestUnifiedI1_MutationVisibleSynchronously(t *testing.T) {
	s, _, _ := unifiedServer(t, "AUTH_TOKENS=tok-0\nADMIN_TOKEN=secretPass123\n")
	h := s.Handler()
	cookie := shadowLogin(t, h, "secretPass123")

	rec := unifiedPost(t, h, cookie, "/admin/api/settings", `{"key":"QUEUE_WAIT","value":"45s"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("settings POST status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// Synchronous visibility, no flush: mem already swapped...
	if got := s.admin.cfgLoad().QueueWait.Seconds(); got != 45 {
		t.Fatalf("mem QueueWait = %v, want 45s before any spill drain", s.admin.cfgLoad().QueueWait)
	}
	// ...and the next read serves it.
	req := httptest.NewRequest(http.MethodGet, "/admin/api/settings", nil)
	req.AddCookie(cookie)
	req.RemoteAddr = "127.0.0.1:4321"
	req.Host = "127.0.0.1:3457"
	get := httptest.NewRecorder()
	h.ServeHTTP(get, req)
	if get.Code != http.StatusOK {
		t.Fatalf("settings GET status = %d, want 200", get.Code)
	}
	if !strings.Contains(get.Body.String(), "45s") {
		t.Errorf("settings GET missing the just-mutated 45s value (pre-flush): %s", get.Body.String())
	}
}

// TestUnifiedI2_NoDiskOnRequestPath: with the spill drain paused, a
// mutation still applies synchronously (mem + response) while the store
// stays untouched — the handler performed zero synchronous writes
// (write-absence counting, not timing).
func TestUnifiedI2_NoDiskOnRequestPath(t *testing.T) {
	s, st, _ := unifiedServer(t, "AUTH_TOKENS=tok-0\nADMIN_TOKEN=secretPass123\n")
	h := s.Handler()
	cookie := shadowLogin(t, h, "secretPass123")

	resume := s.admin.pauseSpill()
	rec := unifiedPost(t, h, cookie, "/admin/api/settings", `{"key":"QUEUE_WAIT","value":"50s"}`)
	if rec.Code != http.StatusOK {
		resume()
		t.Fatalf("settings POST status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := s.admin.cfgLoad().QueueWait.Seconds(); got != 50 {
		resume()
		t.Fatalf("mem QueueWait = %v, want 50s while spill paused", s.admin.cfgLoad().QueueWait)
	}
	// Drain paused: nothing may have reached the store.
	if rows, err := st.ListSettings(); err != nil {
		resume()
		t.Fatalf("ListSettings: %v", err)
	} else if len(rows) != 0 {
		resume()
		t.Fatalf("store holds %d rows while spill paused, want 0 (no sync disk): %v", len(rows), rows)
	}
	resume()
	// After resume + drain, the delta lands (durability without the
	// request path ever blocking on it).
	s.admin.flushSettingsSpill()
	if v, ok, err := st.GetSetting(config.OverlayRowKey("QUEUE_WAIT")); err != nil || !ok || v != "50s" {
		t.Errorf("QUEUE_WAIT row = %q,%v,%v after drain, want 50s,true,nil", v, ok, err)
	}
}

// TestUnifiedI3_RestartRecovers: after the drain, a simulated reboot
// (ListSettings → OverlayFromRows → LoadOpts, the cli_serve boot path)
// recovers both a knob row and the token pool row.
func TestUnifiedI3_RestartRecovers(t *testing.T) {
	s, st, _ := unifiedServer(t, "AUTH_TOKENS=tok-0\nADMIN_TOKEN=secretPass123\n")
	h := s.Handler()
	cookie := shadowLogin(t, h, "secretPass123")

	rec := unifiedPost(t, h, cookie, "/admin/api/settings", `{"key":"QUEUE_WAIT","value":"55s"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("settings POST status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// Token-pool row without touching the pool (pool behavior is Lane B's;
	// the persist shape is this lane's): overlay converging to a 2-list.
	set, del := tokenMarkerDelta([]string{"tok-0", "tok-1"})
	if _, err := s.admin.overlayWrite(set, del, nil); err != nil {
		t.Fatalf("overlayWrite tokens: %v", err)
	}
	s.admin.flushSettingsSpill()

	rows, err := st.ListSettings()
	if err != nil {
		t.Fatalf("ListSettings: %v", err)
	}
	rebooted, err := config.LoadOpts("", config.LoadOptions{Overlay: config.OverlayFromRows(rows)})
	if err != nil {
		t.Fatalf("reboot LoadOpts: %v", err)
	}
	if rebooted.QueueWait.Seconds() != 55 {
		t.Errorf("rebooted QueueWait = %v, want 55s", rebooted.QueueWait)
	}
	if len(rebooted.AuthTokens) != 2 || rebooted.AuthTokens[1] != "tok-1" {
		t.Errorf("rebooted AuthTokens = %v, want [tok-0 tok-1]", rebooted.AuthTokens)
	}
}

// TestUnifiedI4_EnvNeverWrittenNeverReread: mutations across all three
// owned writers (settings knob, token rows, password rows) leave the .env
// bytes identical; reads keep working with the .env removed post-boot.
func TestUnifiedI4_EnvNeverWrittenNeverReread(t *testing.T) {
	s, st, before := unifiedServer(t, "AUTH_TOKENS=tok-0\nADMIN_TOKEN=secretPass123\n")
	h := s.Handler()
	cookie := shadowLogin(t, h, "secretPass123")

	envIdentical := func(stage string) {
		t.Helper()
		after, err := os.ReadFile(filepath.Join(".", ".env"))
		if err != nil {
			t.Fatalf("%s: read .env: %v", stage, err)
		}
		if string(after) != string(before) {
			t.Errorf("%s mutated .env:\nbefore %q\nafter  %q", stage, before, after)
		}
	}

	// 1. Settings knob.
	if rec := unifiedPost(t, h, cookie, "/admin/api/settings", `{"key":"QUEUE_WAIT","value":"45s"}`); rec.Code != http.StatusOK {
		t.Fatalf("settings POST status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// 2. Token rows (persist shape; the pool itself is Lane B's).
	set, del := tokenMarkerDelta([]string{"tok-0", "tok-1"})
	newCfg, err := s.admin.overlayWrite(set, del, nil)
	if err != nil {
		t.Fatalf("overlayWrite tokens: %v", err)
	}
	s.admin.applyReloadedConfig(&newCfg)
	// 3. Password rows (persist shape; the cookie refresh is the handler's).
	pwSet := map[string]string{
		config.OverlayRowKey("ADMIN_TOKEN"):             "rotatedPass789",
		config.OverlayRowKey("DASHBOARD_REQUIRE_LOGIN"): "true",
	}
	newCfg, err = s.admin.overlayWrite(pwSet, nil, nil)
	if err != nil {
		t.Fatalf("overlayWrite password: %v", err)
	}
	s.admin.applyReloadedConfig(&newCfg)
	s.admin.flushSettingsSpill()

	envIdentical("mutations")
	if got := s.admin.cfgLoad().QueueWait.Seconds(); got != 45 {
		t.Errorf("mem QueueWait = %v, want 45s", s.admin.cfgLoad().QueueWait)
	}

	// The file is gone post-boot: reads still serve from mem + overlay.
	if err := os.Remove(filepath.Join(".", ".env")); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/admin/api/settings", nil)
	req.AddCookie(cookie)
	req.RemoteAddr = "127.0.0.1:4321"
	req.Host = "127.0.0.1:3457"
	get := httptest.NewRecorder()
	h.ServeHTTP(get, req)
	if get.Code != http.StatusOK {
		t.Fatalf("settings GET without .env status = %d, want 200", get.Code)
	}
	if !strings.Contains(get.Body.String(), "45s") {
		t.Errorf("settings GET without .env lost the overlay value: %s", get.Body.String())
	}
	if got := s.admin.cfgLoad().AdminToken; got != "rotatedPass789" {
		t.Errorf("mem AdminToken without .env = %q, want rotated", got)
	}
	_ = st
}
