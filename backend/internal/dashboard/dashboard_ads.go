package dashboard

// dashboard_ads.go — ad-leg firing view models: the retained-window
// aggregates plus the recent event list the Ads section renders.
//
// The ledger lives in the pool package (pool/ads_ledger.go, lane B); this
// file only projects it into dashboard view models. Events carry
// titles/brands only — impUrl/clickUrl never enter the ledger, so they
// cannot leak through these endpoints.

import (
	"freebucks-proxy/backend/internal/pool"
	"net/http"
	"strconv"
)

// adsSummaryData serves GET /admin/api/ads/summary: retained-window totals,
// credits granted, per-provider/per-surface counts, last-event instant, and
// the error count. Field order follows the fixed API contract.
func (d *Dashboard) adsSummaryData() pool.AdSummary {
	return pool.AdSummarySnapshot()
}

// adsLegsData serves GET /admin/api/ads/legs: retained events newest-first.
// ?limit=N selects the page size (default pool.DefaultAdLegsLimit; over-cap
// values clamp to the ring).
func (d *Dashboard) adsLegsData(r *http.Request) []pool.AdLegEvent {
	n := pool.DefaultAdLegsLimit
	if r != nil && r.URL != nil {
		if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
			n = v
		}
	}
	return pool.AdLegs(n)
}
