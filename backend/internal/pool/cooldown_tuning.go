// cooldown_tuning.go — live-applied cooldown tuning: pushes the operator's
// COOLDOWN_*/SESSION_*/SMART_PROBE_*/MATURITY_* knob values (read through the
// config accessors, nil-receiver safe) down to the packages that enforce
// them. runs and upstream keep their own package-level durations (importing
// config there would widen the dependency fan-out of the hot path), so the
// pool — which already owns *config.Config — is the single push point: New
// applies the boot values, SetConfig re-pushes on every reload. All
// defaults preserve the current hardcoded behavior.
package pool

import (
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/runs"
	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/upstream"
)

// applyCooldownTuning pushes the live cooldown configuration into every
// package that enforces a cooldown or backoff window. A nil cfg keeps the
// compiled-in defaults (the accessors are nil-receiver safe, but the push
// itself is skipped so tests with hand-built pools observe no drift).
func (p *Pool) applyCooldownTuning(cfg *config.Config) {
	if cfg == nil {
		return
	}
	runs.SetCooldownTuning(
		cfg.DefaultMs(),
		cfg.CountryBlockMs(),
		cfg.CeilingMs(),
		cfg.IpMaxReadmits(),
		cfg.IpJitterRatio(),
	)
	upstream.SetCooldownTuning(
		cfg.FanoutMs(),
		cfg.InvalidModelMs(),
		cfg.OpaqueMs(),
		cfg.LoadShedMs(),
		cfg.PeakHoursMs(),
		cfg.CeilingMs(),
	)
	if d := cfg.SessionPollMaxMs(); d > 0 {
		sessionPollBackoffMax = d
	}
	if d := cfg.SmartProbeBackoffMaxMs(); d > 0 {
		quotaProbeMaxInterval = d
	}
	if d := cfg.MaturityBackoffMs(); d > 0 {
		maturity429Backoff = d
	}
}

// shouldPark reports whether a computed cooldown of duration d must PARK
// (keep the session row, govern by CooldownUntil skip, backoff-retry) rather
// than drop: SESSION_PARK_ENABLED gates the behavior and
// SESSION_PARK_THRESHOLD_MS bounds it — a cooldown at or under the threshold
// parks, a longer one follows the terminal path. A nil cfg never parks, so
// hand-built pools keep the historical behavior.
func shouldPark(cfg *config.Config, d time.Duration) bool {
	if cfg == nil || d <= 0 {
		return false
	}
	if !cfg.SessionParkEnabled() {
		return false
	}
	return d <= cfg.SessionParkThresholdMs()
}

// pushParkConfig forwards the session-park gate to one session manager,
// next to the other per-manager knobs in New/SetConfig/bridgeEntryFor.
// The SessionPreserve lane (feat/session-park-preserve) owns
// (*Manager).SetParkConfig — this branch calls it via an interface guard
// so this lane compiles standalone; at integrate the guard is satisfied
// and the push goes through, no edit needed here.
func pushParkConfig(sess *session.Manager, cfg *config.Config) {
	if sess == nil || cfg == nil {
		return
	}
	type parker interface {
		SetParkConfig(enabled bool, threshold time.Duration)
	}
	if p, ok := any(sess).(parker); ok {
		p.SetParkConfig(cfg.SessionParkEnabled(), cfg.SessionParkThresholdMs())
	}
}
