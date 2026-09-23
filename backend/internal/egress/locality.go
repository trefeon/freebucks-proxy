package egress

import (
	"strings"
	"time"
)

// countryZones maps an ISO-3166 alpha-2 country code (as reported by the
// cdn-cgi/trace `loc=` line) to one representative IANA zone for that
// country. This is the region half of the session-locality rule: a host
// whose own zone carries no locality (see BoringZone) may declare the zone
// of the region it actually egresses from.
//
// The mapping is deliberately a single zone per country, not a full
// zone-in-country table: the upstream wire carries exactly one timezone
// string and the server uses it to pick the account's daily reset zone, so
// "which zone inside the country" only has to be plausible, not exact.
// Multi-zone countries pick their most populous zone (the one an ordinary
// client there would report) and say so in the comment above the entry:
//
//   - US -> America/New_York (Eastern; six zones, Eastern is the largest)
//   - RU -> Europe/Moscow (Moscow is by far the largest)
//   - BR -> America/Sao_Paulo (Sao Paulo is the largest)
//   - CA -> America/Toronto (Eastern Canada is the largest)
//   - AU -> Australia/Sydney (Sydney is the largest)
//   - MX -> America/Mexico_City (the capital metro is the largest)
//   - ID -> Asia/Jakarta (WIB, the most populous western zone)
//   - KZ -> Asia/Almaty (the largest; the western zones are sparse)
//   - ES -> Europe/Madrid (the peninsula; Canary is Atlantic/Canary)
//   - PT -> Europe/Lisbon (the mainland; Azores/Madeira are two outlying zones)
//   - MY -> Asia/Kuala_Lumpur (peninsular Malaysia; Borneo shares UTC+8)
//   - NZ -> Pacific/Auckland (the North Island is the largest)
//   - ZA -> Africa/Johannesburg (the largest)
//   - CN -> Asia/Shanghai (one official zone nationwide)
//
// An operator who wants a precise zone always has SessionTimezone's
// override, which wins over everything below.
var countryZones = map[string]string{
	// South and Southeast Asia
	"ID": "Asia/Jakarta",
	"SG": "Asia/Singapore",
	"MY": "Asia/Kuala_Lumpur",
	"TH": "Asia/Bangkok",
	"VN": "Asia/Ho_Chi_Minh",
	"PH": "Asia/Manila",
	"IN": "Asia/Kolkata",
	"PK": "Asia/Karachi",
	"BD": "Asia/Dhaka",
	"LK": "Asia/Colombo",

	// East Asia
	"JP": "Asia/Tokyo",
	"KR": "Asia/Seoul",
	"CN": "Asia/Shanghai",
	"TW": "Asia/Taipei",
	"HK": "Asia/Hong_Kong",

	// Oceania
	"AU": "Australia/Sydney",
	"NZ": "Pacific/Auckland",

	// North America
	"US": "America/New_York",
	"CA": "America/Toronto",
	"MX": "America/Mexico_City",

	// South America
	"BR": "America/Sao_Paulo",
	"AR": "America/Argentina/Buenos_Aires",
	"CL": "America/Santiago",
	"CO": "America/Bogota",
	"PE": "America/Lima",

	// Western Europe
	"GB": "Europe/London",
	"IE": "Europe/Dublin",
	"FR": "Europe/Paris",
	"DE": "Europe/Berlin",
	"NL": "Europe/Amsterdam",
	"BE": "Europe/Brussels",
	"ES": "Europe/Madrid",
	"PT": "Europe/Lisbon",
	"IT": "Europe/Rome",
	"CH": "Europe/Zurich",
	"AT": "Europe/Vienna",

	// Central and Eastern Europe
	"PL": "Europe/Warsaw",
	"CZ": "Europe/Prague",
	"SK": "Europe/Bratislava",
	"HU": "Europe/Budapest",
	"RO": "Europe/Bucharest",
	"BG": "Europe/Sofia",
	"GR": "Europe/Athens",
	"TR": "Europe/Istanbul",
	"UA": "Europe/Kyiv",
	"RU": "Europe/Moscow",
	"KZ": "Asia/Almaty",

	// Nordics and Baltics
	"SE": "Europe/Stockholm",
	"NO": "Europe/Oslo",
	"DK": "Europe/Copenhagen",
	"FI": "Europe/Helsinki",
	"EE": "Europe/Tallinn",
	"LV": "Europe/Riga",
	"LT": "Europe/Vilnius",

	// Middle East
	"IL": "Asia/Jerusalem",
	"SA": "Asia/Riyadh",
	"AE": "Asia/Dubai",
	"QA": "Asia/Qatar",
	"KW": "Asia/Kuwait",
	"IR": "Asia/Tehran",
	"IQ": "Asia/Baghdad",

	// Africa
	"EG": "Africa/Cairo",
	"ZA": "Africa/Johannesburg",
	"NG": "Africa/Lagos",
	"KE": "Africa/Nairobi",
	"GH": "Africa/Accra",
	"MA": "Africa/Casablanca",
	"TN": "Africa/Tunis",
	"DZ": "Africa/Algiers",
}

// CountryTimezone returns a representative IANA zone for an ISO-3166
// alpha-2 country code. The code is normalized (trimmed, upper-cased) and
// must be exactly two ASCII letters; anything else — empty, longer, digits,
// punctuation — is not a country and yields ok=false. Unknown (but
// well-formed) codes also yield ok=false, so callers fall back rather than
// invent a zone.
func CountryTimezone(country string) (string, bool) {
	code := strings.ToUpper(strings.TrimSpace(country))
	if len(code) != 2 || !isASCIILetter(code[0]) || !isASCIILetter(code[1]) {
		return "", false
	}
	zone, ok := countryZones[code]
	return zone, ok
}

// isASCIILetter reports whether b is A-Z or a-z.
func isASCIILetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

// BoringZone reports whether a zone carries no locality signal: empty,
// "Local", and the UTC/GMT class (UTC, GMT, Etc/UTC, Etc/GMT, Etc/GMT+N,
// Etc/GMT-N, case-insensitive). A VPS clock left at UTC is not a statement
// about where the operator is, so it must not out-rank the detected egress
// region; any real zone is a deliberate choice and does win. "Local" counts
// as boring for the same reason the zero zone does: it means "whatever the
// host is set to", which on a server is usually UTC and never a decision.
func BoringZone(zone string) bool {
	z := strings.TrimSpace(zone)
	if z == "" {
		return true
	}
	// Etc/... is the only namespace for these names in the tz database;
	// stripping the prefix lets Etc/UTC and Etc/GMT+8 take the same path as
	// UTC and GMT+8.
	z = strings.TrimPrefix(z, "Etc/")
	if strings.EqualFold(z, "UTC") || strings.EqualFold(z, "GMT") || strings.EqualFold(z, "Local") {
		return true
	}
	return isFixedOffsetGMT(z)
}

// isFixedOffsetGMT reports whether z is GMT followed only by an optional
// sign and digits ("GMT", "GMT0", "GMT+8", "GMT-5"), case-insensitive.
func isFixedOffsetGMT(z string) bool {
	rest, ok := strings.CutPrefix(strings.ToUpper(z), "GMT")
	if !ok {
		return false
	}
	rest = strings.TrimPrefix(strings.TrimPrefix(rest, "+"), "-")
	if rest == "" {
		return false
	}
	for _, c := range rest {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// ValidZone reports whether zone is a loadable IANA timezone via
// time.LoadLocation. The empty (or blank) string is explicitly invalid even
// though LoadLocation maps "" to UTC: an unset zone is not a zone the
// gateway can declare, and treating it as UTC would let it win a resolution
// branch it must lose. LoadLocation consults $ZONEINFO, the system zoneinfo
// directory, the Go distribution's zoneinfo.zip, and finally the tzdata
// embedded by any package that blank-imports time/tzdata. The gateway
// binary gets its copy from the cli package (cli_serve.go), which is the
// only entrypoint; tests embed it directly. A host with none of those makes
// ValidZone false rather than wrong — SessionTimezone then keeps the
// upstream declaration on UTC instead of naming a zone it cannot resolve.
func ValidZone(zone string) bool {
	zone = strings.TrimSpace(zone)
	if zone == "" {
		return false
	}
	_, err := time.LoadLocation(zone)
	return err == nil
}

// SessionTimezone picks the IANA zone the gateway declares upstream as
// x-fb-timezone (the account's reset-zone preference), in priority order:
//
//	override (valid)             -> (override, "override")
//	host (valid, not boring)     -> (host, "host")
//	region (mapped by country)   -> (map, "region")
//	host (valid, boring)         -> (host, "host")
//	otherwise                    -> ("UTC", "utc")
//
// The returned source tells callers which branch won, for logging.
//
// A non-empty override that does not load is treated as unset, not as an
// error: the resolver stays total, and callers that own a user-facing
// surface log the invalid value themselves. host is the zone the gateway
// host is configured with ("" when unset); region is the country code from
// the egress probe. A deliberately configured host zone carries locality
// and beats detection; a boring host zone (UTC and friends) does not, which
// is the whole point: the region wins exactly when the host says nothing.
func SessionTimezone(override, host, region string) (string, string) {
	override = strings.TrimSpace(override)
	if override != "" && ValidZone(override) {
		return override, "override"
	}
	host = strings.TrimSpace(host)
	if ValidZone(host) && !BoringZone(host) {
		return host, "host"
	}
	if zone, ok := CountryTimezone(region); ok {
		return zone, "region"
	}
	if ValidZone(host) {
		return host, "host"
	}
	return "UTC", "utc"
}
