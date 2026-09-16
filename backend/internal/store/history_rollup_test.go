package store

// RED: RequestsRollup aggregation over request_records (no new columns).
// Fails until history_rollup.go lands.

import "testing"

func seedRollup(t *testing.T) *Store {
	t.Helper()
	s := openTest(t)
	t.Cleanup(func() { _ = s.Close() })
	rows := []RequestRecord{
		{ReqID: "r1", TS: 1_000, Endpoint: "/v1/chat/completions", Model: "m/a", TokenIdx: 0, Status: "ok", TTFBms: 100},
		{ReqID: "r2", TS: 2_000, Endpoint: "/v1/chat/completions", Model: "m/a", TokenIdx: 1, Status: "error", TTFBms: 200, Err: "upstream"},
		{ReqID: "r3", TS: 3_000, Endpoint: "/v1/chat/completions", Model: "m/b", TokenIdx: -1, Status: "ok", TTFBms: 300},
		{ReqID: "r4", TS: 4_000, Endpoint: "/v1/chat/completions", Model: "m/b", TokenIdx: -1, Status: "error", TTFBms: 400, Err: "auth"},
		{ReqID: "r5", TS: 5_000, Endpoint: "/v1/chat/completions", Model: "m/a", TokenIdx: 0, Status: "ok", TTFBms: 500},
	}
	for _, rec := range rows {
		if err := s.RecordRequest(rec); err != nil {
			t.Fatalf("RecordRequest %s: %v", rec.ReqID, err)
		}
	}
	return s
}

func TestRequestsRollupEmpty(t *testing.T) {
	s := openTest(t)
	t.Cleanup(func() { _ = s.Close() })
	got, err := s.RequestsRollup(0, 10_000, 1_000)
	if err != nil {
		t.Fatalf("RequestsRollup empty: %v", err)
	}
	if got.Total != 0 || got.OK != 0 || got.Errors != 0 || got.ErrorRate != 0 {
		t.Fatalf("empty rollup = %+v, want zeros", got)
	}
	if got.TTFB.Count != 0 {
		t.Fatalf("empty TTFB count = %d, want 0", got.TTFB.Count)
	}
}

func TestRequestsRollupBuckets(t *testing.T) {
	s := seedRollup(t)
	got, err := s.RequestsRollup(0, 6_000, 2_000)
	if err != nil {
		t.Fatalf("RequestsRollup: %v", err)
	}
	if got.Total != 5 || got.OK != 3 || got.Errors != 2 {
		t.Fatalf("counts = %d/%d/%d, want 5/3/2", got.Total, got.OK, got.Errors)
	}
	if got.ErrorRate != 0.4 {
		t.Fatalf("error rate = %v, want 0.4", got.ErrorRate)
	}
	if len(got.Buckets) != 3 {
		t.Fatalf("buckets = %d, want 3", len(got.Buckets))
	}
	if got.Buckets[0].TS != 0 || got.Buckets[0].Total != 1 {
		t.Fatalf("bucket0 = %+v, want TS 0 total 1", got.Buckets[0])
	}
	if got.Buckets[2].TS != 4000 || got.Buckets[2].Total != 2 {
		t.Fatalf("bucket2 = %+v, want TS 4000 total 2", got.Buckets[2])
	}
}

func TestRequestsRollupGroups(t *testing.T) {
	s := seedRollup(t)
	got, err := s.RequestsRollup(0, 6_000, 6_000)
	if err != nil {
		t.Fatalf("RequestsRollup: %v", err)
	}
	if len(got.ByModel) != 2 {
		t.Fatalf("models = %d, want 2", len(got.ByModel))
	}
	if got.ByModel[0].Name != "m/a" || got.ByModel[0].Total != 3 || got.ByModel[0].Errors != 1 {
		t.Fatalf("model0 = %+v, want m/a 3/1", got.ByModel[0])
	}
	if len(got.ByToken) != 3 {
		t.Fatalf("tokens = %+v, want 3 groups", got.ByToken)
	}
	if got.ByToken[0].TokenIdx != -1 || got.ByToken[0].Total != 2 {
		t.Fatalf("token0 = %+v, want idx -1 total 2", got.ByToken[0])
	}
	if len(got.ByError) != 2 || got.ByError[0].Name != "auth" || got.ByError[0].Total != 1 {
		t.Fatalf("byError = %+v, want [auth:1 upstream:1]", got.ByError)
	}
}

func TestRequestsRollupTTFBPercentiles(t *testing.T) {
	s := seedRollup(t)
	got, err := s.RequestsRollup(0, 6_000, 6_000)
	if err != nil {
		t.Fatalf("RequestsRollup: %v", err)
	}
	// Sorted TTFBs: 100 200 300 400 500; idx = q*(5-1).
	if got.TTFB.Count != 5 || got.TTFB.P50 != 300 || got.TTFB.P90 != 400 || got.TTFB.P99 != 400 {
		t.Fatalf("ttfb = %+v, want count 5 p50 300 p90 400 p99 400", got.TTFB)
	}
}

func TestRequestsRollupWindowClamp(t *testing.T) {
	s := seedRollup(t)
	// Window wider than the 168h retention bound clamps instead of scanning.
	got, err := s.RequestsRollup(0, RollupMaxWindowMillis+60_000, 60_000)
	if err != nil {
		t.Fatalf("RequestsRollup clamped: %v", err)
	}
	if got.Since != 60_000 {
		t.Fatalf("clamped since = %d, want 60000", got.Since)
	}
	if _, err := s.RequestsRollup(5_000, 5_000, 1_000); err == nil {
		t.Fatal("since == until: want error, got nil")
	}
	if _, err := s.RequestsRollup(0, 6_000, 0); err == nil {
		t.Fatal("bucket 0: want error, got nil")
	}
	if _, err := s.RequestsRollup(-1, 6_000, 1_000); err == nil {
		t.Fatal("negative since: want error, got nil")
	}
}

func TestImportRequestsValidation(t *testing.T) {
	s := openTest(t)
	t.Cleanup(func() { _ = s.Close() })
	counts, err := s.ImportRequests([]RequestRecord{
		{ReqID: "ok1", TS: 1_000, Endpoint: "/v1/chat/completions", Model: "m", Status: "ok"},
		{ReqID: "", TS: 1_000, Endpoint: "/v1/chat/completions", Model: "m", Status: "ok"},
		{ReqID: "bad", TS: 1_000, Endpoint: "/v1/chat/completions", Model: "m", Status: "bogus"},
	})
	if err != nil {
		t.Fatalf("ImportRequests: %v", err)
	}
	if counts.Imported != 1 || counts.Rejected != 2 {
		t.Fatalf("counts = %+v, want imported 1 rejected 2", counts)
	}
	dup, err := s.ImportRequests([]RequestRecord{
		{ReqID: "ok1", TS: 1_000, Endpoint: "/v1/chat/completions", Model: "m", Status: "ok"},
	})
	if err != nil {
		t.Fatalf("ImportRequests dup: %v", err)
	}
	if dup.Imported != 0 || dup.SkippedDuplicate != 1 {
		t.Fatalf("dup counts = %+v, want skipped 1", dup)
	}
}
