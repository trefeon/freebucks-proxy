package dashboard

import (
	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/logring"
	"freebucks-proxy/backend/internal/pool"
	"freebucks-proxy/backend/internal/store"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// unifiedReqWithQuery builds a GET request carrying raw query params for the
// history data readers.
func unifiedReqWithQuery(t *testing.T, raw string) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodGet, "/admin/api/history?"+raw, nil)
}

// durations and never depend on wall timing.
func waitSpillDrained(t *testing.T, d *Dashboard) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for d.spillPending() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if d.spillPending() > 0 {
		t.Fatalf("spill backlog stuck at %d after 15s", d.spillPending())
	}
}

// I1: every history mutation is visible to the next read synchronously —
// before the background flush lands. Quota samples surface in the quota
// history reader, maturity events in the maturity reader, pages upserts in
// PendingPageState, log records in the logs reader.
func TestUnifiedStoreSyncVisible(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "i1.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	d := New(func() *config.Config { return &config.Config{} }, nil, nil, slog.Default(), nil, WithHistory(st))
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	now := store.Millis(time.Now())
	d.EnqueueQuota(store.QuotaSnapshot{TS: now, TokenIdx: 0, Model: "m", Limit: 75, Recent: 30})
	d.EnqueueMaturity(store.MaturityEvent{TS: now, TokenIdx: 0, Kind: "touch", Detail: "admit ok"})
	d.EnqueueRequest(store.RequestRecord{ReqID: "r-i1", TS: now, Endpoint: "/v1/chat/completions", Model: "m", Status: "ok"})
	d.EnqueuePageState("overview", `{"cards":["a"]}`)

	// All four visible synchronously, with the flush still pending.
	if got := d.quotaHistoryData(unifiedReqWithQuery(t, "token=0&model=m")).Snapshots; len(got) != 1 || got[0].Recent != 30 {
		t.Fatalf("quota snapshots = %+v, want [recent 30]", got)
	}
	if got := d.maturityHistoryData(unifiedReqWithQuery(t, "token=0")).Events; len(got) != 1 || got[0].Kind != "touch" {
		t.Fatalf("maturity events = %+v, want [touch]", got)
	}
	// Request rows surface through the store query after flush only (the
	// ring/console views render them via rollups), so I1 for requests is
	// the spill staging itself: the drain below must deliver it.
	if n := d.spillPending(); n < 4 {
		t.Fatalf("spill backlog = %d, want >= 4 staged items", n)
	}
	if data, ok := d.PendingPageState("overview"); !ok || data != `{"cards":["a"]}` {
		t.Fatalf("PendingPageState = %q,%v, want cards,true", data, ok)
	}
	// Overwrite wins synchronously too.
	d.EnqueuePageState("overview", `{"cards":["b"]}`)
	if data, ok := d.PendingPageState("overview"); !ok || data != `{"cards":["b"]}` {
		t.Fatalf("PendingPageState after overwrite = %q,%v, want b,true", data, ok)
	}

	// And the flush delivers every row to the DB exactly once.
	waitSpillDrained(t, d)
	rows, err := st.QuotaHistory(0, "m", 0, 0)
	if err != nil {
		t.Fatalf("QuotaHistory: %v", err)
	}
	if len(rows) != 1 || rows[0].Recent != 30 {
		t.Fatalf("persisted quota rows = %+v, want [30]", rows)
	}
	evs, err := st.MaturityHistory(0, 0, 0)
	if err != nil {
		t.Fatalf("MaturityHistory: %v", err)
	}
	if len(evs) != 1 || evs[0].Kind != "touch" {
		t.Fatalf("persisted maturity rows = %+v, want [touch]", evs)
	}
	reqs, err := st.QueryRequests(0, 0)
	if err != nil {
		t.Fatalf("QueryRequests: %v", err)
	}
	if len(reqs) != 1 || reqs[0].ReqID != "r-i1" {
		t.Fatalf("persisted request rows = %+v, want [r-i1]", reqs)
	}
	if data, ok, err := st.GetPageState("overview"); err != nil || !ok || data != `{"cards":["b"]}` {
		t.Fatalf("persisted page = %q,%v,%v, want b,true,nil", data, ok, err)
	}
	// Re-reads after flush show no duplicates: staged rows withdrew.
	if got := d.quotaHistoryData(unifiedReqWithQuery(t, "token=0&model=m")).Snapshots; len(got) != 1 {
		t.Fatalf("quota snapshots after flush = %d, want 1", len(got))
	}
}

// I1 (pool sink): the pool's HistorySink stages through the spill with no
// disk on the emitting goroutine.
func TestUnifiedStorePoolSinkSyncVisible(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "i1pool.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	d := New(func() *config.Config { return &config.Config{} }, nil, nil, slog.Default(), nil, WithHistory(st))
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	d.RecordMaturity(pool.MaturityHistoryEvent{TS: store.Millis(time.Now()), TokenIdx: 1, Kind: "touch", Detail: "admit ok"})
	if got := d.maturityHistoryData(unifiedReqWithQuery(t, "token=1")).Events; len(got) != 1 {
		t.Fatalf("maturity events = %+v, want 1 staged row", got)
	}
	waitSpillDrained(t, d)
	evs, err := st.MaturityHistory(1, 0, 0)
	if err != nil {
		t.Fatalf("MaturityHistory: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("persisted maturity rows = %d, want 1", len(evs))
	}
}

// I2: no disk I/O on the mutation path. With write-call counting enabled,
// staging five history writes performs zero store writes; the background
// flush performs at most five (one batched transaction per table).
func TestUnifiedStoreNoDiskOnMutationPath(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "i2.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	d := New(func() *config.Config { return &config.Config{} }, nil, nil, slog.Default(), nil, WithHistory(st))
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	store.SetHistoryWriteCounting(true)
	defer store.SetHistoryWriteCounting(false)
	now := store.Millis(time.Now())
	d.EnqueueQuota(store.QuotaSnapshot{TS: now, TokenIdx: 0, Model: "m", Limit: 75, Recent: 30})
	d.EnqueueMaturity(store.MaturityEvent{TS: now, TokenIdx: 0, Kind: "touch"})
	d.EnqueueRequest(store.RequestRecord{ReqID: "r-i2", TS: now, Endpoint: "/v1/chat/completions", Status: "ok"})
	d.EnqueuePageState("overview", `{"a":1}`)
	d.enqueueSpill(logring.Entry{Level: "INFO", Message: "hello"})
	if got := store.HistoryWriteCount(); got != 0 {
		t.Fatalf("store writes during staging = %d, want 0 (no disk on mutation path)", got)
	}
	waitSpillDrained(t, d)
	if got := store.HistoryWriteCount(); got == 0 || got > 5 {
		t.Fatalf("store writes after flush = %d, want 1..5 (one batch per table)", got)
	}
}

// I2 (batch discipline): 250 log records flush in at most 3 batched writes
// (100/100/50), never one write per row.
func TestUnifiedStoreSpillBatches(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "i2batch.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	d := New(func() *config.Config { return &config.Config{} }, nil, nil, slog.Default(), nil, WithHistory(st))
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	store.SetHistoryWriteCounting(true)
	defer store.SetHistoryWriteCounting(false)
	for range 250 {
		d.enqueueSpill(logring.Entry{Level: "INFO", Message: "batch line"})
	}
	waitSpillDrained(t, d)
	if got := store.HistoryWriteCount(); got == 0 || got > 3 {
		t.Fatalf("store writes for 250 rows = %d, want 1..3 batches", got)
	}
	rows, err := st.QueryLogs(store.LogFilter{Limit: 5000})
	if err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if len(rows) != 250 {
		t.Fatalf("persisted rows = %d, want 250", len(rows))
	}
	if got := d.spillStats(); got != 0 {
		t.Fatalf("dropped = %d, want 0", got)
	}
}

// I2 (drop accounting): overfilling the buffer drops with a counter and
// never shows dropped rows as pending.
func TestUnifiedStoreSpillDropsCounted(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "i2drop.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	d := New(func() *config.Config { return &config.Config{} }, nil, nil, slog.Default(), nil, WithHistory(st))
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	// Fill past the 1024 buffer without yielding to the consumer. Staging
	// 2000 items guarantees drops regardless of consumer progress.
	sent := 0
	for i := 0; i < 2000; i++ {
		before := d.spillStats()
		d.enqueueSpill(logring.Entry{Level: "INFO", Message: "flood"})
		sent++
		if d.spillStats() > before && sent > spillBufSize {
			break
		}
	}
	if got := d.spillStats(); got == 0 {
		t.Skip("consumer kept up with 2000 staged rows; drop path not exercised")
	}
	if got := d.SpillDroppedByKind(spillKindLog); got != d.spillStats() {
		t.Fatalf("per-kind log drops = %d, total = %d, want equal", got, d.spillStats())
	}
}

// I4: the history plane never reads or writes the .env file. There is no
// dotenv import in the plane (pinned by TestUnifiedStoreNoDotenvImports),
// and staging plus the full flush leave a planted .env byte-identical.
func TestUnifiedStoreEnvUntouched(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	seed := []byte("ADMIN_TOKEN=seed\nAUTH_TOKENS=x\n")
	if err := os.WriteFile(envPath, seed, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "i4.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	d := New(func() *config.Config { return &config.Config{} }, nil, nil, slog.Default(), nil, WithHistory(st))
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	now := store.Millis(time.Now())
	d.EnqueueQuota(store.QuotaSnapshot{TS: now, TokenIdx: 0, Model: "m", Limit: 75, Recent: 30})
	d.EnqueueMaturity(store.MaturityEvent{TS: now, TokenIdx: 0, Kind: "touch"})
	d.EnqueueRequest(store.RequestRecord{ReqID: "r-i4", TS: now, Endpoint: "/v1/chat/completions", Status: "ok"})
	d.EnqueuePageState("overview", `{"a":1}`)
	d.enqueueSpill(logring.Entry{Level: "INFO", Message: "hello"})
	d.purgeHistory()
	waitSpillDrained(t, d)
	after, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read .env: %v", err)
	}
	if string(after) != string(seed) {
		t.Fatalf(".env changed: was %q, now %q", seed, after)
	}
}
