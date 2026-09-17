package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestAggregateUsageByKey pins the ?group_by=key contract: per-key request
// counts, 0..1 success rates, token totals, per-model splits, first/last
// seen bounds, wire-price freebucks, deterministic key_id order, and the ""
// bucket for bridge/no-key requests.
func TestAggregateUsageByKey(t *testing.T) {
	now := time.Now().UnixMilli()
	entries := []UsageRecord{
		{TsMs: now - 3000, ReqID: "a1", Model: "test/priced", Input: 100, Output: 50, Total: 150, OK: true, ClientKeyHash: "aaaabbbbccccdddd"},
		{TsMs: now - 2000, ReqID: "a2", Model: "test/priced", Input: 10, Output: 5, Reasoning: 4, Total: 15, OK: false, ClientKeyHash: "aaaabbbbccccdddd"},
		{TsMs: now - 1000, ReqID: "b1", Model: "test/unpriced", Input: 7, Output: 3, Total: 10, OK: true, ClientKeyHash: "zzzz000011112222"},
		{TsMs: now, ReqID: "c1", Model: "test/priced", Input: 1, Output: 1, Total: 2, OK: true},
	}
	prices := map[string]float64{"test/priced": 20}
	keys := aggregateUsageByKey(entries, prices)
	if len(keys) != 3 {
		t.Fatalf("keys = %d entries, want 3 (two hashes plus the no-key bucket)", len(keys))
	}
	// Deterministic key_id order: "" sorts first.
	if keys[0].KeyID != "" || keys[1].KeyID != "aaaabbbbccccdddd" || keys[2].KeyID != "zzzz000011112222" {
		t.Fatalf("key order = [%q %q %q], want [\"\" aaa… zzz…]", keys[0].KeyID, keys[1].KeyID, keys[2].KeyID)
	}
	a := keys[1]
	if a.Requests != 2 || a.TotalTokens != 165 {
		t.Errorf("key a requests/tokens = %d/%d, want 2/165", a.Requests, a.TotalTokens)
	}
	if a.SuccessRate != 0.5 {
		t.Errorf("key a success_rate = %v, want 0.5 (one OK of two)", a.SuccessRate)
	}
	if a.FirstSeen != now-3000 || a.LastSeen != now-2000 {
		t.Errorf("key a first/last = %d/%d, want %d/%d", a.FirstSeen, a.LastSeen, now-3000, now-2000)
	}
	if a.Freebucks != 40 {
		t.Errorf("key a freebucks = %v, want 40 (two priced entries at 20)", a.Freebucks)
	}
	m, ok := a.ByModel["test/priced"]
	if !ok {
		t.Fatal("key a by_model lacks test/priced")
	}
	if m.Requests != 2 || m.Tokens != 165 || m.Prompt != 110 || m.Completion != 55 || m.Reasoning != 4 {
		t.Errorf("key a by_model = %+v, want {2 165 110 55 4}", m)
	}
	b := keys[2]
	if b.Requests != 1 || b.SuccessRate != 1 || b.Freebucks != 0 {
		t.Errorf("key b = %+v, want {1 req, rate 1, 0 freebucks (unpriced)}", b)
	}
	c := keys[0]
	if c.Requests != 1 || c.TotalTokens != 2 || c.Freebucks != 20 {
		t.Errorf("no-key bucket = %+v, want {1 req, 2 tokens, 20 freebucks}", c)
	}
	if got := aggregateUsageByKey(nil, nil); len(got) != 0 {
		t.Errorf("nil entries = %d keys, want 0 (never null on the wire)", len(got))
	}
}

// TestUsageGroupByKey pins the HTTP shape: ?group_by=key carries the keys
// aggregation over the same range window, while the bare shape encodes no
// "keys" member at all (backward-compatible bytes).
func TestUsageGroupByKey(t *testing.T) {
	d := usageTestDashboard(t)
	now := time.Now()
	d.RecordUsage(UsageRecord{TsMs: now.UnixMilli(), ReqID: "k1", Model: "test/priced", Input: 100, Output: 50, Total: 150, OK: true, ClientKeyHash: "aaaabbbbccccdddd"})
	d.RecordUsage(UsageRecord{TsMs: now.Add(-240 * time.Hour).UnixMilli(), ReqID: "old", Model: "test/priced", Input: 1, Output: 1, Total: 2, OK: true, ClientKeyHash: "aaaabbbbccccdddd"})

	rec := httptest.NewRecorder()
	d.APIHandler("usage")(rec, httptest.NewRequest(http.MethodGet, "/admin/api/usage?range=7d&group_by=key", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("group_by=key status = %d, want 200", rec.Code)
	}
	var grouped struct {
		Range string          `json:"range"`
		Keys  []usageKeyEntry `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &grouped); err != nil {
		t.Fatalf("grouped response is not valid JSON: %v (%q)", err, rec.Body.String())
	}
	if grouped.Range != "7d" {
		t.Errorf("grouped range = %q, want 7d (same window as the range view)", grouped.Range)
	}
	if len(grouped.Keys) != 1 || grouped.Keys[0].KeyID != "aaaabbbbccccdddd" || grouped.Keys[0].Requests != 1 {
		t.Errorf("grouped keys = %+v, want the single in-window key with 1 request", grouped.Keys)
	}

	bare := httptest.NewRecorder()
	d.APIHandler("usage")(bare, httptest.NewRequest(http.MethodGet, "/admin/api/usage?range=7d", nil))
	if bare.Code != http.StatusOK {
		t.Fatalf("bare status = %d, want 200", bare.Code)
	}
	if strings.Contains(bare.Body.String(), `"keys"`) {
		t.Errorf("bare body contains a keys member, want byte-compatible shape without it: %s", bare.Body.String())
	}
}
