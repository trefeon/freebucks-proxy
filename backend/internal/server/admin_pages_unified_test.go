package server

// Unified-store plane tests for per-page UI state (Lane D).
//
//   - I1 (sync-visible): PUT swaps the mem snapshot in-request; the next
//     GET serves it without waiting on disk.
//   - I2 (no disk on request path): with the spill consumer parked (flush
//     interval pinned huge, batch unfilled), PUT+GET complete while the
//     pages_state table stays empty — proven by write-call counting at the
//     unit level and by direct store reads at the HTTP level, not timing.
//   - I3 (restart recovers): after the spill lands, reopening the DB file
//     under fresh handlers serves the snapshot (boot preload path).
//
// I4 is vacuous for pages (no .env leg ever existed here); the UI-side I4
// proof (no save flow POSTs /admin/config) lives in frontend/e2e/.

import (
	"encoding/json"
	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/dashboard"
	"freebucks-proxy/backend/internal/store"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newPageTestHandlers(t *testing.T, st *store.Store) *adminHandlers {
	t.Helper()
	dash := dashboard.New(func() *config.Config { return &config.Config{} }, nil, nil, slog.Default(), nil)
	return &adminHandlers{
		dash:     dash,
		logfunc:  func() *slog.Logger { return slog.Default() },
		cfgLoad:  func() *config.Config { return &config.Config{} },
		settings: st,
	}
}

func putPage(t *testing.T, a *adminHandlers, id, data string) map[string]any {
	t.Helper()
	body := `{"data":` + data + `}`
	req := httptest.NewRequest(http.MethodPut, "/admin/api/pages/"+id, strings.NewReader(body))
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	a.handlePageStatePut(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT pages/%s = %d, want 200: %s", id, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("PUT pages/%s decode: %v", id, err)
	}
	if out["ok"] != true || out["code"] != "page_saved" {
		t.Fatalf("PUT pages/%s receipt = %v, want ok page_saved", id, out)
	}
	return out
}

func getPage(t *testing.T, a *adminHandlers, id string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/pages/"+id, nil)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	a.handlePageStateGet(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET pages/%s decode: %v", id, err)
	}
	return rec.Code, out
}

// TestPageStateMemSyncVisible (I1, unit): swap is visible to the next get.
func TestPageStateMemSyncVisible(t *testing.T) {
	m := newPageStateMem(nil)
	m.swap("tokens", `{"expanded":2}`)
	got, ok := m.get("tokens")
	if !ok || got != `{"expanded":2}` {
		t.Fatalf("get after swap = (%q, %v), want ({\"expanded\":2}, true)", got, ok)
	}
	m.swap("tokens", `{"expanded":3}`)
	if got, _ := m.get("tokens"); got != `{"expanded":3}` {
		t.Fatalf("get after overwrite = %q, want {\"expanded\":3}", got)
	}
	if _, ok := m.get("overview"); ok {
		t.Fatal("get absent page = true, want false")
	}
}

// TestPageStateMemNoSyncDisk (I2, unit): swap+enqueue performs zero persist
// calls synchronously; the consumer batch-dedupes to one write carrying the
// latest value.
func TestPageStateMemNoSyncDisk(t *testing.T) {
	m := newPageStateMem(nil)
	var calls atomic.Int64
	var last atomic.Value
	m.persist = func(id, data string) error {
		calls.Add(1)
		last.Store(id + "=" + data)
		return nil
	}
	m.swap("tokens", `{"v":1}`)
	m.enqueue(pageSpillEntry{id: "tokens", data: `{"v":1}`})
	m.swap("tokens", `{"v":2}`)
	m.enqueue(pageSpillEntry{id: "tokens", data: `{"v":2}`})
	if got := calls.Load(); got != 0 {
		t.Fatalf("persist calls after swap+enqueue = %d, want 0 (spill is behind)", got)
	}
	if got, _ := m.get("tokens"); got != `{"v":2}` {
		t.Fatalf("mem after swaps = %q, want latest", got)
	}

	oldEvery := pageSpillFlushEvery
	pageSpillFlushEvery = 5 * time.Millisecond
	defer func() { pageSpillFlushEvery = oldEvery }()
	go m.run(func() *slog.Logger { return slog.Default() })
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // let a second cycle prove no duplicate
	if got := calls.Load(); got != 1 {
		t.Fatalf("persist calls after spill = %d, want exactly 1 (batch dedupe)", got)
	}
	if got := last.Load().(string); got != `tokens={"v":2}` {
		t.Fatalf("spilled payload = %q, want latest", got)
	}
}

// TestPageStateSpillDropsOnFull: a saturated buffer drops with a counter
// instead of blocking the request.
func TestPageStateSpillDropsOnFull(t *testing.T) {
	m := newPageStateMem(nil) // no consumer: the 1024 buffer fills, then drops
	for range pageSpillBufSize {
		m.enqueue(pageSpillEntry{id: "tokens", data: "{}"})
	}
	if got := m.droppedCount(); got != 0 {
		t.Fatalf("dropped while buffering = %d, want 0", got)
	}
	m.enqueue(pageSpillEntry{id: "tokens", data: "{}"})
	if got := m.droppedCount(); got != 1 {
		t.Fatalf("dropped after overflow = %d, want 1", got)
	}
}

// TestPageStatePutGetHTTP (I1, handler): PUT then GET serves the snapshot.
func TestPageStatePutGetHTTP(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "pages.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	a := newPageTestHandlers(t, st)

	putPage(t, a, "tokens", `{"expanded":2}`)
	if code, out := getPage(t, a, "tokens"); code != http.StatusOK {
		t.Fatalf("GET after PUT = %d %v, want 200", code, out)
	} else if data, ok := out["data"].(map[string]any); !ok || data["expanded"] != float64(2) {
		t.Fatalf("GET after PUT data = %v, want {expanded:2}", out["data"])
	}
	putPage(t, a, "tokens", `{"filter":"x"}`)
	if _, out := getPage(t, a, "tokens"); out["data"].(map[string]any)["filter"] != "x" {
		t.Fatalf("GET after overwrite = %v, want {filter:x}", out["data"])
	}
}

// TestPageStatePutNoSyncDBWrite (I2, handler): with the consumer parked,
// PUT+GET round-trip while the table stays empty.
func TestPageStatePutNoSyncDBWrite(t *testing.T) {
	oldEvery := pageSpillFlushEvery
	pageSpillFlushEvery = time.Hour
	defer func() { pageSpillFlushEvery = oldEvery }()

	st, err := store.Open(filepath.Join(t.TempDir(), "pages.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	a := newPageTestHandlers(t, st)

	putPage(t, a, "tokens", `{"expanded":2}`)
	if _, out := getPage(t, a, "tokens"); out["data"].(map[string]any)["expanded"] != float64(2) {
		t.Fatalf("GET after PUT = %v, want mem snapshot while DB is parked", out["data"])
	}
	// The spill holds one staged row (batch 1 < 100, timer parked at 1h),
	// so the table must still be empty: zero synchronous writes.
	if _, ok, err := st.GetPageState("tokens"); err != nil || ok {
		t.Fatalf("direct store read = (%v, %v), want (\"\", false): PUT wrote synchronously", ok, err)
	}
}

// TestPageStateRestartRecovers (I3, handler): once spilled, the snapshot
// survives a store reopen under fresh handlers (boot preload).
func TestPageStateRestartRecovers(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "pages.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	a := newPageTestHandlers(t, st)
	putPage(t, a, "overview", `{"scroll":12}`)

	deadline := time.Now().Add(10 * time.Second)
	for {
		if v, ok, err := st.GetPageState("overview"); err == nil && ok && v == `{"scroll":12}` {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("spill did not land within 10s")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store reopen: %v", err)
	}
	t.Cleanup(func() { _ = st2.Close() })
	b := newPageTestHandlers(t, st2)
	if code, out := getPage(t, b, "overview"); code != http.StatusOK {
		t.Fatalf("GET after reopen = %d %v, want 200", code, out)
	} else if data, ok := out["data"].(map[string]any); !ok || data["scroll"] != float64(12) {
		t.Fatalf("GET after reopen = %v, want {scroll:12}", out["data"])
	}
}
