package config

// Cooldown and session-park knobs: every upstream-refusal backoff the pool
// and classifier apply is operator-tunable in milliseconds, plus the
// park-vs-drop switch that keeps short-cooldown sessions alive instead of
// invalidating them. All keys are live-apply (read per refusal/poll, never
// snapshotted at boot) and zero-tolerant: an unset, empty, or non-positive
// value falls back to the Contract default, so a blank row can never
// zero-out a backoff into a hot retry loop.
//
// Contract defaults (equal to the previous hardcoded behavior):
//
//	COOLDOWN_DEFAULT_MS=1800000        (30m auth-rejection cooldown)
//	COOLDOWN_COUNTRY_BLOCK_MS=900000   (15m region-block cooldown)
//	COOLDOWN_CEILING_MS=604800000      (7d ceiling for upstream retry fields)
//	COOLDOWN_FANOUT_MS=60000           (1m fanout-refusal backoff)
//	COOLDOWN_INVALID_MODEL_MS=60000    (1m invalid-agent-model backoff)
//	COOLDOWN_OPAQUE_MS=60000           (1m opaque-429 backoff)
//	COOLDOWN_LOADSHED_MS=90000         (90s load-shedding backoff)
//	COOLDOWN_PEAK_HOURS_MS=1800000     (30m peak-hours backoff)
//	COOLDOWN_IP_MAX_READMITS=3         (ip_capped re-admits per Pacific day)
//	COOLDOWN_IP_JITTER_RATIO=0.2       (+/-fraction re-admit jitter)
//	SESSION_PARK_ENABLED=true          (park short-cooldown sessions)
//	SESSION_PARK_THRESHOLD_MS=900000   (at-or-below parks, never drops)
//	SESSION_POLL_MAX_MS=300000         (5m session-poll backoff cap)
//	SMART_PROBE_BACKOFF_MAX_MS=1800000 (30m quota-probe backoff ceiling)
//	MATURITY_BACKOFF_MS=180000         (3m maturity 429 walk pause)

import (
	"math"
	"time"
)

// Millisecond defaults for the cooldown / park knobs (see above).
const (
	defaultCooldownDefaultMs      = 1800000
	defaultCooldownCountryBlockMs = 900000
	defaultCooldownCeilingMs      = 604800000
	defaultCooldownFanoutMs       = 60000
	defaultCooldownInvalidModelMs = 60000
	defaultCooldownOpaqueMs       = 60000
	defaultCooldownLoadShedMs     = 90000
	defaultCooldownPeakHoursMs    = 1800000
	defaultCooldownIPMaxReadmits  = 3
	defaultSessionParkThresholdMs = 900000
	defaultSessionPollMaxMs       = 300000
	defaultSmartProbeBackoffMaxMs = 1800000
	defaultMaturityBackoffMs      = 180000
)

// defaultCooldownIPJitterRatio is the default +/-fraction of retryAfterMs
// applied to the ip_capped re-admission window.
const defaultCooldownIPJitterRatio = 0.2

// msToDuration converts an integer-millisecond knob to a Duration:
// non-positive values fall back to the Contract default (a blank row can
// never zero-out a backoff), and absurd values saturate at the int64
// duration range instead of wrapping negative (consumers clamp to the
// ceiling at use, mirroring upstream.MaxCooldown).
func msToDuration(ms int, fallbackMs int) time.Duration {
	if ms <= 0 {
		ms = fallbackMs
	}
	if int64(ms) > int64(math.MaxInt64)/int64(time.Millisecond) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(ms) * time.Millisecond
}

// Cooldown accessors: nil-receiver safe (nil Config reports Contract
// defaults), so pool/upstream callers can code against cfg.DefaultMs()
// etc. with a nil guard and no behavior change when unset.

// DefaultMs is the token cooldown on upstream auth rejection
// (COOLDOWN_DEFAULT_MS; default 30m).
func (c *Config) DefaultMs() time.Duration {
	if c == nil || c.CooldownDefault <= 0 {
		return msToDuration(0, defaultCooldownDefaultMs)
	}
	return c.CooldownDefault
}

// CountryBlockMs is the token cooldown on upstream region block
// (COOLDOWN_COUNTRY_BLOCK_MS; default 15m).
func (c *Config) CountryBlockMs() time.Duration {
	if c == nil || c.CooldownCountryBlock <= 0 {
		return msToDuration(0, defaultCooldownCountryBlockMs)
	}
	return c.CooldownCountryBlock
}

// CeilingMs is the farthest future any upstream-derived cooldown deadline
// may extend (COOLDOWN_CEILING_MS; default 7d).
func (c *Config) CeilingMs() time.Duration {
	if c == nil || c.CooldownCeiling <= 0 {
		return msToDuration(0, defaultCooldownCeilingMs)
	}
	return c.CooldownCeiling
}

// FanoutMs bounds a free_mode_run_fanout refusal
// (COOLDOWN_FANOUT_MS; default 1m).
func (c *Config) FanoutMs() time.Duration {
	if c == nil || c.CooldownFanout <= 0 {
		return msToDuration(0, defaultCooldownFanoutMs)
	}
	return c.CooldownFanout
}

// InvalidModelMs bounds a free_mode_invalid_agent_model refusal
// (COOLDOWN_INVALID_MODEL_MS; default 1m).
func (c *Config) InvalidModelMs() time.Duration {
	if c == nil || c.CooldownInvalidModel <= 0 {
		return msToDuration(0, defaultCooldownInvalidModelMs)
	}
	return c.CooldownInvalidModel
}

// OpaqueMs bounds a 429 with no timestamp or Retry-After signal
// (COOLDOWN_OPAQUE_MS; default 1m).
func (c *Config) OpaqueMs() time.Duration {
	if c == nil || c.CooldownOpaque <= 0 {
		return msToDuration(0, defaultCooldownOpaqueMs)
	}
	return c.CooldownOpaque
}

// LoadShedMs bounds a 429 load-saturation refusal
// (COOLDOWN_LOADSHED_MS; default 90s).
func (c *Config) LoadShedMs() time.Duration {
	if c == nil || c.CooldownLoadShed <= 0 {
		return msToDuration(0, defaultCooldownLoadShedMs)
	}
	return c.CooldownLoadShed
}

// PeakHoursMs bounds a 429 peak-hours refusal
// (COOLDOWN_PEAK_HOURS_MS; default 30m).
func (c *Config) PeakHoursMs() time.Duration {
	if c == nil || c.CooldownPeakHours <= 0 {
		return msToDuration(0, defaultCooldownPeakHoursMs)
	}
	return c.CooldownPeakHours
}

// IpMaxReadmits caps how many times one token may re-admit (and be refused
// ip_capped again) per Pacific day before it locks until the next Pacific
// midnight (COOLDOWN_IP_MAX_READMITS; default 3). Zero-tolerant like the ms
// knobs above: an explicit 0 falls back to the default (runs would still
// enforce the old global, so 0 can never mean "no re-admits").
func (c *Config) IpMaxReadmits() int {
	if c == nil || c.CooldownIPMaxReadmits <= 0 {
		return defaultCooldownIPMaxReadmits
	}
	return c.CooldownIPMaxReadmits
}

// IpJitterRatio is the +/-fraction of retryAfterMs applied to the ip_capped
// re-admission window (COOLDOWN_IP_JITTER_RATIO; default 0.2).
func (c *Config) IpJitterRatio() float64 {
	if c == nil || c.CooldownIPJitterRatio < 0 || math.IsNaN(c.CooldownIPJitterRatio) {
		return defaultCooldownIPJitterRatio
	}
	return c.CooldownIPJitterRatio
}

// SessionParkEnabled reports whether short-cooldown sessions park (kept
// row, backoff retry) instead of dropping (SESSION_PARK_ENABLED;
// default true).
func (c *Config) SessionParkEnabled() bool {
	if c == nil {
		return true
	}
	return c.SessionParkEnabledFlag
}

// SessionParkThresholdMs is the park-vs-drop boundary: a session whose
// computed cooldown is at or below this never drops
// (SESSION_PARK_THRESHOLD_MS; default 15m).
func (c *Config) SessionParkThresholdMs() time.Duration {
	if c == nil || c.SessionParkThreshold <= 0 {
		return msToDuration(0, defaultSessionParkThresholdMs)
	}
	return c.SessionParkThreshold
}

// SessionPollMaxMs caps the session-liveness poll failure backoff
// (SESSION_POLL_MAX_MS; default 5m).
func (c *Config) SessionPollMaxMs() time.Duration {
	if c == nil || c.SessionPollMax <= 0 {
		return msToDuration(0, defaultSessionPollMaxMs)
	}
	return c.SessionPollMax
}

// SmartProbeBackoffMaxMs caps the quota-probe 429-backoff doubling
// (SMART_PROBE_BACKOFF_MAX_MS; default 30m).
func (c *Config) SmartProbeBackoffMaxMs() time.Duration {
	if c == nil || c.SmartProbeBackoffMax <= 0 {
		return msToDuration(0, defaultSmartProbeBackoffMaxMs)
	}
	return c.SmartProbeBackoffMax
}

// MaturityBackoffMs pauses the nightly maturity walk after a rate-limited
// touch (MATURITY_BACKOFF_MS; default 3m).
func (c *Config) MaturityBackoffMs() time.Duration {
	if c == nil || c.MaturityBackoff <= 0 {
		return msToDuration(0, defaultMaturityBackoffMs)
	}
	return c.MaturityBackoff
}
