// quotaerr.go — quota-exhaustion classification and bridge quota helpers
// (issue #85/#178). Premium scarce-session protection was removed in the
// session redesign: model switches release the previous slot instead of
// holding it (see session.EnsureSessionForModel).
package pool

import (
	"strings"

	"freebuff-proxy/backend/internal/upstream"
)

// isQuotaExhaustedError reports whether rle represents a session quota exhaustion
// (recentCount >= limit or local session quota error) as opposed to a transient rate limit.
// Issue #178: a refusal carrying a reset timestamp or a pacific_day/
// pacific_week/pacific_month period is quota-shaped too — those are the quota
// windows the upstream serves on daily/weekly/monthly caps (monthly added in
// wire drift 2026-09-04, issue #330), so the pool treats them as per-model
// quota exhaustion and lets the token keep serving its other models.
func isQuotaExhaustedError(rle *upstream.RateLimitError) bool {
	if rle == nil {
		return false
	}
	if !rle.ResetAt.IsZero() || rle.Period == "pacific_day" || rle.Period == "pacific_week" || rle.Period == "pacific_month" || isDailyCapReset(rle) {
		return true
	}
	if rle.Limit > 0 && rle.RecentCount >= rle.Limit {
		return true
	}
	if rle.Body == "session quota exhausted for model" || strings.Contains(rle.Body, "referral entitlement required") || strings.Contains(rle.Body, "no referral quota") {
		return true
	}
	return false
}

// canServeOtherModel reports whether a remembered rate-limit cooldown is
// isolated to another model, so the token may still serve model (issues
// #155/#178: a quota cap on one model must not block the token's others).
// It is the single definition of the per-model bypass shared by the
// failover loop, the hot-first ordering, the bridge gate and the
// smart-routing score, so a kind that is never per-model stays parked
func canServeOtherModel(rle *upstream.RateLimitError, model string) bool {
	if rle == nil || rle.Model == "" || rle.Model == model {
		return false
	}
	// The vendor's freebucks-window ceiling is account-wide, not per-model:
	// the account cannot cover ANY next session's price until the window
	// resets, so a window refusal never isolates to another model. Parking
	// the token for every model also keeps the refresh path from reaching
	// the release-then-create switch (which would DELETE the surviving
	// session for a doomed admission). A plain rate_limited and every other
	// refusal keep the per-model bypass exactly as before.
	if rle.WindowKind() == upstream.WindowKindFreebucks {
		return false
	}
	return isQuotaExhaustedError(rle)
}

// isDailyCapReset mirrors upstream.isDailyCapReset: a no-timestamp 429 body
// signals a genuine daily-cap reset when the quota period is
// pacific_day/pacific_week/pacific_month AND the recent counter is at/over
// the limit (the session-quota bodies the CLI serves on daily-cap refusals).
// The pool needs its own copy because the upstream helper is unexported.
func isDailyCapReset(rle *upstream.RateLimitError) bool {
	if rle.Period != "pacific_day" && rle.Period != "pacific_week" && rle.Period != "pacific_month" {
		return false
	}
	return rle.Limit > 0 && rle.RecentCount >= rle.Limit
}
