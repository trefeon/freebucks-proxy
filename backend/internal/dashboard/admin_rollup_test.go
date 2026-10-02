package dashboard_test

import (
	"encoding/json"
	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/dashboard"
	"freebuff-proxy/backend/internal/store"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func seedActivityStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "activity.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rows := []store.RequestRecord{
		{ReqID: "a1", TS: 1_000, Endpoint: "/v1/chat/completions", Model: "m/a", TokenIdx: 0, Status: "ok"},
		{ReqID: "a2", TS: 2_000, Endpoint: "/v1/chat/completions", Model: "m/a", TokenIdx: 1, Status: "error", Err: "x"},
		{ReqID: "a3", TS: 86400000 + 1_000, Endpoint: "/v1/messages", Model: "m/b", TokenIdx: -1, Status: "ok"},
	}
	for _, rec := range rows {
		if err := st.RecordRequest(rec); err != nil {
			t.Fatalf("RecordRequest %s: %v", rec.ReqID, err)
		}
	}
	return st
}

func TestActivityRollupDay(t *testing.T) {
	cfg := &config.Config{}
	d := dashboard.New(func() *config.Config { return cfg }, nil, nil, slog.Default(), nil, dashboard.WithHistory(seedActivityStore(t)))
	rec := httptest.NewRecorder()
	d.APIActivityRollup(rec, httptest.NewRequest(http.MethodGet, "/admin/api/activity/rollup?since=0&until=200000000", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Buckets []struct {
			TS    int64 `json:"ts"`
			Total int64 `json:"total"`
		} `json:"buckets"`
		Enabled     bool   `json:"enabled"`
		Granularity string `json:"granularity"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Enabled || got.Granularity != "day" {
		t.Errorf("envelope = enabled:%v granularity:%q, want true/day", got.Enabled, got.Granularity)
	}
	if len(got.Buckets) != 2 || got.Buckets[0].Total != 2 || got.Buckets[1].Total != 1 {
		t.Errorf("buckets = %+v, want [2 1]", got.Buckets)
	}
}

func TestActivityRollupHourAndBadParams(t *testing.T) {
	cfg := &config.Config{}
	d := dashboard.New(func() *config.Config { return cfg }, nil, nil, slog.Default(), nil, dashboard.WithHistory(seedActivityStore(t)))
	rec := httptest.NewRecorder()
	d.APIActivityRollup(rec, httptest.NewRequest(http.MethodGet, "/admin/api/activity/rollup?since=0&until=200000000&granularity=hour", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("hour status = %d, want 200", rec.Code)
	}
	var got struct {
		Buckets []struct {
			TS int64 `json:"ts"`
		} `json:"buckets"`
		Granularity string `json:"granularity"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Buckets) != 2 {
		t.Errorf("hour buckets = %d, want 2", len(got.Buckets))
	}
	for _, bad := range []string{
		"/admin/api/activity/rollup?since=0&until=200000000&granularity=fortnight",
		"/admin/api/activity/rollup?since=9&until=9",
	} {
		rec := httptest.NewRecorder()
		d.APIActivityRollup(rec, httptest.NewRequest(http.MethodGet, bad, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s status = %d, want 400 (rejected, never reinterpreted)", bad, rec.Code)
		}
	}
}

func TestActivityRollupNilHistory(t *testing.T) {
	cfg := &config.Config{}
	d := dashboard.New(func() *config.Config { return cfg }, nil, nil, slog.Default(), nil)
	rec := httptest.NewRecorder()
	d.APIActivityRollup(rec, httptest.NewRequest(http.MethodGet, "/admin/api/activity/rollup", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with enabled:false", rec.Code)
	}
	var got struct {
		Buckets []any `json:"buckets"`
		Enabled bool  `json:"enabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Enabled || got.Buckets == nil {
		t.Errorf("nil-history = enabled:%v buckets:%v, want false + empty array", got.Enabled, got.Buckets)
	}
}

func TestActivityTopModelsAndTokens(t *testing.T) {
	cfg := &config.Config{}
	d := dashboard.New(func() *config.Config { return cfg }, nil, nil, slog.Default(), nil, dashboard.WithHistory(seedActivityStore(t)))
	var got struct {
		Dimension string `json:"dimension"`
		Rows      []struct {
			Name  string `json:"name"`
			Total int64  `json:"total"`
		} `json:"rows"`
	}
	rec := httptest.NewRecorder()
	d.APIActivityTop(rec, httptest.NewRequest(http.MethodGet, "/admin/api/activity/top?since=0&until=200000000&dimension=model&limit=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("top models status = %d, want 200", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Rows) != 1 || got.Rows[0].Name != "m/a" || got.Rows[0].Total != 2 {
		t.Errorf("top models = %+v, want [m/a x2] (limit 1 cap)", got.Rows)
	}
	rec = httptest.NewRecorder()
	d.APIActivityTop(rec, httptest.NewRequest(http.MethodGet, "/admin/api/activity/top?since=0&until=200000000&dimension=token", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("top tokens status = %d, want 200", rec.Code)
	}
	got.Rows = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Rows) != 3 {
		t.Errorf("top tokens rows = %+v, want 3 lanes", got.Rows)
	}
	for _, bad := range []string{
		"/admin/api/activity/top?since=0&until=200000000&dimension=account",
		"/admin/api/activity/top?since=0&until=200000000&limit=0",
		"/admin/api/activity/top?since=0&until=200000000&limit=bunch",
	} {
		rec := httptest.NewRecorder()
		d.APIActivityTop(rec, httptest.NewRequest(http.MethodGet, bad, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s status = %d, want 400", bad, rec.Code)
		}
	}
}

func TestMemoryRollupStoreDayTopRecent(t *testing.T) {
	m := dashboard.NewMemoryRollupStore()
	day := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	m.Record("chat", "m/a", "key-1", 10, day)
	m.Record("chat", "m/a", "key-1", 5, day.Add(time.Hour))
	m.Record("chat", "m/b", "key-2", 7, day)
	got := m.Day("chat", "2026-09-01")
	if got == nil || got.Total != 22 || got.Count != 3 {
		t.Fatalf("Day() = %+v, want total 22 count 3", got)
	}
	if got.ByKey["m/a"] != 15 || got.ByUser["key-1"] != 15 {
		t.Errorf("Day dims = %+v, want m/a:15 key-1:15", got)
	}
	// Detached copy: mutating the return must not move live state.
	got.ByKey["m/a"] = -1
	if again := m.Day("chat", "2026-09-01"); again.ByKey["m/a"] != 15 {
		t.Error("Day() aliases live state, want a detached copy")
	}
	top := m.TopKeys("chat", "2026-09-01", 1)
	if len(top) != 1 || top[0].Name != "m/a" || top[0].Total != 15 {
		t.Errorf("TopKeys(1) = %+v, want [m/a 15]", top)
	}
	users := m.TopUsers("chat", "2026-09-01", 10)
	if len(users) != 2 || users[0].Name != "key-1" {
		t.Errorf("TopUsers() = %+v, want key-1 first", users)
	}
	if missing := m.Day("chat", "2026-09-02"); missing != nil {
		t.Errorf("Day(missing) = %+v, want nil", missing)
	}
	if empty := m.TopKeys("nope", "2026-09-01", 5); len(empty) != 0 {
		t.Errorf("TopKeys(missing) = %v, want empty", empty)
	}
	recent := m.Recent()
	if len(recent) != 3 || recent[0].Value != 7 {
		t.Errorf("Recent() = %+v, want 3 newest-first (7 first)", recent)
	}
}

func TestMemoryRollupStoreRingCaps(t *testing.T) {
	m := dashboard.NewMemoryRollupStore()
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i := range 80 {
		m.Record("chat", "m/a", "", float64(i), base.Add(time.Duration(i)*time.Minute))
	}
	recent := m.Recent()
	if len(recent) != 50 {
		t.Fatalf("Recent() = %d entries, want the 50-entry cap", len(recent))
	}
	if recent[0].Value != 79 {
		t.Errorf("Recent()[0].Value = %v, want 79 (newest first)", recent[0].Value)
	}
	if recent[49].Value != 30 {
		t.Errorf("Recent()[49].Value = %v, want 30 (oldest retained)", recent[49].Value)
	}
}

func TestMemoryRollupStoreUTCDayRollover(t *testing.T) {
	m := dashboard.NewMemoryRollupStore()
	// 23:30 Pacific = next UTC day: the sample rolls to the UTC date.
	pacific := time.Date(2026, 9, 1, 23, 30, 0, 0, time.FixedZone("PST", -8*3600))
	m.Record("chat", "m/a", "", 1, pacific)
	if got := m.Day("chat", "2026-09-01"); got != nil {
		t.Errorf("Pacific-day bucket hit %+v, want UTC-day rollover (nil)", got)
	}
	if got := m.Day("chat", "2026-09-02"); got == nil || got.Total != 1 {
		t.Errorf("UTC-day bucket = %+v, want total 1", got)
	}
}
