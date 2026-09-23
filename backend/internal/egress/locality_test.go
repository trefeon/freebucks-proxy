package egress

import (
	"strings"
	"testing"
	"time"

	// LoadLocation on every test host (Windows ships no system zoneinfo).
	_ "time/tzdata"
)

// TestCountryTimezoneTableIsWellFormed guards the whole table as a unit:
// every key is exactly two upper-case ASCII letters and every value loads as
// an IANA zone. This is the typo net — a swapped letter in a zone name or a
// three-letter code fails here instead of silently degrading a live session
// to UTC.
func TestCountryTimezoneTableIsWellFormed(t *testing.T) {
	if len(countryZones) < 60 {
		t.Errorf("countryZones has %d entries, want at least 60", len(countryZones))
	}
	for code, zone := range countryZones {
		if len(code) != 2 || code != strings.ToUpper(code) || !isASCIILetter(code[0]) || !isASCIILetter(code[1]) {
			t.Errorf("countryZones key %q is not two upper-case letters", code)
		}
		if _, err := time.LoadLocation(zone); err != nil {
			t.Errorf("countryZones[%s] = %q does not load: %v", code, zone, err)
		}
		if got, ok := CountryTimezone(code); !ok || got != zone {
			t.Errorf("CountryTimezone(%s) = %q, %v, want %q, true", code, got, ok, zone)
		}
	}
}

// TestCountryTimezoneNormalizationAndRejection guards the input grammar: a
// code is trimmed and upper-cased before lookup, and anything that is not
// two letters is not a country, so callers fall back instead of inventing a
// zone from junk.
func TestCountryTimezoneNormalizationAndRejection(t *testing.T) {
	for _, code := range []string{"id", " ID ", "\tid\n"} {
		if zone, ok := CountryTimezone(code); !ok || zone != "Asia/Jakarta" {
			t.Errorf("CountryTimezone(%q) = %q, %v, want Asia/Jakarta, true", code, zone, ok)
		}
	}
	for _, code := range []string{"", " ", "I", "IDN", "1D", "I1", "I-", "日本", "U$"} {
		if zone, ok := CountryTimezone(code); ok {
			t.Errorf("CountryTimezone(%q) = %q, true, want not ok", code, zone)
		}
	}
	// Well-formed but unknown: a valid alpha-2 that is not in the table
	// (ZZ is ISO's reserved "unknown") must not resolve.
	if zone, ok := CountryTimezone("ZZ"); ok {
		t.Errorf("CountryTimezone(ZZ) = %q, true, want not ok", zone)
	}
}

// TestCountryTimezoneMultiZoneRepresentatives pins the deliberate choice for
// the countries that span several zones: the table is documented to use the
// most populous zone, and this fixes which one that is so a later edit
// cannot silently re-point a country at an outlying zone.
func TestCountryTimezoneMultiZoneRepresentatives(t *testing.T) {
	want := map[string]string{
		"ID": "Asia/Jakarta",        // WIB, most populous
		"US": "America/New_York",    // Eastern, largest of six
		"RU": "Europe/Moscow",       // Moscow, largest of eleven
		"BR": "America/Sao_Paulo",   // Sao Paulo, largest
		"CA": "America/Toronto",     // Eastern Canada, largest
		"AU": "Australia/Sydney",    // Sydney, largest
		"MX": "America/Mexico_City", // capital metro, largest
		"KZ": "Asia/Almaty",         // Almaty, largest
		"ES": "Europe/Madrid",       // peninsula, not Canary
		"PT": "Europe/Lisbon",       // mainland, not Azores/Madeira
		"MY": "Asia/Kuala_Lumpur",   // peninsular Malaysia
		"NZ": "Pacific/Auckland",    // North Island, largest
		"ZA": "Africa/Johannesburg", // largest
		"CN": "Asia/Shanghai",       // one official zone
	}
	for code, zone := range want {
		if got, ok := CountryTimezone(code); !ok || got != zone {
			t.Errorf("CountryTimezone(%s) = %q, %v, want %q, true", code, got, ok, zone)
		}
	}
}

// TestBoringZone guards the "host says nothing" predicate that decides
// whether a detected region may out-rank the host zone: empty, Local, and
// the whole UTC/GMT class are boring (case-insensitive); every real zone is
// not.
func TestBoringZone(t *testing.T) {
	boring := []string{
		"", "  ", "Local", "local", "UTC", "utc", "GMT", "gmt",
		"Etc/UTC", "Etc/GMT", "Etc/GMT+8", "Etc/GMT-5", "Etc/GMT0",
		"Etc/utc", "GMT+14", "GMT-12", "GMT0",
	}
	for _, zone := range boring {
		if !BoringZone(zone) {
			t.Errorf("BoringZone(%q) = false, want true", zone)
		}
	}
	real := []string{
		"Asia/Jakarta", "Europe/Paris", "America/New_York", "Australia/Sydney",
		"Etc/Unknown", "GMTfoo", "Etc/GMTfoo", "Pacific/Auckland", "Asia/Almaty",
	}
	for _, zone := range real {
		if BoringZone(zone) {
			t.Errorf("BoringZone(%q) = true, want false", zone)
		}
	}
}

// TestSessionTimezonePrecedence pins the resolution order: a valid override
// always wins (even a boring one — it was deliberate), then a real host
// zone, then the detected region, then a boring host zone, then UTC. The
// source label reports which branch fired, and an invalid override is
// treated as unset rather than as an error.
func TestSessionTimezonePrecedence(t *testing.T) {
	cases := []struct {
		name                   string
		override, host, region string
		wantZone, wantSource   string
	}{
		{"override wins over host and region", "Europe/Paris", "Asia/Jakarta", "US", "Europe/Paris", "override"},
		{"boring override still wins", "UTC", "Asia/Jakarta", "US", "UTC", "override"},
		{"override is trimmed", "  Europe/Paris  ", "", "", "Europe/Paris", "override"},
		{"invalid override falls through to host", "Not/AZone", "Asia/Jakarta", "US", "Asia/Jakarta", "host"},
		{"blank override is unset", "   ", "Asia/Jakarta", "US", "Asia/Jakarta", "host"},
		{"real host beats region", "Not/AZone", "Europe/Berlin", "SG", "Europe/Berlin", "host"},
		{"boring host yields to region", "", "UTC", "JP", "Asia/Tokyo", "region"},
		{"boring Local yields to region", "", "Local", "SG", "Asia/Singapore", "region"},
		{"region code is normalized", "", "", " sg ", "Asia/Singapore", "region"},
		{"invalid host yields to region", "Not/AZone", "Bogus/Zone", "ID", "Asia/Jakarta", "region"},
		{"boring host with unknown region stays host", "", "UTC", "ZZ", "UTC", "host"},
		{"boring Local with unknown region stays host", "", "Local", "ZZ", "Local", "host"},
		{"nothing usable falls back to UTC", "", "", "", "UTC", "utc"},
		{"invalid host and no region fall back to UTC", "", "Bogus/Zone", "ZZ", "UTC", "utc"},
		{"malformed region is ignored", "", "", "XYZ", "UTC", "utc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			zone, source := SessionTimezone(tc.override, tc.host, tc.region)
			if zone != tc.wantZone || source != tc.wantSource {
				t.Errorf("SessionTimezone(%q, %q, %q) = %q, %q, want %q, %q",
					tc.override, tc.host, tc.region, zone, source, tc.wantZone, tc.wantSource)
			}
		})
	}
}

// TestValidZone guards the loadability predicate that every other decision
// keys off: real zones and Local load, junk and the empty string do not.
func TestValidZone(t *testing.T) {
	for _, zone := range []string{"UTC", "Local", "Asia/Jakarta", "Europe/Kyiv", "America/Argentina/Buenos_Aires", "Etc/GMT+8"} {
		if !ValidZone(zone) {
			t.Errorf("ValidZone(%q) = false, want true", zone)
		}
	}
	for _, zone := range []string{"", "  ", "Not/AZone", "Asia/Nowhere", "junk"} {
		if ValidZone(zone) {
			t.Errorf("ValidZone(%q) = true, want false", zone)
		}
	}
}
