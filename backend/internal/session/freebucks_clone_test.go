package session

import (
	"freebuff-proxy/backend/internal/upstream"
	"testing"
)

// TestCloneFreebucksInfoDeepCopiesListPricesOffPeak pins the vendor 3420c99 /
// af898dc shape: ListPrices, OffPeak, and Upgrade ride the persisted snapshot
// and must not share maps/pointers with live state (ApplyFreebucksPriceChanges
// mutates Prices/ListPrices/PriceNotices in place). Nil stays nil, never
// zero-allocated.
func TestCloneFreebucksInfoDeepCopiesListPricesOffPeak(t *testing.T) {
	id := "inst-1"
	fb := &upstream.FreebucksInfo{
		Balance:      10,
		Prices:       map[string]float64{"m": 5},
		ListPrices:   map[string]float64{"m": 15},
		PriceNotices: map[string]string{"m": "n"},
		PriceChanges: []upstream.FreebucksPriceChange{
			{At: "2030-01-01T00:00:00Z", ModelID: "m", Price: 7, Tagline: "t"},
		},
		Upgrade: &upstream.FreebucksUpgrade{Kind: "limited_offer", CTA: "cta", Tooltip: "tip", ModelID: "m"},
		FirstTabDiscount: &upstream.FreebucksFirstTabDiscount{
			Amount: 3, Available: true,
			Holder: &upstream.FreebucksFirstTabDiscountHolder{InstanceID: &id, Surface: "single", ExpiresAt: "x"},
		},
		OffPeak: map[string]upstream.FreebuffOffPeakPrice{
			"m": {StartHourUtc: 22, EndHourUtc: 6, Price: 2, RegularPrice: 5},
		},
	}
	c := cloneFreebucksInfo(fb)
	if c == fb {
		t.Fatal("clone shares the struct pointer")
	}

	// Mutating the clone leaves the original untouched.
	c.ListPrices["m"] = 99
	c.OffPeak["m"] = upstream.FreebuffOffPeakPrice{}
	c.Upgrade.Kind = "upgrade"
	c.Prices["m"] = 99
	c.PriceNotices["m"] = "mut"
	c.PriceChanges[0].Price = 99
	if fb.ListPrices["m"] != 15 {
		t.Errorf("ListPrices shares map: got %v, want 15", fb.ListPrices["m"])
	}
	if got := fb.OffPeak["m"]; got.Price != 2 || got.RegularPrice != 5 {
		t.Errorf("OffPeak shares map: got %+v, want price 2 regular 5", got)
	}
	if fb.Upgrade.Kind != "limited_offer" {
		t.Errorf("Upgrade shares pointer: got %q, want limited_offer", fb.Upgrade.Kind)
	}
	if fb.Prices["m"] != 5 || fb.PriceNotices["m"] != "n" || fb.PriceChanges[0].Price != 7 {
		t.Errorf("Prices/Notices/Changes share state: %+v", fb)
	}

	// Mutating the original leaves the clone untouched.
	fb.ListPrices["m"] = 100
	fb.OffPeak["other"] = upstream.FreebuffOffPeakPrice{Price: 1}
	fb.Upgrade.CTA = "mut"
	if c.ListPrices["m"] != 99 {
		t.Errorf("clone ListPrices reflects later live write: got %v, want 99", c.ListPrices["m"])
	}
	if _, ok := c.OffPeak["other"]; ok {
		t.Error("clone OffPeak reflects later live write, want detached")
	}
	if c.Upgrade.CTA != "cta" {
		t.Errorf("clone Upgrade reflects later live write: got %q, want cta", c.Upgrade.CTA)
	}
}

func TestCloneFreebucksInfoNilStaysNil(t *testing.T) {
	if got := cloneFreebucksInfo(nil); got != nil {
		t.Errorf("clone(nil) = %+v, want nil", got)
	}
	bare := &upstream.FreebucksInfo{}
	c := cloneFreebucksInfo(bare)
	if c == nil {
		t.Fatal("clone of empty info is nil, want non-nil")
	}
	if c.ListPrices != nil || c.OffPeak != nil || c.Upgrade != nil ||
		c.Prices != nil || c.PriceNotices != nil || c.PriceChanges != nil ||
		c.FirstTabDiscount != nil {
		t.Errorf("clone of empty info allocated: %+v", c)
	}
}
