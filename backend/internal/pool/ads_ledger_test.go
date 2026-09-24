package pool

// Ad-leg ledger tests: accumulation, newest-first legs, limit/ring caps,
// and the no-URL privacy rule.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestAdLedgerAccumulation(t *testing.T) {
	ResetAdLedger()
	RecordAdLeg(AdLegEvent{TS: "2026-09-24T10:00:00Z", Surface: AdSurfaceWaitingRoom, Provider: "gravity", Leg: AdLegAuction, Title: "T1", Brand: "B1", Credits: 5})
	RecordAdLeg(AdLegEvent{TS: "2026-09-24T10:00:01Z", Surface: AdSurfaceWaitingRoom, Provider: "gravity", Leg: AdLegImpression, Title: "T1", Brand: "B1"})
	RecordAdLeg(AdLegEvent{TS: "2026-09-24T10:00:02Z", Surface: AdSurfaceChat, Provider: "zeroclick", Leg: AdLegAuction, Credits: 3})
	RecordAdLeg(AdLegEvent{TS: "2026-09-24T10:00:03Z", Surface: AdSurfaceChat, Provider: "zeroclick", Leg: AdLegStreak})
	RecordAdLeg(AdLegEvent{TS: "2026-09-24T10:00:04Z", Surface: AdSurfaceChat, Provider: "gravity", Leg: AdLegAuction, Error: "ads status 500"})

	s := AdSummarySnapshot()
	if s.Totals.Auction != 3 || s.Totals.Impression != 1 || s.Totals.Streak != 1 {
		t.Fatalf("totals = %+v, want {3 1 1}", s.Totals)
	}
	if s.CreditsGranted != 8 {
		t.Fatalf("creditsGranted = %v, want 8", s.CreditsGranted)
	}
	if s.ByProvider["gravity"] != 3 || s.ByProvider["zeroclick"] != 2 {
		t.Fatalf("byProvider = %v, want gravity:3 zeroclick:2", s.ByProvider)
	}
	if s.BySurface[AdSurfaceWaitingRoom] != 2 || s.BySurface[AdSurfaceChat] != 3 {
		t.Fatalf("bySurface = %v, want waiting_room:2 cli_chat:3", s.BySurface)
	}
	if s.Errors != 1 {
		t.Fatalf("errors = %d, want 1", s.Errors)
	}
	if s.LastEventAt != "2026-09-24T10:00:04Z" {
		t.Fatalf("lastEventAt = %q, want last ts", s.LastEventAt)
	}
}

func TestAdLegsNewestFirstAndLimit(t *testing.T) {
	ResetAdLedger()
	for range 5 {
		RecordAdLeg(AdLegEvent{Surface: AdSurfaceWaitingRoom, Provider: "gravity", Leg: AdLegAuction})
	}
	got := AdLegs(3)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].TS > got[i-1].TS {
			t.Fatalf("legs not newest-first: %q after %q", got[i].TS, got[i-1].TS)
		}
	}
	all := AdLegs(0)
	if len(all) != 5 {
		t.Fatalf("limit 0 len = %d, want whole window 5", len(all))
	}
	huge := AdLegs(10000)
	if len(huge) != 5 {
		t.Fatalf("over-cap limit len = %d, want 5", len(huge))
	}
}

func TestAdLedgerRingEviction(t *testing.T) {
	ResetAdLedger()
	for range maxAdLegEvents + 10 {
		RecordAdLeg(AdLegEvent{Surface: AdSurfaceWaitingRoom, Provider: "gravity", Leg: AdLegAuction, Credits: 1})
	}
	legs := AdLegs(0)
	if len(legs) != maxAdLegEvents {
		t.Fatalf("retained = %d, want cap %d", len(legs), maxAdLegEvents)
	}
	s := AdSummarySnapshot()
	if s.Totals.Auction != maxAdLegEvents {
		t.Fatalf("totals.auction = %d, want %d (evicted events leave the window)", s.Totals.Auction, maxAdLegEvents)
	}
	if s.CreditsGranted != float64(maxAdLegEvents) {
		t.Fatalf("creditsGranted = %v, want %d", s.CreditsGranted, maxAdLegEvents)
	}
}

func TestAdLedgerStampsTS(t *testing.T) {
	ResetAdLedger()
	before := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	RecordAdLeg(AdLegEvent{Surface: AdSurfaceWaitingRoom, Provider: "gravity", Leg: AdLegStreak})
	legs := AdLegs(1)
	if len(legs) != 1 || legs[0].TS < before {
		t.Fatalf("unstamped event ts = %q, want auto-stamped now", legs[0].TS)
	}
	if _, err := time.Parse(time.RFC3339, legs[0].TS); err != nil {
		t.Fatalf("ts %q not RFC3339: %v", legs[0].TS, err)
	}
}

func TestAdLedgerNoURLs(t *testing.T) {
	ResetAdLedger()
	RecordAdLeg(AdLegEvent{Surface: AdSurfaceWaitingRoom, Provider: "gravity", Leg: AdLegAuction, Title: "T", Brand: "B"})
	RecordAdLeg(AdLegEvent{Surface: AdSurfaceChat, Provider: "gravity", Leg: AdLegImpression, Error: "boom"})
	rawSum, _ := json.Marshal(AdSummarySnapshot())
	rawLegs, _ := json.Marshal(AdLegs(0))
	for _, raw := range [][]byte{rawSum, rawLegs} {
		lower := strings.ToLower(string(raw))
		for _, banned := range []string{"impurl", "clickurl", "http://", "https://"} {
			if strings.Contains(lower, banned) {
				t.Fatalf("ledger payload leaks %q: %s", banned, raw)
			}
		}
	}
	var keys []string
	var probe []map[string]any
	if err := json.Unmarshal(rawLegs, &probe); err != nil {
		t.Fatal(err)
	}
	for k := range probe[0] {
		keys = append(keys, k)
	}
	for _, want := range []string{"ts", "surface", "provider", "leg"} {
		found := false
		for _, k := range keys {
			if k == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("leg keys %v missing %q", keys, want)
		}
	}
}
