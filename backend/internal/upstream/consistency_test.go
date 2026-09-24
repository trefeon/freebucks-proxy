package upstream

import (
	"testing"
)

// TestConsistencyAdsOverrideDefaultsOff pins the zero-behavior contract:
// with no override installed the consult helpers return the host-derived
// fallback untouched, so unsetting US_CONSISTENCY changes nothing.
func TestConsistencyAdsOverrideDefaultsOff(t *testing.T) {
	SetConsistencyAdsOverride("", "")
	if got := consistencyAdsZoneOr("Pacific/Auckland"); got != "Pacific/Auckland" {
		t.Errorf("zone fallback = %q, want untouched", got)
	}
	if got := consistencyAdsLocaleOr("de-DE"); got != "de-DE" {
		t.Errorf("locale fallback = %q, want untouched", got)
	}
}

// TestConsistencyAdsOverridePinsUSValues pins the preset contract: an
// installed override wins over any host-derived fallback, values are
// trimmed, and clearing restores the fallback.
func TestConsistencyAdsOverridePinsUSValues(t *testing.T) {
	t.Cleanup(func() { SetConsistencyAdsOverride("", "") })
	SetConsistencyAdsOverride("America/New_York", "en-US")
	if got := consistencyAdsZoneOr("Pacific/Auckland"); got != "America/New_York" {
		t.Errorf("pinned zone = %q, want America/New_York", got)
	}
	if got := consistencyAdsLocaleOr("de-DE"); got != "en-US" {
		t.Errorf("pinned locale = %q, want en-US", got)
	}

	SetConsistencyAdsOverride("  America/New_York  ", "  ")
	if got := consistencyAdsZoneOr("UTC"); got != "America/New_York" {
		t.Errorf("trimmed zone = %q, want America/New_York", got)
	}
	// A blank locale half clears that half only: the zone pin survives.
	if got := consistencyAdsLocaleOr("de-DE"); got != "de-DE" {
		t.Errorf("cleared locale = %q, want fallback de-DE", got)
	}

	SetConsistencyAdsOverride("", "")
	if got := consistencyAdsZoneOr("UTC"); got != "UTC" {
		t.Errorf("cleared zone = %q, want fallback UTC", got)
	}
}
