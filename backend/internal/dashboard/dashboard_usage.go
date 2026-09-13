package dashboard

import (
	"net/http"
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
// window encodes as []).
type usageData struct {
	Range   string        `json:"range"`
	Totals  usageTotals   `json:"totals"`
	Entries []UsageRecord `json:"entries"`
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

// usageRangeCutoff normalizes the ?range= value and returns the echoed
// name plus the inclusive lower bound (Unix-millis compare). "today" is
// local-midnight-to-now; the rest are rolling windows. Unknown or absent
// values fall back to "today" so the dashboard never 400s on a bad tab.
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
// 0 when the model is unpriced (nil map included).
func (d *Dashboard) usageData(r *http.Request) usageData {
	now := time.Now()
	rangeParam := ""
	if r != nil && r.URL != nil {
		rangeParam = r.URL.Query().Get("range")
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
	return out
}
