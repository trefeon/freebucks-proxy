package dashboard

import (
	"encoding/json"
	"freebucks-proxy/backend/internal/pool"
	"strings"
	"testing"
)

// TestCardsCarryCountryMismatchKeys pins the coordinator contract: both the
// full card and the hot-poll live card carry country_code +
// country_block_reason as plain strings from the snapshot, so a block (or
// its lift) renders without waiting for a full fetch.
func TestCardsCarryCountryMismatchKeys(t *testing.T) {
	snap := pool.TokenSnapshot{
		Token:              0,
		CountryCode:        "XX",
		CountryBlockReason: "recent_limited_country",
	}
	card := cardFromSnapshot(snap)
	if card.CountryCode != "XX" {
		t.Errorf("card.CountryCode = %q, want XX", card.CountryCode)
	}
	if card.CountryBlockReason != "recent_limited_country" {
		t.Errorf("card.CountryBlockReason = %q, want the block reason", card.CountryBlockReason)
	}
	live := liveCardFromSnapshot(snap)
	if live.CountryCode != "XX" {
		t.Errorf("live.CountryCode = %q, want XX", live.CountryCode)
	}
	if live.CountryBlockReason != "recent_limited_country" {
		t.Errorf("live.CountryBlockReason = %q, want the block reason", live.CountryBlockReason)
	}
}

// TestCardsOmitAbsentCountryKeys pins the clean-account shape: no block
// means empty strings, and the keys omit from the JSON payload (absent-safe
// for the SPA merge).
func TestCardsOmitAbsentCountryKeys(t *testing.T) {
	for name, raw := range map[string][]byte{
		"full": mustMarshal(t, cardFromSnapshot(pool.TokenSnapshot{Token: 1})),
		"live": mustMarshal(t, liveCardFromSnapshot(pool.TokenSnapshot{Token: 1})),
	} {
		if strings.Contains(string(raw), "country_code") {
			t.Errorf("%s card emits country_code without a block: %s", name, raw)
		}
		if strings.Contains(string(raw), "country_block_reason") {
			t.Errorf("%s card emits country_block_reason without a block: %s", name, raw)
		}
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}
