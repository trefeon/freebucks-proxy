package dashboard_test

// RED: logs rollup + export/import handlers. Fails until dashboard_rollup.go
// lands (APILogsRollup/APILogsExport/APILogsImport + manifest/wire/routes).

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/dashboard"
	"freebuff-proxy/backend/internal/store"
)

func seedRollupStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "rollup.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rows := []store.RequestRecord{
		{ReqID: "r1", TS: 1_000, Endpoint: "/v1/chat/completions", Model: "m/a", TokenIdx: 0, Status: "ok", TTFBms: 100},
		{ReqID: "r2", TS: 2_000, Endpoint: "/v1/chat/completions", Model: "m/a", TokenIdx: 1, Status: "error", TTFBms: 200, Err: "upstream"},
		{ReqID: "r3", TS: 3_000, Endpoint: "/v1/chat/completions", Model: "m/b", TokenIdx: -1, Status: "ok", TTFBms: 300},
		{ReqID: "r4", TS: 4_000, Endpoint: "/v1/chat/completions", Model: "m/b", TokenIdx: -1, Status: "error", TTFBms: 400, Err: "auth"},
		{ReqID: "r5", TS: 5_000, Endpoint: "/v1/chat/completions", Model: "m/a", TokenIdx: 0, Status: "ok", TTFBms: 500},
	}
	for _, rec := range rows {
		if err := st.RecordRequest(rec); err != nil {
			t.Fatalf("RecordRequest %s: %v", rec.ReqID, err)
		}
	}
	if err := st.AppendLogs([]store.LogEntry{
		{TS: 1_500, Level: "info", Msg: "chat done", ReqID: "r1"},
		{TS: 2_500, Level: "error", Msg: "chat failed", ReqID: "r2"},
	}); err != nil {
		t.Fatalf("AppendLogs: %v", err)
	}
	return st
}

func rollupMux(st *store.Store) *httptest.Server {
	cfg := &config.Config{}
	d := dashboard.New(func() *config.Config { return cfg }, nil, nil, slog.Default(), nil, dashboard.WithHistory(st))
	mux := http.NewServeMux()
	mux.Handle("GET /admin/api/logs/rollup", http.HandlerFunc(d.APILogsRollup))
	mux.Handle("GET /admin/api/logs/export", http.HandlerFunc(d.APILogsExport))
	mux.Handle("POST /admin/api/logs/import", http.HandlerFunc(d.APILogsImport))
	return httptest.NewServer(mux)
}

func getRollupJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec,noctx // hermetic httptest server
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, out
}

func TestLogsRollupEndpoint(t *testing.T) {
	ts := rollupMux(seedRollupStore(t))
	t.Cleanup(ts.Close)
	code, got := getRollupJSON(t, ts.URL+"/admin/api/logs/rollup?since=0&until=6000&bucket_ms=2000")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if got["enabled"] != true {
		t.Fatalf("enabled = %v, want true", got["enabled"])
	}
	if got["total"] != 5.0 || got["ok"] != 3.0 || got["errors"] != 2.0 {
		t.Fatalf("counts = %v/%v/%v, want 5/3/2", got["total"], got["ok"], got["errors"])
	}
	if byModel, ok := got["by_model"].([]any); !ok || len(byModel) != 2 {
		t.Fatalf("by_model = %v, want 2 groups", got["by_model"])
	}
	if buckets, ok := got["buckets"].([]any); !ok || len(buckets) != 3 {
		t.Fatalf("buckets = %v, want 3", got["buckets"])
	}
	if _, ok := got["total_latency_ms"]; ok {
		t.Fatal("rollup must not invent total-latency fields")
	}
	if _, ok := got["http_codes"]; ok {
		t.Fatal("rollup must not invent http-code fields")
	}
}

func TestLogsRollupLiveOnly(t *testing.T) {
	cfg := &config.Config{}
	d := dashboard.New(func() *config.Config { return cfg }, nil, nil, slog.Default(), nil)
	rec := httptest.NewRecorder()
	d.APILogsRollup(rec, httptest.NewRequest(http.MethodGet, "/admin/api/logs/rollup", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["enabled"] != false {
		t.Fatalf("enabled = %v, want false", got["enabled"])
	}
}

func TestLogsRollupRejectsInvalid(t *testing.T) {
	ts := rollupMux(seedRollupStore(t))
	t.Cleanup(ts.Close)
	for _, q := range []string{"?since=5000&until=5000", "?since=0&until=6000&bucket_ms=0", "?since=-5&until=6000"} {
		resp, err := http.Get(ts.URL + "/admin/api/logs/rollup" + q) //nolint:gosec,noctx // hermetic
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET rollup%s status = %d, want 400", q, resp.StatusCode)
		}
	}
}

func TestLogsExportImportRoundTrip(t *testing.T) {
	st := seedRollupStore(t)
	ts := rollupMux(st)
	t.Cleanup(ts.Close)

	export := func() map[string]any {
		t.Helper()
		resp, err := http.Get(ts.URL + "/admin/api/logs/export?since=0&until=6000") //nolint:gosec,noctx // hermetic
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("export status = %d, want 200", resp.StatusCode)
		}
		var doc map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}

	rollup := func() string {
		t.Helper()
		_, got := getRollupJSON(t, ts.URL+"/admin/api/logs/rollup?since=0&until=6000&bucket_ms=2000")
		raw, _ := json.Marshal(got)
		return string(raw)
	}

	before := rollup()
	doc := export()
	if doc["version"] != 1.0 {
		t.Fatalf("version = %v, want 1", doc["version"])
	}
	if recs, ok := doc["request_records"].([]any); !ok || len(recs) != 5 {
		t.Fatalf("request_records = %v, want 5 rows", doc["request_records"])
	}
	if logs, ok := doc["log_entries"].([]any); !ok || len(logs) != 2 {
		t.Fatalf("log_entries = %v, want 2 rows", doc["log_entries"])
	}

	// Wipe both tables, prove the rollup goes empty, re-import, prove
	// byte-identical rollup output.
	if err := st.Purge(1<<62, 1<<62, 1<<62, 1<<62); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if _, got := getRollupJSON(t, ts.URL+"/admin/api/logs/rollup?since=0&until=6000&bucket_ms=2000"); got["total"] != 0.0 {
		t.Fatalf("post-wipe total = %v, want 0", got["total"])
	}
	raw, _ := json.Marshal(doc)
	resp, err := http.Post(ts.URL+"/admin/api/logs/import", "application/json", bytes.NewReader(raw)) //nolint:noctx // hermetic
	if err != nil {
		t.Fatal(err)
	}
	var rep map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		_ = resp.Body.Close()
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("import status = %d, want 200 (%v)", resp.StatusCode, rep)
	}
	if rep["imported"] != 7.0 || rep["skipped_duplicate"] != 0.0 || rep["rejected"] != 0.0 {
		t.Fatalf("report = %v, want imported 7 skipped 0 rejected 0", rep)
	}
	if after := rollup(); after != before {
		t.Fatalf("rollup mismatch:\nbefore %s\nafter  %s", before, after)
	}

	// Duplicate import is a data no-op: all rows skip, rollup unchanged.
	resp2, err := http.Post(ts.URL+"/admin/api/logs/import", "application/json", bytes.NewReader(raw)) //nolint:noctx // hermetic
	if err != nil {
		t.Fatal(err)
	}
	var rep2 map[string]any
	if err := json.NewDecoder(resp2.Body).Decode(&rep2); err != nil {
		_ = resp2.Body.Close()
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if rep2["imported"] != 0.0 || rep2["skipped_duplicate"] != 7.0 {
		t.Fatalf("dup report = %v, want imported 0 skipped 7", rep2)
	}
	if after := rollup(); after != before {
		t.Fatalf("post-dup rollup mismatch:\nbefore %s\nafter  %s", before, after)
	}
}
func TestLogsImportOverCap(t *testing.T) {
	ts := rollupMux(seedRollupStore(t))
	t.Cleanup(ts.Close)
	// 64MB cap + slack: the MaxBytesReader must trip a 413 before JSON
	// parsing, so padding (not rows) proves the limiter.
	body := `{"version":1,"request_records":[` + strings.Repeat(" ", (64<<20)+1024)
	resp, err := http.Post(ts.URL+"/admin/api/logs/import", "application/json", strings.NewReader(body)) //nolint:noctx // hermetic
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-cap status = %d, want 413", resp.StatusCode)
	}
}

func TestLogsImportRejects(t *testing.T) {
	ts := rollupMux(seedRollupStore(t))
	t.Cleanup(ts.Close)
	post := func(body string) int {
		t.Helper()
		resp, err := http.Post(ts.URL+"/admin/api/logs/import", "application/json", strings.NewReader(body)) //nolint:noctx // hermetic
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(`{"version":2,"request_records":[],"log_entries":[]}`); code != http.StatusBadRequest {
		t.Errorf("wrong version status = %d, want 400", code)
	}
	if code := post(`{"version":1,"request_records":[{"req_id":"x","ts":1,"endpoint":"e","status":"bogus"}],"log_entries":[]}`); code != http.StatusOK {
		t.Errorf("bad row status = %d, want 200 with rejected count", code)
	} else {
		resp, err := http.Post(ts.URL+"/admin/api/logs/import", "application/json", strings.NewReader(`{"version":1,"request_records":[{"req_id":"x","ts":1,"endpoint":"e","status":"bogus"}],"log_entries":[]}`)) //nolint:noctx // hermetic
		if err != nil {
			t.Fatal(err)
		}
		var rep map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&rep)
		_ = resp.Body.Close()
		if rep["rejected"] != 1.0 || rep["imported"] != 0.0 {
			t.Errorf("bad-row report = %v, want rejected 1 imported 0", rep)
		}
	}
	if code := post(`{"version":1,`); code != http.StatusBadRequest {
		t.Errorf("truncated JSON status = %d, want 400", code)
	}
}

func TestLogsRollupRoutesAreSensitive(t *testing.T) {
	want := map[string]bool{
		"GET /admin/api/logs/rollup":  false,
		"GET /admin/api/logs/export":  false,
		"POST /admin/api/logs/import": false,
	}
	for _, r := range dashboard.AdminRoutes {
		if _, ok := want[r.Method+" "+r.Path]; ok {
			want[r.Method+" "+r.Path] = true
			if r.Auth != dashboard.AuthSensitive {
				t.Errorf("%s %s auth = %v, want sensitive", r.Method, r.Path, r.Auth)
			}
		}
	}
	for k, found := range want {
		if !found {
			t.Errorf("route %s missing from AdminRoutes", k)
		}
	}
}
