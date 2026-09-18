package pool

import (
	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/upstream"
	"testing"
	"time"
)

const effectiveTestModel = "openai/gpt-5.6-luna"

func effectiveTestFB(balance float64, price float64) *upstream.FreebucksInfo {
	return &upstream.FreebucksInfo{
		Balance: balance,
		Daily:   upstream.FreebucksWindow{Limit: 20, Spent: 15, Remaining: 5, ResetAt: time.Now().Add(time.Hour)},
		Prices:  map[string]float64{effectiveTestModel: price},
	}
}

// TestFreebucksCappedAppliesDuePriceChange pins the read-time quote: a
// repricing that fell due after the last parse gates admission without a
// reprobe, and the read never mutates the stored snapshot. Future and
// unparsable changes are ignored.
func TestFreebucksCappedAppliesDuePriceChange(t *testing.T) {
	// Stored 2, due change to 10, spendable 5: the stored price reads
	// affordable, the effective price caps.
	due := effectiveTestFB(5, 2)
	due.PriceChanges = []upstream.FreebucksPriceChange{
		{At: "2020-05-05T00:00:00Z", ModelID: effectiveTestModel, Price: 10, Tagline: "repriced"},
	}
	snap := session.SessionSnapshot{Freebucks: due}
	if capped, _ := freebucksCappedForSnapshot(snap, effectiveTestModel); !capped {
		t.Error("not capped with due repricing 2 -> 10 above spendable 5, want capped")
	}
	// The gate is a pure read: stored price, schedule, and notices unchanged.
	if due.Prices[effectiveTestModel] != 2 {
		t.Errorf("stored Prices mutated: got %v, want 2", due.Prices[effectiveTestModel])
	}
	if len(due.PriceChanges) != 1 {
		t.Errorf("stored PriceChanges consumed: len %d, want 1", len(due.PriceChanges))
	}
	if len(due.PriceNotices) != 0 {
		t.Errorf("stored PriceNotices mutated: %v", due.PriceNotices)
	}
	// The effective notice copy resolves to the change tagline.
	if _, notices := EffectiveFreebucksPrices(due, time.Now()); notices[effectiveTestModel] != "repriced" {
		t.Errorf("effective notice = %q, want repriced", notices[effectiveTestModel])
	}

	// Reverse: stored 10, due change to 2, spendable 5 → affordable.
	down := effectiveTestFB(5, 10)
	down.PriceChanges = []upstream.FreebucksPriceChange{
		{At: "2020-05-05T00:00:00Z", ModelID: effectiveTestModel, Price: 2, Tagline: "cheaper"},
	}
	if capped, _ := freebucksCappedForSnapshot(session.SessionSnapshot{Freebucks: down}, effectiveTestModel); capped {
		t.Error("capped with due repricing 10 -> 2 below spendable 5, want not capped")
	}

	// Future changes gate nothing.
	future := effectiveTestFB(5, 2)
	future.PriceChanges = []upstream.FreebucksPriceChange{
		{At: "2030-05-05T00:00:00Z", ModelID: effectiveTestModel, Price: 10, Tagline: "later"},
	}
	if capped, _ := freebucksCappedForSnapshot(session.SessionSnapshot{Freebucks: future}, effectiveTestModel); capped {
		t.Error("capped on a future (not yet due) change, want not capped")
	}
	prices, _ := EffectiveFreebucksPrices(future, time.Now())
	if len(prices) != len(future.Prices) || prices[effectiveTestModel] != 2 {
		t.Errorf("future change altered the quote: %v", prices)
	}

	// Unparsable timestamps stay pending, never applied.
	bad := effectiveTestFB(5, 2)
	bad.PriceChanges = []upstream.FreebucksPriceChange{
		{At: "not-a-time", ModelID: effectiveTestModel, Price: 10, Tagline: "bad"},
	}
	if capped, _ := freebucksCappedForSnapshot(session.SessionSnapshot{Freebucks: bad}, effectiveTestModel); capped {
		t.Error("capped on an unparsable change, want not capped")
	}

	// Due repricing lands discount-adjusted while the first-tab offer is
	// available: raw 10 minus 3 → 7, still above spendable 5.
	disc := effectiveTestFB(5, 2)
	disc.FirstTabDiscount = &upstream.FreebucksFirstTabDiscount{Amount: 3, Available: true}
	disc.PriceChanges = []upstream.FreebucksPriceChange{
		{At: "2020-05-05T00:00:00Z", ModelID: effectiveTestModel, Price: 10, Tagline: "repriced"},
	}
	if capped, _ := freebucksCappedForSnapshot(session.SessionSnapshot{Freebucks: disc}, effectiveTestModel); !capped {
		t.Error("not capped with discount-adjusted 7 above spendable 5, want capped")
	}
	if prices, _ := EffectiveFreebucksPrices(disc, time.Now()); prices[effectiveTestModel] != 7 {
		t.Errorf("discount-adjusted price = %v, want 7", prices[effectiveTestModel])
	}
}

// TestFreebucksCappedAppliesOffPeakWindow pins the recurring policy: the
// daily window flips the gated price as UTC time crosses it, with no parse
// in between. Fixed timestamps pin the window math (including a
// midnight-crossing offer); relative windows pin the gate end to end.
func TestFreebucksCappedAppliesOffPeakWindow(t *testing.T) {
	offer := upstream.FreebuffOffPeakPrice{StartHourUtc: 22, EndHourUtc: 6, Price: 1, RegularPrice: 9}
	at := func(h int) time.Time { return time.Date(2026, 9, 18, h, 0, 0, 0, time.UTC) }
	fb := &upstream.FreebucksInfo{Prices: map[string]float64{"flash": 9}, OffPeak: map[string]upstream.FreebuffOffPeakPrice{"flash": offer}}

	for _, h := range []int{22, 23, 0, 5} {
		prices, notices := EffectiveFreebucksPrices(fb, at(h))
		if prices["flash"] != 1 {
			t.Errorf("hour %d: effective price = %v, want off-peak 1", h, prices["flash"])
		}
		if notices["flash"] != "Off-peak pricing · 9 Freebucks/hour at peak" {
			t.Errorf("hour %d: effective notice = %q, want off-peak copy", h, notices["flash"])
		}
	}
	for _, h := range []int{6, 12, 21} {
		prices, notices := EffectiveFreebucksPrices(fb, at(h))
		if prices["flash"] != 9 {
			t.Errorf("hour %d: effective price = %v, want regular 9", h, prices["flash"])
		}
		if notices["flash"] != "Peak pricing · 1 Freebucks/hour off-peak" {
			t.Errorf("hour %d: effective notice = %q, want peak copy", h, notices["flash"])
		}
	}

	// Models outside the meter never join it via the policy.
	unmetered := &upstream.FreebucksInfo{Prices: map[string]float64{}, OffPeak: map[string]upstream.FreebuffOffPeakPrice{"flash": offer}}
	if prices, _ := EffectiveFreebucksPrices(unmetered, at(23)); len(prices) != 0 {
		t.Errorf("off-peak priced an unmetered model: %v", prices)
	}

	// Gate end to end with windows built around the real clock (the gate
	// reads time.Now itself): stored regular 9, spendable 5.
	h := time.Now().UTC().Hour()
	activeOffer := upstream.FreebuffOffPeakPrice{StartHourUtc: (h + 23) % 24, EndHourUtc: (h + 1) % 24, Price: 1, RegularPrice: 9}
	active := &upstream.FreebucksInfo{
		Balance: 5,
		Daily:   upstream.FreebucksWindow{Limit: 20, Spent: 15, Remaining: 5, ResetAt: time.Now().Add(time.Hour)},
		Prices:  map[string]float64{"deepseek/deepseek-v4-flash": 9},
		OffPeak: map[string]upstream.FreebuffOffPeakPrice{"deepseek/deepseek-v4-flash": activeOffer},
	}
	if capped, _ := freebucksCappedForSnapshot(session.SessionSnapshot{Freebucks: active}, "deepseek/deepseek-v4-flash"); capped {
		t.Error("capped inside the off-peak window (effective 1 <= spendable 5), want not capped")
	}

	idleOffer := upstream.FreebuffOffPeakPrice{StartHourUtc: (h + 1) % 24, EndHourUtc: (h + 2) % 24, Price: 1, RegularPrice: 9}
	idle := &upstream.FreebucksInfo{
		Balance: 5,
		Daily:   upstream.FreebucksWindow{Limit: 20, Spent: 15, Remaining: 5, ResetAt: time.Now().Add(time.Hour)},
		Prices:  map[string]float64{"deepseek/deepseek-v4-flash": 9},
		OffPeak: map[string]upstream.FreebuffOffPeakPrice{"deepseek/deepseek-v4-flash": idleOffer},
	}
	if capped, _ := freebucksCappedForSnapshot(session.SessionSnapshot{Freebucks: idle}, "deepseek/deepseek-v4-flash"); !capped {
		t.Error("not capped outside the off-peak window (regular 9 > spendable 5), want capped")
	}
}
