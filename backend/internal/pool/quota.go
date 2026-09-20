package pool

import (
	"fmt"
	"freebucks-proxy/backend/internal/modelcat"
	"freebucks-proxy/backend/internal/session"
	"freebucks-proxy/backend/internal/upstream"
	"sort"
	"time"
)

// freebucksCapped reports whether the token's Freebucks allowance is exhausted
// for model (issue #321 wire drift: balance is now the server-computed
// spendable = daily.remaining + wallet.balance; vendor af898dc adds eligible
// earned grants admission may convert, so the gate uses Spendable() =
// balance + claimableGrantFreebucks, mirroring getFreebucksModelMeter).
// When Freebucks is absent or the model has no price, the token is not
// capped. The token is capped when spendable < price, or when the monthly
// dollar allowance is spent (wire drift 2026-09-04, issue #330 — fresh
// sessions stop upstream regardless of the daily balance). RetryAfter is the
// earliest future recovery instant among the applicable windows. When every
// recovery instant is past or unknown, the stored numbers are self-declared
// stale and the token is NOT capped — one admission revalidates live truth
// (polls never carry Freebucks, so nothing else could refresh them).
func freebucksCapped(acc tokenAccount, model string) (bool, time.Duration) {
	return freebucksCappedForSnapshot(acc.sessionMgr().Snapshot(), model)
}

// EffectiveFreebucksPrices projects the server's announced repricing schedule
// and recurring off-peak policy onto a stored quote at read time (mirrors
// applyFreebucksPriceChanges in common/src/util/freebuff-price-changes.ts):
// due price-changes (at <= now, chronological) apply first, then the off-peak
// daily window (authoritative last). Only models already on the meter
// reprice, discount-adjusted while the first-tab offer is available, and
// their notice copy refreshes. Pure read: fb is never mutated, so persisted
// snapshots cannot race live state; when nothing is due the stored maps
// return as-is (no allocation). New-session quotes only: balances, exemption,
// monthly allowance, and admitted-session charges never change. ListPrices
// stays display-only and never gates.
func EffectiveFreebucksPrices(fb *upstream.FreebucksInfo, now time.Time) (map[string]float64, map[string]string) {
	if fb == nil {
		return nil, nil
	}
	var ready []upstream.FreebucksPriceChange
	for _, c := range fb.PriceChanges {
		at, err := time.Parse(time.RFC3339, c.At)
		if err != nil || at.After(now) {
			continue
		}
		ready = append(ready, c)
	}
	sort.SliceStable(ready, func(i, j int) bool {
		ai, _ := time.Parse(time.RFC3339, ready[i].At)
		aj, _ := time.Parse(time.RFC3339, ready[j].At)
		return ai.Before(aj)
	})
	if len(ready) == 0 && len(fb.OffPeak) == 0 {
		return fb.Prices, fb.PriceNotices
	}
	discount := 0.0
	if fb.FirstTabDiscount != nil && fb.FirstTabDiscount.Available {
		discount = fb.FirstTabDiscount.Amount
	}
	type priceAdj struct {
		price   float64
		tagline string
	}
	var final map[string]priceAdj
	apply := func(modelID string, raw float64, tagline string) {
		if _, ok := fb.Prices[modelID]; !ok {
			return
		}
		if final == nil {
			final = make(map[string]priceAdj)
		}
		final[modelID] = priceAdj{price: upstream.DiscountedSessionPrice(raw, discount), tagline: tagline}
	}
	for _, c := range ready {
		apply(c.ModelID, c.Price, c.Tagline)
	}
	for id, offer := range fb.OffPeak {
		price, tagline := offPeakQuoteAt(offer, now)
		apply(id, price, tagline)
	}
	if len(final) == 0 {
		return fb.Prices, fb.PriceNotices
	}
	// The stored quote may already reflect every adjustment (parse-time
	// Apply ran at or after the transition): then keep the stored maps.
	changed := false
	for id, adj := range final {
		if p, ok := fb.Prices[id]; !ok || p != adj.price {
			changed = true
			break
		}
		if fb.PriceNotices[id] != adj.tagline {
			changed = true
			break
		}
	}
	if !changed {
		return fb.Prices, fb.PriceNotices
	}
	prices := make(map[string]float64, len(fb.Prices))
	for k, v := range fb.Prices {
		prices[k] = v
	}
	var notices map[string]string
	if fb.PriceNotices != nil {
		notices = make(map[string]string, len(fb.PriceNotices)+len(final))
		for k, v := range fb.PriceNotices {
			notices[k] = v
		}
	} else {
		notices = make(map[string]string, len(final))
	}
	for id, adj := range final {
		prices[id] = adj.price
		notices[id] = adj.tagline
	}
	return prices, notices
}

// offPeakQuoteAt resolves one server-owned daily off-peak policy at now
// (mirrors offPeakPriceAt in common/src/util/freebuff-price-changes.ts):
// the daily [startHourUtc, endHourUtc) UTC window (end may cross midnight)
// prices at the off-peak price; outside it the regular price holds. Returns
// the raw pre-discount price and the picker tagline.
func offPeakQuoteAt(offer upstream.FreebuffOffPeakPrice, now time.Time) (float64, string) {
	u := now.UTC()
	start := time.Date(u.Year(), u.Month(), u.Day(), offer.StartHourUtc, 0, 0, 0, time.UTC)
	if start.After(u) {
		start = start.Add(-24 * time.Hour)
	}
	end := time.Date(start.Year(), start.Month(), start.Day(), offer.EndHourUtc, 0, 0, 0, time.UTC)
	if !end.After(start) {
		end = end.Add(24 * time.Hour)
	}
	if u.Before(end) {
		return offer.Price, "Off-peak pricing · " + formatFreebucksBalance(offer.RegularPrice) + " Freebucks/hour at peak"
	}
	return offer.RegularPrice, "Peak pricing · " + formatFreebucksBalance(offer.Price) + " Freebucks/hour off-peak"
}

// freebucksCappedForSnapshot is the snapshot-direct form of freebucksCapped
// (kept for testing and for spillOrder's quotaLimited loop which already
// holds a snapshot).
func freebucksCappedForSnapshot(snap session.SessionSnapshot, model string) (bool, time.Duration) {
	fb := snap.Freebucks
	if fb == nil {
		return false, 0
	}
	// Reuse bypass: a token holding a live reusable session for THIS model
	// is never capped — reuse performs zero admission POST, so the balance
	// is irrelevant, and a later chat-path refusal is owned by the existing
	// chat handling. Applies to balance- and monthly-based caps alike (the
	// gate predicts a fresh admission's cost; a live session pays none).
	// Deliberately conservative: "ended"+grace sessions are NOT bypassed —
	// a walk attempt may still reuse them, but the pre-filter stays strict.
	if snap.Status == "active" && snap.InstanceID != "" &&
		(snap.Model == "" || snap.Model == model) &&
		(snap.ExpiresAt.IsZero() || time.Now().Before(snap.ExpiresAt)) {
		return false, 0
	}
	// New-session quote at read time: due repricings and the off-peak window
	// project onto the stored prices (vendor applyFreebucksPriceChanges).
	// Balances, exemption, and monthly logic below are untouched, and
	// ListPrices never gates (display-only).
	now := time.Now()
	prices, _ := EffectiveFreebucksPrices(fb, now)
	price, ok := prices[model]
	if !ok {
		if modelcat.IsPremium(model) {
			price = 1.0
		} else {
			return false, 0
		}
	}
	// Monthly dollar allowance (wire drift 2026-09-04, issue #330): when
	// the period is spent, fresh sessions stop upstream regardless of the
	// daily balance. Absent on older servers (nil) — no behavior change.
	monthlySpent := fb.Monthly != nil && fb.Monthly.RemainingUsd <= 0
	// Server-authorized quota exemption (wire drift 2026-09-05, issue
	// #350): new sessions stay usable at zero balance — the meter's
	// canStart is exempt || balance >= price. The monthly allowance still
	// gates (separate upstream refusal).
	if fb.QuotaExempt && !monthlySpent {
		return false, 0
	}
	// Claimable earned grants count toward canStart (vendor af898dc
	// getFreebucksModelMeter: balance + claimableGrantFreebucks >= price).
	if fb.Spendable() >= price && !monthlySpent {
		return false, 0
	}
	// Capped. Recovery signals: the daily pool refill, the plan's next
	// wallet bonus, and the monthly allowance reset (when the monthly
	// period is what blocks). Take the earliest future instant; when
	// nothing is known, surface 0.
	earliest := time.Time{}
	hasPastCandidate := false
	candidates := []time.Time{fb.Daily.ResetAt, fb.Wallet.NextBonusAt}
	if monthlySpent {
		candidates = append(candidates, fb.Monthly.ResetAt)
	}
	for _, t := range candidates {
		if t.IsZero() {
			continue
		}
		if !t.After(now) {
			hasPastCandidate = true
			continue
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	if earliest.IsZero() {
		if hasPastCandidate {
			// All recovery instants past: the stored numbers are self-declared
			// stale (their own windows passed). Treat as unknown so one admission
			// revalidates against live upstream truth.
			return false, 0
		}
		// When no explicit reset timestamp was provided by upstream, fall back
		// to the next Pacific midnight (daily Freebucks refill). An account
		// with exhausted Freebucks balance must be skipped rather than
		// repeatedly failing admission live against upstream.
		earliest = nextPacificMidnight(now)
	}
	return true, time.Until(earliest)
}

// freebucksLimitError builds the 429 surfaced when Freebucks balance is
// insufficient for model. RetryAfter mirrors freebucksCapped's window-reset
// signal.
func freebucksLimitError(acc tokenAccount, model string) *upstream.RateLimitError {
	return freebucksLimitErrorForSnapshot(acc.sessionMgr().Snapshot(), model)
}

func freebucksLimitErrorForSnapshot(snap session.SessionSnapshot, model string) *upstream.RateLimitError {
	fb := snap.Freebucks
	price := 0.0
	if fb != nil {
		// Same read-time quote as the gate: the diagnostic reports the
		// price admission actually gated on (indexing a nil map is safe).
		prices, _ := EffectiveFreebucksPrices(fb, time.Now())
		if p, ok := prices[model]; ok {
			price = p
		}
	}
	capped, retryAfter := freebucksCappedForSnapshot(snap, model)
	_ = capped
	body := "freebucks balance insufficient for model"
	// Surface price vs spendable in the diagnostic body when available
	// (spendable = balance + claimable grants, the gated amount).
	if fb != nil {
		body = body + " (spendable " + formatFreebucksBalance(fb.Spendable()) + " < price " + formatFreebucksBalance(price) + ")"
		if fb.Monthly != nil && fb.Monthly.RemainingUsd <= 0 {
			body = "freebucks monthly allowance exhausted for model"
		}
	}
	return &upstream.RateLimitError{
		Status:     "rate_limited",
		Model:      model,
		RetryAfter: retryAfter,
		Body:       body,
	}
}

func formatFreebucksBalance(v float64) string {
	return fmt.Sprintf("%g", v)
}

// recordChat appends one successful upstream chat for token and prunes the
// token's usage history outside the 24h window. The ledger travels with the
// entry (issue #263), so the roster's single mutex guards it.
func (p *Pool) recordChat(token int) { p.roster.recordChat(token) }

// recordChatEntry appends one successful upstream chat for the lease's
// backing entry by pointer and prunes its usage history outside the 24h
// window. The entry is the authoritative owner of its ledger, so after a
// concurrent RemoveLastToken+AddToken a lease's Token index is never used
// to locate the ledger — the pointer stays immune to index reuse.
func (p *Pool) recordChatEntry(entry *tokenEntry) {
	p.roster.recordChatEntry(entry)
	p.markPersistDirty()
}

// usageCount returns how many successful chats token sent within the last
// usageWindow, pruning expired timestamps. Feeds the dashboard messages_24h
// display; upstream quota/429 is the enforcement.
func (p *Pool) usageCount(token int) int { return p.roster.usageCount(token) }

// dayRequestCount returns how many successful chats token sent in the
// current Pacific day, rolling the bucket at Pacific midnight. Read by the
// dashboard per-day display, the maturity client-active skip, and maturity
// touch guards; upstream quota/429 is the enforcement.
func (p *Pool) dayRequestCount(token int) int { return p.roster.dayRequestCount(token) }
