package runs

// Token cooldown management: the remembered upstream errors (rate limit,
// ban, country block) so Acquires keep surfacing the exact 429/403 +
// Retry-After instead of re-hitting upstream during the window. Only
// terminal bans are written by the pool's classifier now; 429s pass their
// upstream RetryAfter through to the caller with no cooldown write.

import (
	"freebuff-proxy/backend/internal/upstream"
	"time"
)

// countryBlockCooldown is the token cooldown applied when upstream reports a
// region block (country_blocked): long enough to stop the request hammer
// from re-hitting the blocked admission, short enough to re-probe after the
// client switches egress/VPN. Tunable via COOLDOWN_COUNTRY_BLOCK_MS; the
// default preserves the 15m behavior. Time-bound only — the pool never
// quarantines a country block, so expiry always revives the token.
var countryBlockCooldown = 15 * time.Minute

// SetCooldownTuning keeps the operator-config push point (pool.SetConfig
// pushes the live values on boot and every reload) for the surviving
// country-block window. The retired knobs (default, ceiling, ip readmits,
// ip jitter) are ignored — their windows died with the bounded cooldowns
// (the pool/cooldown_tuning.go caller is removed with them).
func SetCooldownTuning(defaultD, countryBlock, ceiling time.Duration, ipMaxReadmits int, ipJitterRatio float64) {
	if countryBlock > 0 {
		countryBlockCooldown = countryBlock
	}
}

// TuningSnapshot captures the live cooldown tuning values. Tests that push
// nonzero values through pool.New/SetConfig snapshot first and Restore on
// cleanup: the tuning vars are package globals and would otherwise leak
// across tests in the same binary. Production code never calls these.
type TuningSnapshot struct {
	Default, CountryBlock, Ceiling time.Duration
	IPMaxReadmits                  int
	IPJitterRatio                  float64
}

// SnapshotTuning captures the current cooldown tuning values. Removed
// windows read back zero; Restore only re-applies the surviving
// country-block window.
func SnapshotTuning() TuningSnapshot {
	return TuningSnapshot{CountryBlock: countryBlockCooldown}
}

// Restore re-applies a captured snapshot's surviving window.
func (s TuningSnapshot) Restore() {
	countryBlockCooldown = s.CountryBlock
}

// Cooldown puts the token in a cooldown window of duration d. Durations
// <= 0 are ignored.
func (m *RunManager) Cooldown(d time.Duration) {
	if d <= 0 {
		return
	}
	m.mu.Lock()
	m.cooldownUntil = time.Now().Add(d)
	m.rateLimit = nil
	m.ban = nil
	m.banPermanent = false
	m.countryBlock = nil
	// The ban/country windows die with their remembered errors: leaving the
	// deadlines set would surface a stale future BannedUntil (healthz risk
	// gating via Snapshot) with no ban attached. Mirror ClearCooldowns.
	m.banUntil = time.Time{}
	m.countryUntil = time.Time{}
	m.mu.Unlock()
}

// ClearCooldowns removes any cooldown, rate-limit lock, and ban window so
// the token is immediately acquirable again (dashboard unlock action).
// Per-model refusal memory dies here too: success/unlock proves health,
// mirroring the blanket clearing.
func (m *RunManager) ClearCooldowns() {
	m.mu.Lock()
	m.cooldownUntil = time.Time{}
	m.rateLimit = nil
	m.ban = nil
	m.banPermanent = false
	m.banUntil = time.Time{}
	m.countryBlock = nil
	m.countryUntil = time.Time{}
	m.modelLimits = nil
	m.mu.Unlock()
}

// CooldownUntil returns the cooldown deadline (zero when not cooling down).
func (m *RunManager) CooldownUntil() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cooldownUntil
}

// MaintenanceEligible reports whether this manager should receive background
// maintenance work: the cooldown window has passed AND no live ban is
// active. It is the single gate shared by runs.Maintain and every pool
// maintain/poll caller (issue #266), replacing the previously copy-pasted
// time.Now().Before(CooldownUntil()) || BanError() != nil predicate — the
// pool pre-gates and runs.Maintain's own internal check had divergent
// semantics (runs.Maintain carried no ban check, so a hard-banned token with
// a zero cooldown deadline passed it and was only saved by the pool gate).
func (m *RunManager) MaintenanceEligible() bool {
	if time.Now().Before(m.CooldownUntil()) {
		return false
	}
	return m.BanError() == nil
}

// CooldownRateLimit applies a rate-limit cooldown and remembers the error
// so subsequent Acquires surface 429 + Retry-After instead of a generic
// 502. Errors with RetryAfter <= 0 are ignored.
func (m *RunManager) CooldownRateLimit(rle *upstream.RateLimitError) {
	if rle == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if rle.RetryAfter > 0 {
		m.cooldownUntil = time.Now().Add(rle.RetryAfter)
	} else if !rle.ResetAt.IsZero() && rle.ResetAt.After(time.Now()) {
		m.cooldownUntil = rle.ResetAt
	} else {
		m.cooldownUntil = upstream.NextPacificMidnight()
	}
	m.rateLimit = rle
	m.ban = nil
	m.banUntil = time.Time{}
	m.banPermanent = false
	m.countryBlock = nil
	m.countryUntil = time.Time{}
}

// RateLimitError returns the remembered rate-limit error while its
// cooldown is still active, nil otherwise.
func (m *RunManager) RateLimitError() *upstream.RateLimitError {
	m.mu.Lock()
	defer m.mu.Unlock()
	if time.Now().Before(m.cooldownUntil) && m.rateLimit != nil {
		return m.rateLimit
	}
	return nil
}

// modelLimitEntry is one model's remembered admission/run-start refusal:
// the refusal plus the instant the lane may be re-attempted.
type modelLimitEntry struct {
	err   *upstream.RateLimitError
	until time.Time
}

// RememberModelRateLimit remembers one model's admission/run-start rate-limit
// refusal so the next same-model walk can skip the dead lane without
// upstream contact. Admission 429s carry quota truth that dies with the
// walk unless remembered here. The struct value is copied onto a fresh
// heap object — walk errors may be single-flight-shared, so the caller's
// pointer is never stored. Opaque refusals (no RetryAfter/ResetAt) or
// already-past windows are not parked: they retry live next time.
// Overwrites any previous memory for the model.
func (m *RunManager) RememberModelRateLimit(model string, rle *upstream.RateLimitError) {
	if model == "" || rle == nil {
		return
	}
	now := time.Now()
	var until time.Time
	if rle.RetryAfter > 0 {
		until = now.Add(rle.RetryAfter)
	} else {
		until = rle.ResetAt
	}
	if until.IsZero() || !until.After(now) {
		return
	}
	cp := *rle
	m.mu.Lock()
	if m.modelLimits == nil {
		m.modelLimits = make(map[string]*modelLimitEntry)
	}
	m.modelLimits[model] = &modelLimitEntry{err: &cp, until: until}
	m.mu.Unlock()
}

// ModelRateLimit returns the remembered rate-limit refusal for model while
// its window is still live, nil when absent or expired (lazy expiry on
// read, same discipline as RateLimitError). Admission 429s carry quota
// truth that dies with the walk unless remembered — this is that memory.
// The returned pointer is the stored copy: callers must copy before
// tagging (cf. tagRateLimitModel) and never mutate it.
func (m *RunManager) ModelRateLimit(model string) *upstream.RateLimitError {
	if model == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.modelLimits[model]
	if e == nil {
		return nil
	}
	if !time.Now().Before(e.until) {
		delete(m.modelLimits, model)
		return nil
	}
	return e.err
}

// CooldownBan applies a ban cooldown and remembers the error so Acquires
// keep surfacing 403 banned + resumes-at until the unban time.
func (m *RunManager) CooldownBan(be *upstream.BanError) {
	if be == nil {
		return
	}
	m.mu.Lock()
	m.ban = be
	if be.ResumesAt.IsZero() {
		// Hard ban (no resumes_at): the account is dead upstream — trust
		// caps (past_enforcement) make it permanent, and a timed retry only
		// generates repeated 403 contacts against a banned account. Keep
		// the remembered ban live indefinitely (banUntil zero = permanent:
		// BanError() returns it, banView renders hard/zero, Acquire skips
		// via the BanError guard) until the operator clears it (dashboard
		// unlock / AUTH_TOKENS change).
		m.banUntil = time.Time{}
		m.banPermanent = true
	} else if !be.ResumesAt.After(time.Now()) {
		// resumes_at present but past: an expired temporary ban — already
		// lifted upstream, so keep no ban memory at all. Retiring it would
		// wrongly kill a merely-expired temporary ban; a stale window would
		// only delay the next (correct) admission.
		m.ban = nil
		m.banUntil = time.Time{}
		m.banPermanent = false
	} else {
		m.banUntil = be.ResumesAt
		m.banPermanent = false
	}
	// The ban also fills the shared cooldown deadline so Acquire skips the
	// token entirely during the window (the remembered error is surfaced by
	// the cooldown-skip branch instead of re-hitting upstream).
	m.cooldownUntil = m.banUntil
	m.rateLimit = nil // a ban supersedes any rate-limit cooldown
	m.countryBlock = nil
	m.mu.Unlock()
}

// BanError returns the remembered ban error while the ban window is
// active, nil otherwise. A permanent (hard) ban is always live.
func (m *RunManager) BanError() *upstream.BanError {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ban != nil && (m.banPermanent || time.Now().Before(m.banUntil)) {
		return m.ban
	}
	return nil
}

// CooldownCountryBlocked applies a country-block cooldown and remembers the
// error so Acquires keep surfacing the region-block instead of re-hitting
// upstream during the window (mirrors CooldownRateLimit/CooldownBan).
func (m *RunManager) CooldownCountryBlocked(cbe *upstream.CountryBlockedError) {
	if cbe == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// A ban outranks a country block (pool precedence ban > country): keep
	// the ban window and its remembered error instead of downgrading to the
	// shorter country cooldown.
	if m.ban != nil && (m.banPermanent || time.Now().Before(m.banUntil)) {
		return
	}
	m.countryBlock = cbe
	m.countryUntil = time.Now().Add(countryBlockCooldown)
	// The block also fills the shared cooldown deadline so Acquire skips
	// the token entirely during the window (the remembered error is
	// surfaced by the cooldown-skip branch instead of re-hitting upstream).
	m.cooldownUntil = m.countryUntil
	m.rateLimit = nil
	m.ban = nil
	m.banPermanent = false
	m.banUntil = time.Time{}
}

// CountryBlockedError returns the remembered country-block error while its
// cooldown window is active, nil otherwise.
func (m *RunManager) CountryBlockedError() *upstream.CountryBlockedError {
	m.mu.Lock()
	defer m.mu.Unlock()
	if time.Now().Before(m.countryUntil) && m.countryBlock != nil {
		return m.countryBlock
	}
	return nil
}
