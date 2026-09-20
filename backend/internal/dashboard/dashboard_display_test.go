package dashboard

import (
	"encoding/json"
	"freebucks-proxy/backend/internal/pool"
	"freebucks-proxy/backend/internal/upstream"
	"strings"
	"testing"
)

// TestFreebucksCardListPricesThrough pins the DISPLAY-wave mapping (vendor
// 3420c99): listPrices rides the card beside the effective prices map, and
// the effective values are preserved untouched.
func TestFreebucksCardListPricesThrough(t *testing.T) {
	card := freebucksCardFromInfo(&upstream.FreebucksInfo{
		Balance:    10,
		Prices:     map[string]float64{"openai/gpt-5.6-luna": 5},
		ListPrices: map[string]float64{"openai/gpt-5.6-luna": 15},
		FirstTabDiscount: &upstream.FreebucksFirstTabDiscount{
			Amount: 10, Available: true,
		},
	})
	if card == nil {
		t.Fatal("card = nil, want mapped block")
	}
	if card.ListPrices["openai/gpt-5.6-luna"] != 15 {
		t.Errorf("ListPrices = %v, want luna 15", card.ListPrices)
	}
	if card.Prices["openai/gpt-5.6-luna"] != 5 {
		t.Errorf("Prices = %v, want luna 5 (effective, not list)", card.Prices)
	}
}

// TestFreebucksCardPreListPricesNil pins the nil-safe rule: a quote from a
// server that predates listPrices/offPeak keeps nil maps (no zero-alloc),
// so the SPA renders no strike rather than a guessed one.
func TestFreebucksCardPreListPricesNil(t *testing.T) {
	card := freebucksCardFromInfo(&upstream.FreebucksInfo{
		Balance: 10,
		Prices:  map[string]float64{"openai/gpt-5.6-luna": 5},
	})
	if card == nil {
		t.Fatal("card = nil, want mapped block")
	}
	if card.ListPrices != nil {
		t.Errorf("ListPrices = %v, want nil (predates listPrices)", card.ListPrices)
	}
	if card.OffPeak != nil {
		t.Errorf("OffPeak = %v, want nil (predates offPeak)", card.OffPeak)
	}
	raw, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"list_prices"`, `"off_peak"`} {
		if strings.Contains(string(raw), key) {
			t.Errorf("card JSON contains %s, want key absent: %s", key, raw)
		}
	}
}

// TestFreebucksCardKeysPresent pins the card keys once the server sends
// both display maps, including the snake_case off-peak entry shape.
func TestFreebucksCardKeysPresent(t *testing.T) {
	card := freebucksCardFromInfo(&upstream.FreebucksInfo{
		Balance:    10,
		Prices:     map[string]float64{"openai/gpt-5.6-luna": 5},
		ListPrices: map[string]float64{"openai/gpt-5.6-luna": 15},
		OffPeak: map[string]upstream.FreebuffOffPeakPrice{
			"openai/gpt-5.6-luna": {
				StartHourUtc: 0, EndHourUtc: 8, Price: 5, RegularPrice: 15,
			},
		},
	})
	raw, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{
		`"list_prices"`, `"off_peak"`,
		`"start_hour_utc"`, `"end_hour_utc"`, `"regular_price"`,
	} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("card JSON missing %s: %s", key, raw)
		}
	}
	var decoded struct {
		OffPeak map[string]freebucksOffPeakCard `json:"off_peak"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	offer := decoded.OffPeak["openai/gpt-5.6-luna"]
	if offer.StartHourUtc != 0 || offer.EndHourUtc != 8 ||
		offer.Price != 5 || offer.RegularPrice != 15 {
		t.Errorf("off_peak[luna] = %+v, want 0-8 price 5 regular 15", offer)
	}
}

// TestListPricesNeverGates pins the display-only contract: the admission
// spendable amount is balance + claimable grants regardless of how large
// the list prices are, and mapping the card never mutates the effective
// prices map the gate admits on.
func TestListPricesNeverGates(t *testing.T) {
	info := &upstream.FreebucksInfo{
		Balance:        10,
		ClaimableGrant: 4,
		Prices:         map[string]float64{"openai/gpt-5.6-luna": 5},
		ListPrices:     map[string]float64{"openai/gpt-5.6-luna": 1500},
		OffPeak: map[string]upstream.FreebuffOffPeakPrice{
			"openai/gpt-5.6-luna": {
				StartHourUtc: 0, EndHourUtc: 8, Price: 5, RegularPrice: 15,
			},
		},
	}
	if got := info.Spendable(); got != 14 {
		t.Errorf("Spendable = %v, want 14 (listPrices must never gate)", got)
	}
	card := freebucksCardFromInfo(info)
	if card.Prices["openai/gpt-5.6-luna"] != 5 {
		t.Errorf("card Prices = %v, want effective 5 untouched", card.Prices)
	}
	if got := info.Spendable(); got != 14 {
		t.Errorf("Spendable after card mapping = %v, want 14", got)
	}
}

// TestTokenCardDailyBonusOmitted pins the streak-payload floor: without a
// freebucks_daily_bonus value the key omits (nil) and the SPA falls back
// to the session copy with no Freebucks bonus note. No poller or admission
// wiring populates it in this wave.
func TestTokenCardDailyBonusOmitted(t *testing.T) {
	card := cardFromSnapshot(pool.TokenSnapshot{Token: 0, Streak: 7})
	if card.FreebucksDailyBonus != nil {
		t.Errorf("FreebucksDailyBonus = %v, want nil (no bonus source)", card.FreebucksDailyBonus)
	}
	raw, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), `"freebucks_daily_bonus"`) {
		t.Errorf("token JSON contains freebucks_daily_bonus, want absent: %s", raw)
	}
}
