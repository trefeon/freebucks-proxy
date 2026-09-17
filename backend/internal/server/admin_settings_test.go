package server_test

import (
	"encoding/json"
	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/server"
	"freebuff-proxy/backend/internal/store"
	"freebuff-proxy/backend/internal/testutil"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// settingsTestServer builds a store-backed gateway (ADR-0019): the temp DB
// feeds the settings overlay through server.WithHistory, and the login
// returns both session cookies the mutation endpoints require.
func settingsTestServer(t *testing.T) (*httptest.Server, string, string) {
	t.Helper()
	ts, cookie, csrf, _ := settingsStoreTestServer(t)
	return ts, cookie, csrf
}

// settingsStoreTestServer is settingsTestServer plus the store handle, for
// tests that assert what did (or did not) land in the settings table.
func settingsStoreTestServer(t *testing.T) (*httptest.Server, string, string, *store.Store) {
	t.Helper()
	t.Chdir(t.TempDir())
	st, err := store.Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv, _ := server.NewTestServerStack(t, nil, []*testutil.MockUpstream{testutil.NewMock()},
		func(c *config.Config) { c.AdminToken = "secret" }, nil, nil, server.WithHistory(st))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp := postLogin(t, ts.URL+"/admin/login", "secret")
	defer func() { _ = resp.Body.Close() }()
	var admin, csrf string
	for _, c := range resp.Cookies() {
		if c.Name == "fb_admin" {
			admin = c.Name + "=" + c.Value
		}
		if c.Name == "fb_csrf" {
			csrf = c.Value
		}
	}
	if admin == "" || csrf == "" {
		t.Fatal("login did not set fb_admin + fb_csrf cookies")
	}
	return ts, admin + "; fb_csrf=" + csrf, csrf, st
}

// settingsDo performs one settings request and decodes the JSON envelope.
func settingsDo(t *testing.T, method, url, cookie, csrf string, body any) (int, map[string]any) {
	t.Helper()
	var reader *strings.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = strings.NewReader(string(raw))
	} else {
		reader = strings.NewReader("")
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("%s %s: decode: %v", method, url, err)
	}
	return resp.StatusCode, out
}

func settingsSources(t *testing.T, ts *httptest.Server, cookie string) map[string]map[string]any {
	t.Helper()
	resp, data := doJSON(t, http.MethodGet, ts.URL+"/admin/api/settings", nil, map[string]string{"Cookie": cookie})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET settings = %d: %s", resp.StatusCode, data)
	}
	var payload struct {
		Settings []map[string]any `json:"settings"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	out := make(map[string]map[string]any, len(payload.Settings))
	for _, e := range payload.Settings {
		out[e["key"].(string)] = e
	}
	return out
}

// TestSettingsOverlayCycle proves the write→effective→delete→fallback loop:
// POST persists a DB row and hot-applies it, GET reports source=db, DELETE
// drops the row and the effective value falls back.
func TestSettingsOverlayCycle(t *testing.T) {
	ts, cookie, csrf := settingsTestServer(t)

	// Baseline: LOG_LEVEL ships at its default with no overlay.
	entries := settingsSources(t, ts, cookie)
	if entries["LOG_LEVEL"]["source"] != "default" {
		t.Fatalf("LOG_LEVEL source = %v, want default", entries["LOG_LEVEL"]["source"])
	}
	baseline := entries["LOG_LEVEL"]["value"]

	// Write: LOG_LEVEL is restart-only (the reload never reconfigures the
	// logger), so the POST persists but reports setting_restart_only.
	code, res := settingsDo(t, http.MethodPost, ts.URL+"/admin/api/settings", cookie, csrf,
		map[string]any{"key": "LOG_LEVEL", "value": "debug"})
	if code != http.StatusOK || res["ok"] != true {
		t.Fatalf("POST LOG_LEVEL = %d %v, want 200 ok", code, res)
	}
	if res["code"] != "setting_restart_only" {
		t.Errorf("POST code = %v, want setting_restart_only (logger is reload-proof)", res["code"])
	}
	if ro, _ := res["restart_only"].([]any); len(ro) != 1 || ro[0] != "LOG_LEVEL" {
		t.Errorf("restart_only = %v, want [LOG_LEVEL]", res["restart_only"])
	}

	// A live key still hot-applies with setting_saved.
	code, res = settingsDo(t, http.MethodPost, ts.URL+"/admin/api/settings", cookie, csrf,
		map[string]any{"key": "LOG_ACCESS", "value": false})
	if code != http.StatusOK || res["ok"] != true {
		t.Fatalf("POST LOG_ACCESS = %d %v, want 200 ok", code, res)
	}
	if res["code"] != "setting_saved" {
		t.Errorf("POST code = %v, want setting_saved (live key)", res["code"])
	}

	// Effective: the settings view AND the classic config view agree.
	entries = settingsSources(t, ts, cookie)
	if entries["LOG_LEVEL"]["value"] != "debug" || entries["LOG_LEVEL"]["source"] != "db" {
		t.Fatalf("LOG_LEVEL entry = %v, want value=debug source=db", entries["LOG_LEVEL"])
	}
	if entries["LOG_ACCESS"]["value"] != "false" || entries["LOG_ACCESS"]["source"] != "db" {
		t.Fatalf("LOG_ACCESS entry = %v, want value=false source=db", entries["LOG_ACCESS"])
	}
	resp, data := doJSON(t, http.MethodGet, ts.URL+"/admin/api/config", nil, map[string]string{"Cookie": cookie})
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(data), `"key":"LOG_LEVEL"`) {
		t.Fatalf("GET config = %d %s, want the effective view", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), `"value":"debug"`) {
		t.Errorf("config effective view missing debug LOG_LEVEL: %s", data)
	}

	// Restart-only keys persist too, flagged honestly.
	code, res = settingsDo(t, http.MethodPost, ts.URL+"/admin/api/settings", cookie, csrf,
		map[string]any{"key": "TRANSIENT_RETRIES", "value": "3"})
	if code != http.StatusOK || res["ok"] != true {
		t.Fatalf("POST TRANSIENT_RETRIES = %d %v, want 200 ok", code, res)
	}
	if res["code"] != "setting_restart_only" {
		t.Errorf("POST code = %v, want setting_restart_only", res["code"])
	}
	if ro, _ := res["restart_only"].([]any); len(ro) != 1 || ro[0] != "TRANSIENT_RETRIES" {
		t.Errorf("restart_only = %v, want [TRANSIENT_RETRIES]", res["restart_only"])
	}

	// Reset: the row drops and the value falls back to its default tier.
	code, res = settingsDo(t, http.MethodDelete, ts.URL+"/admin/api/settings/LOG_LEVEL", cookie, csrf, nil)
	if code != http.StatusOK || res["ok"] != true {
		t.Fatalf("DELETE LOG_LEVEL = %d %v, want 200 ok", code, res)
	}
	entries = settingsSources(t, ts, cookie)
	if entries["LOG_LEVEL"]["source"] != "default" || entries["LOG_LEVEL"]["value"] != baseline {
		t.Fatalf("LOG_LEVEL after reset = %v, want value=%v source=default", entries["LOG_LEVEL"], baseline)
	}

	// Second delete: nothing left to reset.
	code, res = settingsDo(t, http.MethodDelete, ts.URL+"/admin/api/settings/LOG_LEVEL", cookie, csrf, nil)
	if code != http.StatusNotFound {
		t.Errorf("DELETE missing overlay = %d %v, want 404", code, res)
	}
}

// TestSettingsPostRejects pins the validation gate: pool/password credentials
// with dedicated endpoints (AUTH_TOKENS, ADMIN_TOKEN), unknown keys, and
// unparseable values never reach the DB as knob writes. The remaining
// formerly-blocked keys persist since the env-to-DB migration (the DB holds
// secrets at mode 0600); their acceptance is pinned by
// TestSettingsPostAcceptsMigratedSecrets.
func TestSettingsPostRejects(t *testing.T) {
	ts, cookie, csrf := settingsTestServer(t)

	for _, key := range []string{"AUTH_TOKENS", "ADMIN_TOKEN", "DB_PATH"} {
		code, res := settingsDo(t, http.MethodPost, ts.URL+"/admin/api/settings", cookie, csrf,
			map[string]any{"key": key, "value": "x"})
		if code != http.StatusBadRequest {
			t.Errorf("POST %s = %d %v, want 400 (dedicated endpoint or unknown)", key, code, res)
		}
	}
	for _, kv := range [][2]string{
		{"NOPE_NOT_A_KEY", "x"},
		{"SAFE_MODE", "banana"},
		{"RATE_LIMIT_BURST", "lots"},
		{"RATE_LIMIT_PER_IP", "fast"},
		{"HTTP_READ_TIMEOUT", "soon"},
		{"LOG_LEVEL", ""},
		{"LOG_LEVEL", "   "},
	} {
		code, res := settingsDo(t, http.MethodPost, ts.URL+"/admin/api/settings", cookie, csrf,
			map[string]any{"key": kv[0], "value": kv[1]})
		if code != http.StatusBadRequest {
			t.Errorf("POST %v = %d %v, want 400", kv, code, res)
		}
	}

	// Rejections store nothing: the source map stays clean.
	entries := settingsSources(t, ts, cookie)
	for _, key := range []string{"SAFE_MODE", "RATE_LIMIT_BURST", "RATE_LIMIT_PER_IP", "HTTP_READ_TIMEOUT"} {
		if entries[key]["source"] == "db" {
			t.Errorf("%s source = db after rejected POSTs, want no overlay row", key)
		}
	}
}

// TestSettingsPostDurationRejectedBeforeTheOverlay pins the order of the
// instant-save gate: an unparseable duration knob is refused by
// config.ValidateSettingValue, which runs before the handler reads the DB
// overlay (and before it takes the save mutex), so the rejection costs no
// database round trip and leaves no row behind. Which gate answered is
// visible in the message: the overlay Load inside the handler wraps its
// errors in "Setting rejected: ...", while the pre-DB gate names the knob's
// own shape.
func TestSettingsPostDurationRejectedBeforeTheOverlay(t *testing.T) {
	ts, cookie, csrf, st := settingsStoreTestServer(t)

	code, res := settingsDo(t, http.MethodPost, ts.URL+"/admin/api/settings", cookie, csrf,
		map[string]any{"key": "QUEUE_WAIT", "value": "bogus"})
	if code != http.StatusBadRequest || res["code"] != "invalid_setting" {
		t.Fatalf("POST QUEUE_WAIT=bogus = %d %v, want 400 invalid_setting", code, res)
	}
	msg, _ := res["message"].(string)
	if !strings.Contains(msg, "Go duration") {
		t.Errorf("message = %q, want the duration shape named", msg)
	}
	if strings.Contains(msg, "Setting rejected") {
		t.Errorf("message = %q came from the post-overlay Load, want the pre-DB validate gate", msg)
	}
	if v, ok, err := st.GetSetting("config:QUEUE_WAIT"); err != nil || ok {
		t.Errorf("QUEUE_WAIT row = %q,%v,%v after a rejected POST, want no row", v, ok, err)
	}

	// The gate judges parseability only: the zero-tolerant values the loader
	// documents (a non-positive duration floors to the 30s default) still land.
	code, res = settingsDo(t, http.MethodPost, ts.URL+"/admin/api/settings", cookie, csrf,
		map[string]any{"key": "QUEUE_WAIT", "value": "0s"})
	if code != http.StatusOK || res["code"] != "setting_saved" {
		t.Fatalf("POST QUEUE_WAIT=0s = %d %v, want 200 setting_saved", code, res)
	}
	if v, ok, err := st.GetSetting("config:QUEUE_WAIT"); err != nil || !ok || v != "0s" {
		t.Errorf("QUEUE_WAIT row = %q,%v,%v after POST, want 0s,true,nil", v, ok, err)
	}
}

// TestSettingsPostAcceptsMigratedSecrets pins the env-to-DB migration's POST
// surface: API_KEYS, WEBHOOK_URL, UPSTREAM_BASE_URL, and AUTO_DISCOVER_TOKEN
// persist to the DB overlay and report source=db (fake values only).
func TestSettingsPostAcceptsMigratedSecrets(t *testing.T) {
	ts, cookie, csrf := settingsTestServer(t)

	for key, value := range map[string]string{
		"API_KEYS":            "fb-test-fake-client-1",
		"WEBHOOK_URL":         "https://example.invalid/hook",
		"UPSTREAM_BASE_URL":   "https://example.invalid",
		"AUTO_DISCOVER_TOKEN": "false",
	} {
		code, res := settingsDo(t, http.MethodPost, ts.URL+"/admin/api/settings", cookie, csrf,
			map[string]any{"key": key, "value": value})
		if code != http.StatusOK || res["ok"] != true {
			t.Errorf("POST %s = %d %v, want 200 ok (migrated secret persists)", key, code, res)
		}
	}
	entries := settingsSources(t, ts, cookie)
	for _, key := range []string{"API_KEYS", "WEBHOOK_URL", "UPSTREAM_BASE_URL", "AUTO_DISCOVER_TOKEN"} {
		if entries[key]["source"] != "db" {
			t.Errorf("%s source = %v, want db after POST", key, entries[key]["source"])
		}
	}
}

// TestSettingsDeleteCSRF: the DELETE row carries the same double-submit
// gate as every other state-changing admin route.
func TestSettingsDeleteCSRF(t *testing.T) {
	ts, cookie, _ := settingsTestServer(t)
	code, _ := settingsDo(t, http.MethodDelete, ts.URL+"/admin/api/settings/LOG_LEVEL", cookie, "", nil)
	if code != http.StatusForbidden {
		t.Errorf("DELETE without X-CSRF-Token = %d, want 403", code)
	}
}

// TestSettingsWithoutStore: a live-only gateway (DB failed at boot) still
// serves the effective view; mutations 503 instead of landing nowhere.
func TestSettingsWithoutStore(t *testing.T) {
	t.Chdir(t.TempDir())
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)

	resp, data := doJSON(t, http.MethodGet, ts.URL+"/admin/api/settings", nil, map[string]string{"Cookie": cookie})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET settings live-only = %d: %s", resp.StatusCode, data)
	}
	var payload struct {
		Settings []map[string]any `json:"settings"`
		Degraded bool             `json:"degraded"`
	}
	if err := json.Unmarshal(data, &payload); err != nil || len(payload.Settings) == 0 {
		t.Fatalf("GET settings payload = %s, want a non-empty settings array", data)
	}
	if !payload.Degraded {
		t.Errorf("GET settings live-only degraded = false, want true (nil store serves file/env/default read-only)")
	}

	code, res := settingsDo(t, http.MethodPost, ts.URL+"/admin/api/settings", cookie, "csrf",
		map[string]any{"key": "LOG_LEVEL", "value": "debug"})
	if code != http.StatusServiceUnavailable {
		t.Errorf("POST live-only = %d %v, want 503", code, res)
	}
}

// TestSettingsPostRestartOnlyMatrix pins the logger/listener review finding
// end to end: every restart-only key persists through POST but reports
// setting_restart_only (never setting_saved), and GET flags the row
// restart_only with source=db.
func TestSettingsPostRestartOnlyMatrix(t *testing.T) {
	ts, cookie, csrf := settingsTestServer(t)
	for key, value := range map[string]string{
		"LOG_LEVEL":   "debug",
		"LOG_FORMAT":  "json",
		"LOG_FILE":    "proxy.log",
		"LISTEN_ADDR": "127.0.0.1:3458",
	} {
		code, res := settingsDo(t, http.MethodPost, ts.URL+"/admin/api/settings", cookie, csrf,
			map[string]any{"key": key, "value": value})
		if code != http.StatusOK || res["ok"] != true {
			t.Errorf("POST %s = %d %v, want 200 ok", key, code, res)
			continue
		}
		if res["code"] != "setting_restart_only" {
			t.Errorf("POST %s code = %v, want setting_restart_only", key, res["code"])
		}
		if ro, _ := res["restart_only"].([]any); len(ro) != 1 || ro[0] != key {
			t.Errorf("POST %s restart_only = %v, want [%s]", key, res["restart_only"], key)
		}
	}
	entries := settingsSources(t, ts, cookie)
	for _, key := range []string{"LOG_LEVEL", "LOG_FORMAT", "LOG_FILE", "LISTEN_ADDR"} {
		if entries[key]["source"] != "db" {
			t.Errorf("%s source = %v, want db after POST", key, entries[key]["source"])
		}
	}
}

// TestSettingsPostNumericCoercion pins JSON-number handling for int keys:
// an integral float64 is exactly the int the caller meant (30.0 persists as
// "30" and hot-applies), while a non-integral float rejects with a message
// that names the number instead of the generic shape error.
func TestSettingsPostNumericCoercion(t *testing.T) {
	ts, cookie, csrf := settingsTestServer(t)

	code, res := settingsDo(t, http.MethodPost, ts.URL+"/admin/api/settings", cookie, csrf,
		map[string]any{"key": "RATE_LIMIT_BURST", "value": float64(30)})
	if code != http.StatusOK || res["ok"] != true || res["code"] != "setting_saved" {
		t.Fatalf("POST int key with 30.0 = %d %v, want 200 setting_saved", code, res)
	}
	entries := settingsSources(t, ts, cookie)
	if entries["RATE_LIMIT_BURST"]["value"] != "30" || entries["RATE_LIMIT_BURST"]["source"] != "db" {
		t.Fatalf("RATE_LIMIT_BURST entry = %v, want value=30 source=db", entries["RATE_LIMIT_BURST"])
	}

	code, res = settingsDo(t, http.MethodPost, ts.URL+"/admin/api/settings", cookie, csrf,
		map[string]any{"key": "RATE_LIMIT_BURST", "value": 30.5})
	if code != http.StatusBadRequest {
		t.Fatalf("POST int key with 30.5 = %d %v, want 400", code, res)
	}
	if msg, _ := res["message"].(string); !strings.Contains(msg, "non-integral") {
		t.Errorf("POST 30.5 message = %q, want it to name the non-integral number", msg)
	}

	// The rejected write stores nothing: the effective value is untouched.
	entries = settingsSources(t, ts, cookie)
	if entries["RATE_LIMIT_BURST"]["value"] != "30" {
		t.Errorf("RATE_LIMIT_BURST after rejected POST = %v, want value=30", entries["RATE_LIMIT_BURST"])
	}
}

// TestSettingsDegradedFlag pins nil-store honesty on the healthy side too:
// a store-backed gateway reports degraded:false alongside the full catalog.
func TestSettingsDegradedFlag(t *testing.T) {
	ts, cookie, _ := settingsTestServer(t)
	resp, data := doJSON(t, http.MethodGet, ts.URL+"/admin/api/settings", nil, map[string]string{"Cookie": cookie})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET settings = %d: %s", resp.StatusCode, data)
	}
	var payload struct {
		Settings []map[string]any `json:"settings"`
		Degraded bool             `json:"degraded"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	if payload.Degraded {
		t.Error("GET settings store-backed degraded = true, want false")
	}
	if len(payload.Settings) == 0 {
		// The degraded/get split must never shrink the catalog: keep 200 +
		// the full effective view in both states.
		t.Error("GET settings store-backed returned an empty catalog")
	}
}

// migrateTestCookie logs into a store-backed gateway and returns the Cookie
// header value carrying the session (mirrors settingsTestServer).
func migrateTestCookie(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	resp := postLogin(t, ts.URL+"/admin/login", "secret")
	defer func() { _ = resp.Body.Close() }()
	var admin, csrf string
	for _, c := range resp.Cookies() {
		if c.Name == "fb_admin" {
			admin = c.Name + "=" + c.Value
		}
		if c.Name == "fb_csrf" {
			csrf = c.Value
		}
	}
	if admin == "" || csrf == "" {
		t.Fatal("login did not set fb_admin + fb_csrf cookies")
	}
	return admin + "; fb_csrf=" + csrf
}

// migratePayload fetches GET /admin/api/settings and returns its migrate
// object (nil when the gateway serves live-only without a store).
func migratePayload(t *testing.T, ts *httptest.Server, cookie string) map[string]any {
	t.Helper()
	code, out := settingsDo(t, http.MethodGet, ts.URL+"/admin/api/settings", cookie, "", nil)
	if code != http.StatusOK {
		t.Fatalf("GET settings = %d: %v", code, out)
	}
	raw, ok := out["migrate"]
	if !ok || raw == nil {
		return nil
	}
	mig, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("migrate = %T, want an object", raw)
	}
	return mig
}

// TestSettingsMigratePayloadShape pins the migrate-status readers on the
// settings payload: from_version, applied[], noop (plus to_version, fresh,
// marker) ride GET /admin/api/settings from the store's in-memory Open
// report plus the marker row — read-cheap, no per-request migration work.
func TestSettingsMigratePayloadShape(t *testing.T) {
	t.Chdir(t.TempDir())
	dbPath := filepath.Join(t.TempDir(), "migrate.db")
	st, ms, err := store.OpenWithStatus(dbPath)
	if err != nil {
		t.Fatalf("OpenWithStatus: %v", err)
	}
	if ms.FromVersion != 0 || len(ms.Applied) != 5 || !ms.Fresh || ms.Noop {
		t.Fatalf("fresh status = %+v, want {From:0 Applied:x5 Fresh:true Noop:false}", ms)
	}
	srv, _ := server.NewTestServerStack(t, nil, []*testutil.MockUpstream{testutil.NewMock()},
		func(c *config.Config) { c.AdminToken = "secret" }, nil, nil, server.WithHistory(st))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(func() { _ = st.Close() })
	cookie := migrateTestCookie(t, ts)

	// Fresh boot, marker-less: the report names the detected generation and
	// the applied chain; the marker is absent until the env import runs.
	mig := migratePayload(t, ts, cookie)
	if mig == nil {
		t.Fatal("store-backed settings has no migrate object, want the boot report")
	}
	if mig["from_version"] != 0.0 || mig["to_version"] != 5.0 {
		t.Errorf("migrate from/to = %v/%v, want 0/5", mig["from_version"], mig["to_version"])
	}
	applied, ok := mig["applied"].([]any)
	if !ok || len(applied) != 5 {
		t.Fatalf("migrate applied = %v, want the 5-step chain", mig["applied"])
	}
	for i, v := range applied {
		if v != float64(i+1) {
			t.Errorf("migrate applied[%d] = %v, want %d", i, v, i+1)
		}
	}
	if mig["fresh"] != true || mig["marker"] != false || mig["noop"] != false {
		t.Errorf("migrate fresh/marker/noop = %v/%v/%v, want true/false/false", mig["fresh"], mig["marker"], mig["noop"])
	}

	// The env import flips the marker (no server restart, same handle).
	if err := st.SetSetting(config.MigrationMarkerRow, config.MigrationMarkerValue); err != nil {
		t.Fatalf("set marker: %v", err)
	}
	if mig := migratePayload(t, ts, cookie); mig["marker"] != true {
		t.Errorf("migrate marker = %v after the import, want true", mig["marker"])
	}

	// A re-boot converges to the strict no-op shape: applied encodes [] and
	// the marker stays set.
	ts.Close()
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	st2, ms2, err := store.OpenWithStatus(dbPath)
	if err != nil {
		t.Fatalf("re-OpenWithStatus: %v", err)
	}
	t.Cleanup(func() { _ = st2.Close() })
	if ms2.Fresh || len(ms2.Applied) != 0 || !ms2.Noop {
		t.Fatalf("re-boot status = %+v, want {Fresh:false Applied:[] Noop:true}", ms2)
	}
	srv2, _ := server.NewTestServerStack(t, nil, []*testutil.MockUpstream{testutil.NewMock()},
		func(c *config.Config) { c.AdminToken = "secret" }, nil, nil, server.WithHistory(st2))
	ts2 := httptest.NewServer(srv2.Handler())
	t.Cleanup(ts2.Close)
	mig2 := migratePayload(t, ts2, migrateTestCookie(t, ts2))
	if mig2["marker"] != true || mig2["noop"] != true {
		t.Errorf("re-boot migrate marker/noop = %v/%v, want true/true", mig2["marker"], mig2["noop"])
	}
	applied2, ok := mig2["applied"].([]any)
	if !ok || applied2 == nil || len(applied2) != 0 {
		t.Errorf("re-boot migrate applied = %#v, want [] (never null)", mig2["applied"])
	}
}

// TestSettingsMigrateAbsentLiveOnly pins the degraded side: without a store
// the settings payload carries no migrate object (there are no boot facts).
func TestSettingsMigrateAbsentLiveOnly(t *testing.T) {
	t.Chdir(t.TempDir())
	ts := dashboardServer(t, "secret", nil)
	t.Cleanup(ts.Close)
	if mig := migratePayload(t, ts, authedCookie(t, ts)); mig != nil {
		t.Errorf("live-only migrate = %v, want absent", mig)
	}
}

// TestSettingsPostSecureCookiesEnvOnly pins the owner decision: the cookie
// reader never consults the overlay, so a direct knob write 400s with a
// pointer to the environment/.env instead of persisting an inert row.
func TestSettingsPostSecureCookiesEnvOnly(t *testing.T) {
	ts, cookie, csrf := settingsTestServer(t)
	code, res := settingsDo(t, http.MethodPost, ts.URL+"/admin/api/settings", cookie, csrf,
		map[string]any{"key": "ADMIN_FORCE_SECURE_COOKIES", "value": "true"})
	if code != http.StatusBadRequest {
		t.Fatalf("POST ADMIN_FORCE_SECURE_COOKIES = %d %v, want 400", code, res)
	}
	if msg, _ := res["message"].(string); !strings.Contains(msg, ".env") {
		t.Errorf("POST ADMIN_FORCE_SECURE_COOKIES message = %q, want a pointer to the environment/.env", msg)
	}
	entries := settingsSources(t, ts, cookie)
	if entries["ADMIN_FORCE_SECURE_COOKIES"]["source"] == "db" {
		t.Error("ADMIN_FORCE_SECURE_COOKIES source = db after rejected POST, want no overlay row")
	}
}

// TestSettingsPostEnvShadowNote pins env-shadow honesty: with the key pinned
// by the process environment, the overlay row still persists but the message
// says the effective value still comes from the environment, and GET keeps
// reporting source=env.
func TestSettingsPostEnvShadowNote(t *testing.T) {
	t.Setenv("SAFE_MODE", "true")
	ts, cookie, csrf := settingsTestServer(t)
	code, res := settingsDo(t, http.MethodPost, ts.URL+"/admin/api/settings", cookie, csrf,
		map[string]any{"key": "SAFE_MODE", "value": "false"})
	if code != http.StatusOK || res["ok"] != true {
		t.Fatalf("POST env-pinned SAFE_MODE = %d %v, want 200 ok", code, res)
	}
	if msg, _ := res["message"].(string); !strings.Contains(msg, "Overridden by process env") {
		t.Errorf("POST env-pinned message = %q, want the process-env shadow note", msg)
	}
	entries := settingsSources(t, ts, cookie)
	if entries["SAFE_MODE"]["source"] != "env" {
		t.Errorf("SAFE_MODE source = %v, want env (process env beats the saved row)", entries["SAFE_MODE"]["source"])
	}
}

// TestSettingsDurationEchoStable proves the POST→GET echo contract for saved
// rows: GET /admin/api/settings reports the saved literal for db-tier rows,
// not the Go-normalized effective form. time.Duration.String rewrites "60s"
// as "1m0s", which the dashboard could not round-trip — the Pool Strategy
// badge read Custom after one Balance tap and only settled on the second.
// File/env tiers keep the normalized effective value (backward compatible).
func TestSettingsDurationEchoStable(t *testing.T) {
	ts, cookie, csrf := settingsTestServer(t)

	post := func(key, value string) {
		t.Helper()
		code, res := settingsDo(t, http.MethodPost, ts.URL+"/admin/api/settings", cookie, csrf,
			map[string]any{"key": key, "value": value})
		if code != http.StatusOK || res["ok"] != true {
			t.Fatalf("POST %s = %d %v, want 200 ok", key, code, res)
		}
	}
	echo := func(key, want string) {
		t.Helper()
		e := settingsSources(t, ts, cookie)[key]
		if e["source"] != "db" {
			t.Errorf("%s source = %v, want db", key, e["source"])
		}
		if e["value"] != want {
			t.Errorf("%s GET value = %q, want saved literal %q", key, e["value"], want)
		}
	}

	// The MASQ owned keys (strict spill posture).
	post("SLOTS_PER_ACCOUNT", "2")
	post("MAX_SPILL_ACCOUNTS", "0")
	post("QUEUE_WAIT", "60s")
	post("QUEUE_DEPTH", "16")
	for key, want := range map[string]string{
		"SLOTS_PER_ACCOUNT":  "2",
		"MAX_SPILL_ACCOUNTS": "0", "QUEUE_WAIT": "60s",
		"QUEUE_DEPTH": "16",
	} {
		echo(key, want)
	}

	// Drain↔Balance flip-flop: every literal round-trips, never the
	// normalized echo ("5m0s"/"1m0s").
	post("QUEUE_WAIT", "300s")
	post("QUEUE_DEPTH", "1024")
	echo("QUEUE_WAIT", "300s")
	echo("QUEUE_DEPTH", "1024")
	post("QUEUE_WAIT", "60s")
	post("QUEUE_DEPTH", "16")
	echo("QUEUE_WAIT", "60s")
	echo("QUEUE_DEPTH", "16")
	// Bool spellings still normalize to one display form.
	post("SAFE_MODE", "on")
	echo("SAFE_MODE", "true")

	// Knob-chain agreement: the raw db-tier echo ("60s") and the live
	// effective rendering (Go-normalized, e.g. "1m0s") denote the same
	// duration. The settings GET stays echo-stable for the dashboard while
	// GET /admin/api/config keeps serving the normalized effective value
	// (file/env tiers and the settings store refresh read it); both must
	// parse to 60s or the badge and the store would hold two truths.
	resp, data := doJSON(t, http.MethodGet, ts.URL+"/admin/api/config", nil, map[string]string{"Cookie": cookie})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET config = %d: %s", resp.StatusCode, data)
	}
	var cfgPayload struct {
		Effective []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"effective"`
	}
	if err := json.Unmarshal(data, &cfgPayload); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	effective := map[string]string{}
	for _, kv := range cfgPayload.Effective {
		effective[kv.Key] = kv.Value
	}
	if d, err := time.ParseDuration(effective["QUEUE_WAIT"]); err != nil || d != 60*time.Second {
		t.Errorf("config effective QUEUE_WAIT = %q (parse %v), want a 60s duration agreeing with the raw echo", effective["QUEUE_WAIT"], d)
	}
	if effective["QUEUE_DEPTH"] != "16" {
		t.Errorf("config effective QUEUE_DEPTH = %q, want 16", effective["QUEUE_DEPTH"])
	}
	// The live tokens surface (10s hot poll + SSE store refresh) stays
	// healthy after the preset writes.
	resp, _ = doJSON(t, http.MethodGet, ts.URL+"/admin/api/tokens?view=live", nil, map[string]string{"Cookie": cookie})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET tokens?view=live = %d, want 200", resp.StatusCode)
	}
}

// TestSettingsGetMasksSecrets pins the secret-echo fix: for every Secret
// catalog def, GET /admin/api/settings reports the masked Data() rendering
// (counts / set-unset words), never the raw DB-overlay literal — including
// migrated AUTH_TOKENS/ADMIN_TOKEN/API_KEYS rows and the overlay-writable
// WEBHOOK_URL. Non-secret rows keep the echo-stable literal contract, and
// DELETE/Reset still clears every secret row. Fake literals only.
func TestSettingsGetMasksSecrets(t *testing.T) {
	ts, cookie, csrf, st := settingsStoreTestServer(t)

	// Seed overlay rows directly (migration-shaped raw literals), bypassing
	// the POST gate that routes AUTH_TOKENS/ADMIN_TOKEN to dedicated
	// endpoints — the env-to-DB migration writes these rows straight to
	// the table. A reload hot-applies them into the live snapshot (the
	// reload bearer gate reads the pre-reload snapshot, still "secret").
	seeds := map[string]string{
		"AUTH_TOKENS": "fb-test-fake-token-1,fb-test-fake-token-2",
		"ADMIN_TOKEN": "fb-test-fake-admin-1",
		"API_KEYS":    "fb-test-fake-client-1",
		"WEBHOOK_URL": "https://example.invalid/hook",
	}
	for key, raw := range seeds {
		if err := st.SetSetting(config.OverlayRowKey(key), raw); err != nil {
			t.Fatalf("SetSetting %s: %v", key, err)
		}
	}
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/admin/reload", nil,
		map[string]string{"Authorization": "Bearer secret"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reload after seeding secrets = %d, want 200: %s", resp.StatusCode, data)
	}

	// The masked Data() renderings every secret row must carry.
	masked := map[string]string{
		"AUTH_TOKENS": "2 token(s)",
		"API_KEYS":    "1 key(s)",
		"ADMIN_TOKEN": "set",
		"WEBHOOK_URL": "set",
	}
	// Credential fragments that must never appear in a display value.
	fragments := map[string]string{
		"AUTH_TOKENS": "fb-test-fake-token",
		"ADMIN_TOKEN": "fb-test-fake-admin",
		"API_KEYS":    "fb-test-fake-client",
		"WEBHOOK_URL": "example.invalid",
	}
	entries := settingsSources(t, ts, cookie)
	for key, raw := range seeds {
		e := entries[key]
		if e["source"] != "db" {
			t.Errorf("%s source = %v, want db", key, e["source"])
		}
		if e["secret"] != true {
			t.Errorf("%s secret = %v, want true", key, e["secret"])
		}
		if e["value"] != masked[key] {
			t.Errorf("%s GET value = %q, want masked %q", key, e["value"], masked[key])
		}
		if v, _ := e["value"].(string); v == raw || strings.Contains(v, fragments[key]) {
			t.Errorf("%s GET value = %q, carries credential material", key, v)
		}
	}

	// Non-secret control: the echo-stable contract is intact — a db-tier
	// row still reports its saved literal.
	code, res := settingsDo(t, http.MethodPost, ts.URL+"/admin/api/settings", cookie, csrf,
		map[string]any{"key": "QUEUE_WAIT", "value": "60s"})
	if code != http.StatusOK || res["ok"] != true {
		t.Fatalf("POST QUEUE_WAIT = %d %v, want 200 ok", code, res)
	}
	if e := settingsSources(t, ts, cookie)["QUEUE_WAIT"]; e["value"] != "60s" || e["source"] != "db" {
		t.Errorf("QUEUE_WAIT = %v, want echo-stable 60s from db", e)
	}

	// Reset still clears: DELETE drops each row and the display value falls
	// back off the db tier with no credential residue. ADMIN_TOKEN goes
	// last — its row is verified at the store so no claim about the
	// post-reset session outlives the credential it was issued under.
	for _, key := range []string{"API_KEYS", "WEBHOOK_URL", "AUTH_TOKENS"} {
		code, res := settingsDo(t, http.MethodDelete, ts.URL+"/admin/api/settings/"+key, cookie, csrf, nil)
		if code != http.StatusOK || res["ok"] != true {
			t.Errorf("DELETE %s = %d %v, want 200 ok", key, code, res)
			continue
		}
		e := settingsSources(t, ts, cookie)[key]
		if e["source"] == "db" {
			t.Errorf("%s source = db after DELETE, want fallback", key)
		}
		if v, _ := e["value"].(string); v == seeds[key] || strings.Contains(v, fragments[key]) {
			t.Errorf("%s GET value after DELETE = %q, still carries the cleared credential", key, v)
		}
	}
	code, res = settingsDo(t, http.MethodDelete, ts.URL+"/admin/api/settings/ADMIN_TOKEN", cookie, csrf, nil)
	if code != http.StatusOK || res["ok"] != true {
		t.Errorf("DELETE ADMIN_TOKEN = %d %v, want 200 ok", code, res)
	} else if v, ok, err := st.GetSetting(config.OverlayRowKey("ADMIN_TOKEN")); err != nil || ok || v != "" {
		t.Errorf("ADMIN_TOKEN row after DELETE = %q %v %v, want it gone", v, ok, err)
	}
}

// TestSettingsGetMasksSecretsOpenMode pins the same masking for the
// unauthenticated loopback tier: open mode (ADMIN_TOKEN unset) serves GET
// /admin/api/settings without a session, and seeded secret rows still
// render masked there. No reload: any fresh load reverts an unset
// ADMIN_TOKEN to the factory default and would close open mode by design,
// so this exercises the pre-sync snapshot — the masking holds either way
// because secret rows never take the raw-echo branch, whatever the
// snapshot counts are.
func TestSettingsGetMasksSecretsOpenMode(t *testing.T) {
	t.Chdir(t.TempDir())
	st, err := store.Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv, _ := server.NewTestServerStack(t, nil, []*testutil.MockUpstream{testutil.NewMock()},
		func(c *config.Config) { c.AdminToken = "" }, nil, nil, server.WithHistory(st))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	seeds := map[string]string{
		"AUTH_TOKENS": "fb-test-fake-token-1,fb-test-fake-token-2",
		"API_KEYS":    "fb-test-fake-client-1",
		"WEBHOOK_URL": "https://example.invalid/hook",
	}
	fragments := map[string]string{
		"AUTH_TOKENS": "fb-test-fake-token",
		"API_KEYS":    "fb-test-fake-client",
		"WEBHOOK_URL": "example.invalid",
	}
	for key, raw := range seeds {
		if err := st.SetSetting(config.OverlayRowKey(key), raw); err != nil {
			t.Fatalf("SetSetting %s: %v", key, err)
		}
	}

	// No session cookie: the masking branch runs after dashboardAuth and is
	// auth-independent. Values are asserted as masked shapes (counts /
	// set-unset words), never the raw literal — exact counts depend on the
	// pre-sync snapshot, which this path deliberately does not refresh.
	entries := settingsSources(t, ts, "")
	for key, raw := range seeds {
		e := entries[key]
		if e["source"] != "db" {
			t.Errorf("open-mode %s source = %v, want db", key, e["source"])
		}
		if e["secret"] != true {
			t.Errorf("open-mode %s secret = %v, want true", key, e["secret"])
		}
		v, _ := e["value"].(string)
		if v == raw || strings.Contains(v, fragments[key]) {
			t.Errorf("open-mode %s GET value = %q, carries credential material", key, v)
		}
		if !maskedSecretShape(v) {
			t.Errorf("open-mode %s GET value = %q, want a masked counts/set-unset rendering", key, v)
		}
	}
}

// maskedSecretShape reports whether v looks like a masked secret rendering
// (the Data() counts / set-unset words), never a credential literal.
func maskedSecretShape(v string) bool {
	if v == "set" || v == "unset" {
		return true
	}
	for _, suffix := range []string{" token(s)", " key(s)"} {
		if n, ok := strings.CutSuffix(v, suffix); ok {
			for _, r := range n {
				if r < '0' || r > '9' {
					return false
				}
			}
			return n != ""
		}
	}
	return false
}

func TestSettingsPostMaturityKeys(t *testing.T) {
	ts, cookie, csrf := settingsTestServer(t)

	// MATURITY_ENABLED
	code, res := settingsDo(t, http.MethodPost, ts.URL+"/admin/api/settings", cookie, csrf,
		map[string]any{"key": "MATURITY_ENABLED", "value": "false"})
	if code != http.StatusOK || res["ok"] != true {
		t.Fatalf("POST MATURITY_ENABLED = %d %v, want 200 ok", code, res)
	}

	// MATURITY_TARGET_DAYS
	code, res = settingsDo(t, http.MethodPost, ts.URL+"/admin/api/settings", cookie, csrf,
		map[string]any{"key": "MATURITY_TARGET_DAYS", "value": "14"})
	if code != http.StatusOK || res["ok"] != true {
		t.Fatalf("POST MATURITY_TARGET_DAYS = %d %v, want 200 ok", code, res)
	}

	// MATURITY_TOUCH_MODEL
	code, res = settingsDo(t, http.MethodPost, ts.URL+"/admin/api/settings", cookie, csrf,
		map[string]any{"key": "MATURITY_TOUCH_MODEL", "value": "deepseek/deepseek-v4-flash"})
	if code != http.StatusOK || res["ok"] != true {
		t.Fatalf("POST MATURITY_TOUCH_MODEL = %d %v, want 200 ok", code, res)
	}

	entries := settingsSources(t, ts, cookie)
	if entries["MATURITY_ENABLED"]["value"] != "false" {
		t.Errorf("MATURITY_ENABLED value = %v, want false", entries["MATURITY_ENABLED"]["value"])
	}
	if entries["MATURITY_TARGET_DAYS"]["value"] != "14" {
		t.Errorf("MATURITY_TARGET_DAYS value = %v, want 14", entries["MATURITY_TARGET_DAYS"]["value"])
	}
	if entries["MATURITY_TOUCH_MODEL"]["value"] != "deepseek/deepseek-v4-flash" {
		t.Errorf("MATURITY_TOUCH_MODEL value = %v, want deepseek/deepseek-v4-flash", entries["MATURITY_TOUCH_MODEL"]["value"])
	}
}
