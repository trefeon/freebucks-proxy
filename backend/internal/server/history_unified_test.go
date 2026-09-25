package server

import (
	"freebucks-proxy/backend/internal/logring"
	"freebucks-proxy/backend/internal/phasetiming"
	"freebucks-proxy/backend/internal/store"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

// waitHistoryDrained polls the dashboard spill backlog to zero: background
// delivery is bounded by the 1s flush tick, so tests never sleep fixed
// durations.
func waitHistoryDrained(t *testing.T, srv *Server) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for srv.dash.SpillPending() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if srv.dash.SpillPending() > 0 {
		t.Fatalf("spill backlog stuck at %d after 15s", srv.dash.SpillPending())
	}
}

// I1: traceChat stages the request outcome synchronously visible to the next
// store read after flush, with zero disk writes during the chat-path call.
func TestUnifiedHistoryRequestOutcomeSpill(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "unified-chat.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ring := logring.NewHandler(slog.Default().Handler(), 16)
	srv, _ := newTestServerStack(t, nil, nil, nil, slog.Default(), ring, WithHistory(st))
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	store.SetHistoryWriteCounting(true)
	defer store.SetHistoryWriteCounting(false)
	srv.traceChat(nil, "m/a", 42, "ok", "", map[string]int64{phasetiming.UpstreamTTFBMS: 7}, &chatTraceState{reqID: "r-unified-1", clientKeyHash: "0123456789abcdef"})
	// Pre-attempt refusals carry no req_id: ring log only, no DB row.
	srv.traceChat(nil, "m/a", 1, "error", "refused", nil, nil)
	if got := store.HistoryWriteCount(); got != 0 {
		t.Fatalf("store writes on chat path = %d, want 0 (spill only)", got)
	}
	waitHistoryDrained(t, srv)
	rows, err := st.QueryRequests(0, 0)
	if err != nil {
		t.Fatalf("QueryRequests: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("request rows = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.ReqID != "r-unified-1" || got.Model != "m/a" || got.Status != "ok" || got.TTFBms != 7 || got.TokenIdx != -1 {
		t.Fatalf("request row = %+v, want r-unified-1/m/a/ok/ttfb 7/token -1", got)
	}
	if got.ClientKeyHash != "0123456789abcdef" {
		t.Fatalf("client key identity = %q, want sha-only hash", got.ClientKeyHash)
	}
	if n := store.HistoryWriteCount(); n == 0 || n > 2 {
		t.Fatalf("store writes after flush = %d, want 1..2", n)
	}
}

// I4: the history plane has no dotenv leg. The only ReadFile/WriteFile-style
// filesystem access in the owned history files is the legacy-carry trio
// staging (history.go), which copies a legacy DB file — never a .env. This
// pins the I4 file contract: no imported dotenv/envfile package, no .env
// path reference in the dashboard spill or pages paths.
func TestUnifiedHistoryNoDotenvLeg(t *testing.T) {
	for _, tc := range []struct{ path, file string }{
		{"backend/internal/dashboard/dashboard_history.go", "dashboard_history.go"},
		{"backend/internal/store/history.go", "history.go"},
		{"backend/internal/store/pages.go", "pages.go"},
		{"backend/internal/store/history_rollup.go", "history_rollup.go"},
	} {
		assertNoDotenvRef(t, tc.path)
	}
}
