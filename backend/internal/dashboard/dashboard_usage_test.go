package dashboard

import (
	"encoding/json"
	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/pool"
	"freebucks-proxy/backend/internal/registry"
	"freebucks-proxy/backend/internal/session"
	"freebucks-proxy/backend/internal/testutil"
	"freebucks-proxy/backend/internal/upstream"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// usageTestDashboard builds a dashboard whose wire prices map prices
// "test/priced" at 20 Freebucks and leaves "test/unpriced" without a
// price — the same UpdateQuotaFromProbe path production admissions use.
func usageTestDashboard(t *testing.T) *Dashboard {
	t.Helper()
	cfg := &config.Config{
		AuthTokens:         []string{"tok-0"},
		ListenAddr:         "127.0.0.1:3457",
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    "https://www.codebuff.com",
	}
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	clientCfg := *cfg
	clientCfg.UpstreamBaseURL = mock.URL()
	client, err := upstream.New("tok-0", &clientCfg)
	if err != nil {
		t.Fatal(err)
	}
	mgr := session.NewManager(client)
	mgr.UpdateQuotaFromProbe(&upstream.SessionState{
		Freebucks: &upstream.FreebucksInfo{
			Balance: 100,
			Prices:  map[string]float64{"test/priced": 20},
		},
	})
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, []*upstream.Client{client}, []*session.Manager{mgr}, reg)
	if err != nil {
		t.Fatal(err)
	}
	return New(func() *config.Config { return cfg }, p, reg, nil, nil)
}

func getUsage(t *testing.T, d *Dashboard, query string) (int, usageData) {
	t.Helper()
	rec := httptest.NewRecorder()
	d.APIHandler("usage")(rec, httptest.NewRequest(http.MethodGet, "/admin/api/usage"+query, nil))
	var out usageData
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("usage response is not valid JSON: %v (body %q)", err, rec.Body.String())
	}
	return rec.Code, out
}

// Three records across time windows: now (priced), 2 days ago (priced),
// 10 days ago (unpriced). Range filters must slice totals/entries; the
// unpriced model contributes tokens but zero cost.
func TestUsageRangeFiltersTotalsAndEntries(t *testing.T) {
	d := usageTestDashboard(t)
	now := time.Now()
	d.RecordUsage(UsageRecord{TsMs: now.UnixMilli(), ReqID: "r-now", Model: "test/priced", Input: 100, Output: 50, Cached: 10, Total: 160, OK: true})
	d.RecordUsage(UsageRecord{TsMs: now.Add(-48 * time.Hour).UnixMilli(), ReqID: "r-2d", Model: "test/priced", Input: 200, Output: 20, Cached: 0, Reasoning: 5, Total: 225, OK: true})
	d.RecordUsage(UsageRecord{TsMs: now.Add(-240 * time.Hour).UnixMilli(), ReqID: "r-10d", Model: "test/unpriced", Input: 7, Output: 3, Cached: 0, Total: 10, OK: false})

	code, today := getUsage(t, d, "?range=today")
	if code != http.StatusOK {
		t.Fatalf("today status = %d, want 200", code)
	}
	if today.Range != "today" {
		t.Errorf("today range echo = %q, want %q", today.Range, "today")
	}
	if today.Totals.Requests != 1 || today.Totals.Input != 100 || today.Totals.Cached != 10 || today.Totals.Output != 50 {
		t.Errorf("today totals = %+v, want {1 100 10 50}", today.Totals)
	}
	if today.Totals.Cost != 20 {
		t.Errorf("today cost = %v, want 20 (one priced entry)", today.Totals.Cost)
	}
	if len(today.Entries) != 1 || today.Entries[0].ReqID != "r-now" {
		t.Errorf("today entries = %+v, want [r-now]", today.Entries)
	}

	_, week := getUsage(t, d, "?range=7d")
	if week.Totals.Requests != 2 || week.Totals.Input != 300 || week.Totals.Output != 70 {
		t.Errorf("7d totals = %+v, want {2 300 cached:10 70}", week.Totals)
	}
	if week.Totals.Cost != 40 {
		t.Errorf("7d cost = %v, want 40 (two priced entries)", week.Totals.Cost)
	}
	if len(week.Entries) != 2 || week.Entries[0].ReqID != "r-2d" || week.Entries[1].ReqID != "r-now" {
		t.Errorf("7d entries order = %+v, want newest-first [r-2d r-now]", week.Entries)
	}

	_, month := getUsage(t, d, "?range=30d")
	if month.Totals.Requests != 3 {
		t.Errorf("30d requests = %d, want 3", month.Totals.Requests)
	}
	if month.Totals.Cost != 40 {
		t.Errorf("30d cost = %v, want 40 (unpriced model contributes 0)", month.Totals.Cost)
	}
	if len(month.Entries) != 3 || month.Entries[0].ReqID != "r-10d" {
		t.Errorf("30d entries = %+v, want 3 rows newest-first", month.Entries)
	}
}

// Default range is today with an empty (non-null) entries array.
func TestUsageDefaultRangeAndEmptyEntries(t *testing.T) {
	d := usageTestDashboard(t)
	code, out := getUsage(t, d, "")
	if code != http.StatusOK {
		t.Fatalf("default status = %d, want 200", code)
	}
	if out.Range != "today" {
		t.Errorf("default range = %q, want today", out.Range)
	}
	if out.Entries == nil {
		t.Error("entries is null, want []")
	}
	if out.Totals.Requests != 0 || out.Totals.Cost != 0 {
		t.Errorf("empty totals = %+v, want zeros", out.Totals)
	}
}

// The ring evicts oldest-first past the cap and stays O(1)/zero-alloc once
// full (issue #656: the old append-and-reslice copied the whole window on
// every request past the cap).
func TestUsageRingEvictsOldest(t *testing.T) {
	d := usageTestDashboard(t)
	base := time.Now().UnixMilli()
	for i := range maxUsageRecords + 1 {
		d.RecordUsage(UsageRecord{TsMs: base + int64(i), ReqID: "r", Model: "m", Total: 1, OK: true})
	}
	_, out := getUsage(t, d, "")
	if len(out.Entries) != maxUsageRecords {
		t.Fatalf("entries = %d, want cap %d", len(out.Entries), maxUsageRecords)
	}
	if got := out.Entries[0].TsMs; got != base+int64(maxUsageRecords) {
		t.Errorf("newest ts_ms = %d, want %d", got, base+int64(maxUsageRecords))
	}
	if got := out.Entries[len(out.Entries)-1].TsMs; got != base+1 {
		t.Errorf("oldest ts_ms = %d, want %d (first record evicted)", got, base+1)
	}
	rec := UsageRecord{TsMs: base, ReqID: "alloc", Model: "m", Total: 1, OK: true}
	if allocs := testing.AllocsPerRun(100, func() { d.RecordUsage(rec) }); allocs != 0 {
		t.Errorf("RecordUsage allocs/run = %v on a full ring, want 0", allocs)
	}
}

// traceFromFields must surface the Contract UsageRecord keys verbatim when
// the chat path logs them, and omit them otherwise (TracesBadgeAudit
// renders the per-trace token line only when present).
func TestTraceFromFieldsCarriesUsageKeys(t *testing.T) {
	e := traceFromFields("2026-09-14T00:00:00Z", []string{
		"token=1", "model=test/priced", "status=ok", "ms=42", "req_id=abc-123",
		"input=100", "output=50", "cached=10", "reasoning=5", "total=165",
	})
	if e.ReqID != "abc-123" {
		t.Errorf("req_id = %q, want abc-123", e.ReqID)
	}
	if e.Input != 100 || e.Output != 50 || e.Cached != 10 || e.Reasoning != 5 || e.Total != 165 {
		t.Errorf("token counts = %+v, want 100/50/10/5/165", e)
	}
	if e.TsMs == 0 {
		t.Error("ts_ms missing on a parseable stamp")
	}
	bare := traceFromFields("2026-09-14T00:00:00Z", []string{"token=1", "model=m"})
	if bare.ReqID != "" || bare.Input != 0 || bare.Output != 0 || bare.Cached != 0 || bare.Reasoning != 0 || bare.Total != 0 {
		t.Errorf("absent keys must stay zero, got %+v", bare)
	}
	raw, _ := json.Marshal(bare)
	for _, k := range []string{"req_id", "input", "output", "cached", "reasoning", "total"} {
		if strings.Contains(string(raw), `"`+k+`"`) {
			t.Errorf("bare trace JSON contains %q, want omitted: %s", k, raw)
		}
	}
}
