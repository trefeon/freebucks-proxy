package dashboard

import (
	"net/http"
	"sort"
	"time"
)

// Token-usage log (9Router-style Usage Overview): every completed chat
// appends one UsageRecord via RecordUsage; GET /admin/api/usage serves
// range-filtered totals plus the per-entry detail rows. Storage is an
// in-memory ring on the Dashboard (no SQLite usage table exists —
// pool_state holds only opaque ledger/admission blobs), so history starts
// empty on every restart. Zero new knobs: the cap is a constant.

// maxUsageRecords caps the in-memory usage ring; older records evict
// first. Sized for 60 days of heavy use without noticeable memory.
const maxUsageRecords = 5000

// UsageRecord is one completed request's token account. Field order is the
// capture contract (BackendUsageCapture feeds these from the relay usage
// blocks); JSON keys are the GET /admin/api/usage entries shape.
type UsageRecord struct {
	TsMs      int64  `json:"ts_ms"`
	ReqID     string `json:"req_id"`
	Model     string `json:"model"`
	Input     int64  `json:"input"`
	Output    int64  `json:"output"`
	Cached    int64  `json:"cached"`
	Reasoning int64  `json:"reasoning"`
	Total     int64  `json:"total"`
	OK        bool   `json:"ok"`
	// ClientKeyHash is the caller's pooled API-key identity
	// (hex(sha256(rawKey))[:16]), "" for bridge/no-key requests. The raw
	// key never reaches this ring.
	ClientKeyHash string `json:"client_key_hash,omitempty"`
}

// RecordUsage appends one usage record to the ring, evicting the oldest
// past the cap. A zero TsMs stamps now so capture call sites that only
// have "just completed" still log correctly. Nil-receiver safe: the engine
// may hold a nil dashboard in tests.
func (d *Dashboard) RecordUsage(rec UsageRecord) {
	if d == nil {
		return
	}
	if rec.TsMs == 0 {
		rec.TsMs = time.Now().UnixMilli()
	}
	d.usageMu.Lock()
	defer d.usageMu.Unlock()
	d.usageRing = append(d.usageRing, rec)
	if len(d.usageRing) > maxUsageRecords {
		d.usageRing = append([]UsageRecord(nil), d.usageRing[len(d.usageRing)-maxUsageRecords:]...)
	}
}

// usageTotals is the range summary: request and token sums plus a
// session-cost tally. Cost sums the wire per-session Freebucks price for
// each entry's model (firstFreebucksPrices, the same map the quota labels
// and the pool admission gate on), 0 for unpriced models. It scales with
// request count, not token volume: the wire map is Freebucks per session
// hour with no per-token rate, so no token-derived estimate is possible.
// Unit is Freebucks, not USD.
type usageTotals struct {
	Requests int64   `json:"requests"`
	Input    int64   `json:"input"`
	Cached   int64   `json:"cached"`
	Output   int64   `json:"output"`
	Cost     float64 `json:"cost"`
}

// usageData is the GET /admin/api/usage answer: the normalized range, the
// totals over it, and the entries newest-first (never null — an empty
// window encodes as []). Keys is the per-client-key aggregation, populated
// only for ?group_by=key (omitempty keeps the bare shape byte-identical).
type usageData struct {
	Range   string          `json:"range"`
	Totals  usageTotals     `json:"totals"`
	Entries []UsageRecord   `json:"entries"`
	Keys    []usageKeyEntry `json:"keys,omitempty"`
}

// usageKeyModelStats is one key's token split for one model.
type usageKeyModelStats struct {
	Requests   int64 `json:"requests"`
	Tokens     int64 `json:"tokens"`
	Prompt     int64 `json:"prompt"`
	Completion int64 `json:"completion"`
	Reasoning  int64 `json:"reasoning"`
}

// usageKeyEntry is one client key's range aggregate for ?group_by=key:
// key_id is the hex(sha256(rawKey))[:16] identity ("" buckets bridge and
// no-key requests), success_rate is the 0..1 share of OK entries,
// first_seen/last_seen are Unix-millis, freebucks sums the wire
// per-session price per entry (same unit as usageTotals cost).
type usageKeyEntry struct {
	KeyID       string                        `json:"key_id"`
	Requests    int64                         `json:"requests"`
	SuccessRate float64                       `json:"success_rate"`
	TotalTokens int64                         `json:"total_tokens"`
	ByModel     map[string]usageKeyModelStats `json:"by_model"`
	FirstSeen   int64                         `json:"first_seen"`
	LastSeen    int64                         `json:"last_seen"`
	Freebucks   float64                       `json:"freebucks"`
}

// aggregateUsageByKey folds range-filtered entries into per-key aggregates,
// sorted by key_id for a deterministic wire order. Pure: no dashboard
// state, prices is the wire per-session Freebucks map (nil-safe).
func aggregateUsageByKey(entries []UsageRecord, prices map[string]float64) []usageKeyEntry {
	byKey := map[string]*usageKeyEntry{}
	for _, rec := range entries {
		agg, ok := byKey[rec.ClientKeyHash]
		if !ok {
			agg = &usageKeyEntry{KeyID: rec.ClientKeyHash, ByModel: map[string]usageKeyModelStats{}}
			byKey[rec.ClientKeyHash] = agg
		}
		if agg.Requests == 0 || rec.TsMs < agg.FirstSeen {
			agg.FirstSeen = rec.TsMs
		}
		if rec.TsMs > agg.LastSeen {
			agg.LastSeen = rec.TsMs
		}
		agg.Requests++
		if rec.OK {
			// SuccessRate derives at the end; count OK rows via the
			// rate accumulator scaled back on finalize.
			agg.SuccessRate++
		}
		agg.TotalTokens += rec.Total
		m := agg.ByModel[rec.Model]
		m.Requests++
		m.Tokens += rec.Total
		m.Prompt += rec.Input
		m.Completion += rec.Output
		m.Reasoning += rec.Reasoning
		agg.ByModel[rec.Model] = m
		if p, ok := prices[rec.Model]; ok {
			agg.Freebucks += p
		}
	}
	out := make([]usageKeyEntry, 0, len(byKey))
	for _, agg := range byKey {
		if agg.Requests > 0 {
			agg.SuccessRate /= float64(agg.Requests)
		}
		out = append(out, *agg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].KeyID < out[j].KeyID })
	return out
}

func usageRangeCutoff(rangeName string, now time.Time) (string, time.Time) {
	switch rangeName {
	case "24h":
		return "24h", now.Add(-24 * time.Hour)
	case "7d":
		return "7d", now.Add(-7 * 24 * time.Hour)
	case "30d":
		return "30d", now.Add(-30 * 24 * time.Hour)
	case "60d":
		return "60d", now.Add(-60 * 24 * time.Hour)
	default:
		y, m, day := now.Date()
		return "today", time.Date(y, m, day, 0, 0, 0, 0, now.Location())
	}
}

// usageData serves the range-filtered usage view: totals plus entries
// newest-first. Cost per entry is the wire per-session price for its model,
// 0 when the model is unpriced (nil map included). With ?group_by=key the
// same window additionally carries Keys, the per-client-key aggregation —
// the bare shape stays byte-identical (Keys omits when ungrouped).
func (d *Dashboard) usageData(r *http.Request) usageData {
	now := time.Now()
	rangeParam := ""
	groupBy := ""
	if r != nil && r.URL != nil {
		rangeParam = r.URL.Query().Get("range")
		groupBy = r.URL.Query().Get("group_by")
	}
	name, cutoff := usageRangeCutoff(rangeParam, now)
	cutoffMs := cutoff.UnixMilli()
	prices := d.firstFreebucksPrices()
	out := usageData{Range: name, Entries: []UsageRecord{}}
	d.usageMu.Lock()
	ring := make([]UsageRecord, len(d.usageRing))
	copy(ring, d.usageRing)
	d.usageMu.Unlock()
	// Ring is oldest-first; walk back so entries render newest-first.
	for i := len(ring) - 1; i >= 0; i-- {
		rec := ring[i]
		if rec.TsMs < cutoffMs {
			continue
		}
		out.Entries = append(out.Entries, rec)
		out.Totals.Requests++
		out.Totals.Input += rec.Input
		out.Totals.Cached += rec.Cached
		out.Totals.Output += rec.Output
		if p, ok := prices[rec.Model]; ok {
			out.Totals.Cost += p
		}
	}
	if groupBy == "key" {
		out.Keys = aggregateUsageByKey(out.Entries, prices)
		if out.Keys == nil {
			out.Keys = []usageKeyEntry{}
		}
	}
	return out
}
