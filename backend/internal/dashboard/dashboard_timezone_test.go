package dashboard

import (
	"testing"
	"time"

	"freebucks-proxy/backend/internal/upstream"
)

// TestFreebucksWindowResetAtUTC pins the server-truth time contract: every
// Freebucks allowance window carries its refill instant twice — reset_at_utc
// (the authoritative countdown anchor, absolute UTC RFC3339) alongside the
// legacy reset_at twin. Countdowns and wall clocks MUST read reset_at_utc.
func TestFreebucksWindowResetAtUTC(t *testing.T) {
	reset := time.Date(2026, 10, 2, 7, 0, 0, 0, time.UTC)
	card := freebucksCardFromInfo(&upstream.FreebucksInfo{
		Daily:   upstream.FreebucksWindow{Limit: 20, Spent: 5, Remaining: 15, ResetAt: reset},
		Monthly: &upstream.FreebucksMonthlyAllowance{LimitUsd: 50, SpentUsd: 10, RemainingUsd: 40, ResetAt: reset},
	})
	if card == nil {
		t.Fatal("freebucksCardFromInfo = nil, want card")
	}
	for name, win := range map[string]freebucksWindowCard{"daily": card.Daily, "monthly": *card.Monthly} {
		if win.ResetAtUTC != "2026-10-02T07:00:00Z" {
			t.Errorf("%s reset_at_utc = %q, want 2026-10-02T07:00:00Z", name, win.ResetAtUTC)
		}
		if win.ResetAt != win.ResetAtUTC {
			t.Errorf("%s reset_at = %q, want legacy twin %q", name, win.ResetAt, win.ResetAtUTC)
		}
		if _, err := time.Parse(time.RFC3339, win.ResetAtUTC); err != nil {
			t.Errorf("%s reset_at_utc %q does not parse as RFC3339: %v", name, win.ResetAtUTC, err)
		}
	}
	// Zero reset stays omitted on both keys, never a zero time stated as fact.
	empty := freebucksCardFromInfo(&upstream.FreebucksInfo{})
	if empty.Daily.ResetAt != "" || empty.Daily.ResetAtUTC != "" {
		t.Errorf("zero-reset daily = %q/%q, want both empty", empty.Daily.ResetAt, empty.Daily.ResetAtUTC)
	}
}
