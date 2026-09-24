package config

import "strings"

// consistency.go — US-consistency preset (US_CONSISTENCY): pins every
// client-controlled signal to US values.
//
// What it does: the session declaration (x-fb-timezone) defaults to
// USConsistencyZone unless SESSION_TIMEZONE is explicitly set, and the ads
// device block follows the declared zone with USConsistencyLocale.
//
// What it does NOT do: change the server-resolved country. The upstream
// resolves country from the egress IP alone (vendor
// common/src/constants/freebuff-models.ts: a client-chosen country is what
// an abusive client rotates, so no client-sent signal is honored). A non-US
// or anonymized egress still resolves non-US no matter what this preset
// declares — fix the egress exit (clean, non-anonymized US IP on every
// session admission, poll, and chat call) to move the verdict. A mismatched
// declaration behind a foreign egress only adds suspicion, which is why the
// preset aligns the signals instead of letting them drift.
const (
	// USConsistencyZone is the US zone the preset declares when the
	// operator did not set an explicit SESSION_TIMEZONE.
	USConsistencyZone = "America/New_York"
	// USConsistencyLocale is the ads device locale the preset declares.
	USConsistencyLocale = "en-US"
)

// EffectiveSessionTimezone returns the SESSION_TIMEZONE override the
// session-locality rule must apply: the explicit value when set, else the
// preset US zone when US_CONSISTENCY is on, else "" (auto — host, region,
// UTC). Nil-safe: a nil config resolves to auto.
func (c *Config) EffectiveSessionTimezone() string {
	if c == nil {
		return ""
	}
	if z := strings.TrimSpace(c.SessionTimezone); z != "" {
		return z
	}
	if c.USConsistency {
		return USConsistencyZone
	}
	return ""
}

// USPresetActive reports whether the preset (rather than an explicit
// SESSION_TIMEZONE) supplies the session zone: US_CONSISTENCY set with no
// explicit SESSION_TIMEZONE. Callers relabel the locality source from
// "override" to "us-consistency" when this holds so /healthz and the doctor
// show where the zone actually came from. Nil-safe.
func (c *Config) USPresetActive() bool {
	return c != nil && c.USConsistency && strings.TrimSpace(c.SessionTimezone) == ""
}
