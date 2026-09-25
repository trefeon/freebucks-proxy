// Unified-store I3: restart recovers every history plane. Staged rows flush
// on Close; a reopened store serves them through the readers with identical
// shapes, and Close with a live dashboard drains buffered items first.
package store

import (
	"path/filepath"
	"testing"
)

// Restart recovers request records, quota samples, maturity events, log
// entries, and page snapshots written before close (I3).
func TestUnifiedHistoryRestartRecovers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unified-restart.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.AppendRequests([]RequestRecord{{ReqID: "r-restart", TS: 1000, Endpoint: "/v1/chat/completions", Model: "m", Status: "ok", ClientKeyHash: "0123456789abcdef"}}); err != nil {
		t.Fatalf("AppendRequests: %v", err)
	}
	if err := s.AppendQuotas([]QuotaSnapshot{{TS: 1000, TokenIdx: 0, Model: "m", Limit: 75, Recent: 30}}); err != nil {
		t.Fatalf("AppendQuotas: %v", err)
	}
	if err := s.AppendMaturities([]MaturityEvent{{TS: 1000, TokenIdx: 0, Kind: "touch", Detail: "admit ok"}}); err != nil {
		t.Fatalf("AppendMaturities: %v", err)
	}
	if err := s.AppendLogs([]LogEntry{{TS: 1000, Level: "info", Msg: "chat done", ReqID: "r-restart"}}); err != nil {
		t.Fatalf("AppendLogs: %v", err)
	}
	if err := s.PutPageState("overview", `{"cards":["a"]}`); err != nil {
		t.Fatalf("PutPageState: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	defer func() { _ = s.Close() }()
	if reqs, err := s.QueryRequests(0, 0); err != nil || len(reqs) != 1 || reqs[0].ReqID != "r-restart" {
		t.Fatalf("requests after restart = %+v,%v, want [r-restart],nil", reqs, err)
	}
	if rows, err := s.QuotaHistory(0, "m", 0, 0); err != nil || len(rows) != 1 || rows[0].Recent != 30 {
		t.Fatalf("quota after restart = %+v,%v, want [30],nil", rows, err)
	}
	if evs, err := s.MaturityHistory(0, 0, 0); err != nil || len(evs) != 1 || evs[0].Kind != "touch" {
		t.Fatalf("maturity after restart = %+v,%v, want [touch],nil", evs, err)
	}
	if logs, err := s.QueryLogs(LogFilter{}); err != nil || len(logs) != 1 || logs[0].Msg != "chat done" {
		t.Fatalf("logs after restart = %+v,%v, want [chat done],nil", logs, err)
	}
	if data, ok, err := s.GetPageState("overview"); err != nil || !ok || data != `{"cards":["a"]}` {
		t.Fatalf("page after restart = %q,%v,%v, want cards,true,nil", data, ok, err)
	}
}

// The batch appends keep the single-row contracts: empty batches are
// no-ops, empty req_ids never persist, re-records stay idempotent.
func TestUnifiedHistoryBatchContracts(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "unified-batch.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.AppendRequests(nil); err != nil {
		t.Fatalf("AppendRequests(nil): %v", err)
	}
	if err := s.AppendQuotas(nil); err != nil {
		t.Fatalf("AppendQuotas(nil): %v", err)
	}
	if err := s.AppendMaturities(nil); err != nil {
		t.Fatalf("AppendMaturities(nil): %v", err)
	}
	if err := s.AppendRequests([]RequestRecord{{TS: 1, Endpoint: "/v1/chat/completions"}}); err != nil {
		t.Fatalf("AppendRequests(empty id): %v", err)
	}
	if err := s.AppendRequests([]RequestRecord{
		{ReqID: "r1", TS: 1000, Endpoint: "/v1/chat/completions", Model: "m", Status: "ok"},
		{ReqID: "r2", TS: 2000, Endpoint: "/v1/chat/completions", Model: "m", Status: "error"},
	}); err != nil {
		t.Fatalf("AppendRequests: %v", err)
	}
	if err := s.AppendRequests([]RequestRecord{
		{ReqID: "r2", TS: 2500, Endpoint: "/v1/chat/completions", Model: "m/a", Status: "error"},
	}); err != nil {
		t.Fatalf("AppendRequests upsert: %v", err)
	}
	reqs, err := s.QueryRequests(0, 0)
	if err != nil {
		t.Fatalf("QueryRequests: %v", err)
	}
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2 (empty skipped, upsert not duplicated)", len(reqs))
	}
	// Raw client tokens never reach the table: only the sha-only identity.
	if err := s.AppendRequests([]RequestRecord{{ReqID: "r3", TS: 3000, Endpoint: "/v1/chat/completions", ClientKeyHash: "0123456789abcdef"}}); err != nil {
		t.Fatalf("AppendRequests key hash: %v", err)
	}
	got, err := s.QueryRequests(0, 10)
	if err != nil {
		t.Fatalf("QueryRequests: %v", err)
	}
	for _, r := range got {
		if len(r.ClientKeyHash) > 16 {
			t.Fatalf("client key identity = %q, want sha-only (<=16 hex)", r.ClientKeyHash)
		}
	}
}
