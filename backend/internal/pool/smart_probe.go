// smart_probe.go — smart zero-cost quota auto-refresh.
//
// Trigger-only, never sweeping: a token is probed only when real activity
// marked it dirty (lease grant, successful chat, 429 refusal) or a known
// reset instant arrives (per-model QuotaByModel ResetAt, remembered-429
// reset, Freebucks daily refill, Pacific-midnight fallback when quota
// memory exists but carries no instant). Accounts with no quota memory are
// never due, so idle accounts see zero traffic: there is no periodic
// full-sweep timer and no page-visit sweep.
//
// The tick rides maintainTick (cheap predicate checks only) and dispatches
// a due round to a detached stagger worker (single-flight, wg-tracked,
// Shutdown-cancelable), so slow probes never stall rotation and liveness
// work. Every refresh path reuses ProbeTokenDetailed — the single
// session-less path — whose live writes funnel through session
// UpdateQuotaFromProbe (stamping QuotaSavedAt, the clean flag the 60s
// fresh gate reads). No refresh path creates a session: probes are GET
// /api/v1/freebuff/session with no instance header, never admission.
package pool

import (
	"context"
	"errors"
	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/runs"
	"freebucks-proxy/backend/internal/session"
	"freebucks-proxy/backend/internal/upstream"
	"time"
)

const (
	// smartProbeFreshWindow is how long a token's cached quota counts as
	// fresh: a token probed or seeded within this window sits out the
	// next pass (UpdateQuotaFromProbe stamps QuotaSavedAt; admissions do
	// not, so a lease grant's dirty mark fires once, then the fire itself
	// stamps freshness and the gate absorbs hot-lane bursts).
	smartProbeFreshWindow = 60 * time.Second
	// smartProbeFireTimeout bounds one token's session-less probe (same
	// 15s bound ProbeAllTokens gives each token).
	smartProbeFireTimeout = 15 * time.Second
	// smartProbeStagger spaces the fires of one dispatched round so a
	// multi-token round never bursts upstream.
	smartProbeStagger = 5 * time.Second
	// smartProbeMaxPerTick caps one tick's dispatch: a wider due set waits
	// for the next pass instead of fanning out.
	smartProbeMaxPerTick = 3
	// smartProbe429BaseInterval is the probe-429 retry base: the first
	// probe 429 doubles it, each consecutive probe 429 doubles again, up
	// to the configured cap.
	smartProbe429BaseInterval = 5 * time.Minute
	// defaultSmartProbeBackoffMax is the 429-backoff doubling ceiling when
	// the knob is unset or non-positive (mirrored in config defaults +
	// catalog).
	defaultSmartProbeBackoffMax = 30 * time.Minute
)

// smartProbeBackoffCap returns the effective 429-backoff ceiling
// (zero-tolerant: a zero-value Config behaves like production).
func smartProbeBackoffCap(cfg *config.Config) time.Duration {
	if cfg != nil && cfg.SmartProbeBackoffMax > 0 {
		return cfg.SmartProbeBackoffMax
	}
	return defaultSmartProbeBackoffMax
}

// smartProbeRetryDelay doubles the base per consecutive probe-429 step
// (step 1 = first 429 = twice the base), saturating at cap.
func smartProbeRetryDelay(step int64, cap time.Duration) time.Duration {
	if cap <= 0 {
		cap = defaultSmartProbeBackoffMax
	}
	d := smartProbe429BaseInterval
	for i := int64(0); i < step; i++ {
		if d >= cap || d <= 0 || d > (1<<62)/2 {
			return cap
		}
		d *= 2
		if d >= cap || d <= 0 {
			return cap
		}
	}
	return d
}

// markProbeDirty records activity interest for entry: the next pass may
// probe it once the guards pass. Idempotent and debounce-safe: the
// eligible instant only ever moves earlier to now, never past a pending
// 429-backoff, and the 60s fresh gate plus the per-token single-flight
// absorb mark bursts from hot lanes.
func (p *Pool) markProbeDirty(entry *tokenEntry) {
	if entry == nil {
		return
	}
	entry.probeDirty.Store(true)
	now := time.Now().UnixNano()
	for {
		cur := entry.probeNextAt.Load()
		if cur != 0 && cur <= now {
			return
		}
		if entry.probeNextAt.CompareAndSwap(cur, now) {
			return
		}
	}
}

// smartProbeResetInstant returns the earliest known quota-reset instant
// for the token: per-model ResetAt rows, the remembered-429 reset (live
// window only — the snapshot fills it solely while the cooldown holds),
// and the Freebucks daily refill. Quota memory with no usable instant
// falls back to the next Pacific midnight (the daily-refill rule
// recoverAtForProbe applies when no window is known). No memory at all —
// a never-active account — reports none, so idle accounts are never due.
func smartProbeResetInstant(ss session.SessionSnapshot, rs runs.RunSnapshot, now time.Time) (time.Time, bool) {
	if len(ss.QuotaByModel) == 0 && ss.Freebucks == nil && rs.RateLimitResetsAt.IsZero() {
		return time.Time{}, false
	}
	earliest := time.Time{}
	consider := func(t time.Time) {
		if t.IsZero() {
			return
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	for _, q := range ss.QuotaByModel {
		consider(q.ResetAt)
	}
	consider(rs.RateLimitResetsAt)
	if ss.Freebucks != nil {
		consider(ss.Freebucks.Daily.ResetAt)
	}
	if earliest.IsZero() {
		return nextPacificMidnight(now), true
	}
	return earliest, true
}

// smartProbeDueToken reports whether entry is due for a smart probe at
// now, with a machine-readable reason for logs and tests. Guard order
// mirrors the health gates: locked, quarantined, live-ban, cooling,
// country, inflight, then the 60s fresh gate; only then do the triggers
// (dirty mark, reset instant past its time and newer than the last fire)
// apply.
func (p *Pool) smartProbeDueToken(tok *tokenEntry, now time.Time) (bool, string) {
	if tok == nil {
		return false, "no-entry"
	}
	if tok.locked.Load() {
		return false, "locked"
	}
	if tok.quarantine.Load() != nil {
		return false, "quarantined"
	}
	if tok.runs.BanError() != nil {
		return false, "banned"
	}
	rs := tok.runs.Snapshot()
	if now.Before(rs.CooldownUntil) {
		return false, "cooling"
	}
	if tok.runs.CountryBlockedError() != nil {
		return false, "country-blocked"
	}
	if tok.probeInflight.Load() {
		return false, "inflight"
	}
	ss := tok.session.Snapshot()
	if !ss.QuotaSavedAt.IsZero() && now.Sub(ss.QuotaSavedAt) < smartProbeFreshWindow {
		return false, "fresh"
	}
	if nextAt := tok.probeNextAt.Load(); nextAt != 0 && now.UnixNano() < nextAt {
		return false, "debounced"
	}
	if tok.probeDirty.Load() {
		return true, "dirty"
	}
	if resetAt, ok := smartProbeResetInstant(ss, rs, now); ok {
		var lastProbe time.Time
		if ns := tok.probeLastAt.Load(); ns != 0 {
			lastProbe = time.Unix(0, ns)
		}
		if !now.Before(resetAt) && resetAt.After(lastProbe) {
			return true, "reset"
		}
		return false, "reset-pending"
	}
	return false, "idle"
}

// smartProbeDue collects the due token indexes in roster order, capped to
// one round's width.
func (p *Pool) smartProbeDue(now time.Time) []int {
	toks := p.roster.Load()
	if toks == nil {
		return nil
	}
	var due []int
	for i, tok := range *toks {
		if len(due) >= smartProbeMaxPerTick {
			break
		}
		if ok, _ := p.smartProbeDueToken(tok, now); ok {
			due = append(due, i)
		}
	}
	return due
}

// smartProbeTick runs one scheduler pass on the maintain clock.
func (p *Pool) smartProbeTick(ctx context.Context) {
	p.smartProbeTickAt(ctx, time.Now())
}

// smartProbeTickAt is smartProbeTick with the clock injected (tests).
//
// The tick itself never probes: it runs only the cheap due decision on the
// maintain goroutine and dispatches a due round to a detached stagger
// worker (single-flight, wg-tracked, Shutdown-cancelable), so slow probes
// never block maintainTick's rotation and liveness work.
func (p *Pool) smartProbeTickAt(ctx context.Context, now time.Time) {
	cfg := p.cfg.Load()
	if cfg == nil || !cfg.SmartProbeEnabled {
		return
	}
	// Shutdown is terminal: never dispatch past it (see maintainTick's
	// draining gate for the same rule on admissions).
	if p.draining.Load() {
		return
	}
	due := p.smartProbeDue(now)
	if len(due) == 0 {
		return
	}
	// Round single-flight: a stagger worker still running suppresses this
	// tick. Due tokens keep their dirty/reset state, so nothing is lost —
	// the next pass re-collects them.
	if !p.smartProbeInflight.CompareAndSwap(false, true) {
		p.logger.Debug("pool: smart probe round already in flight, tick suppressed", "tokens", due)
		return
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer p.smartProbeInflight.Store(false)
		for i, idx := range due {
			if i > 0 {
				t := time.NewTimer(smartProbeStagger)
				select {
				case <-ctx.Done():
					t.Stop()
					return
				case <-t.C:
				}
			}
			p.smartProbeFireOne(ctx, idx)
		}
	}()
	disp := make([]int, 0, len(due))
	for _, idx := range due {
		disp = append(disp, idx+1)
	}
	p.logger.Debug("pool: smart probe dispatch", "tokens", disp)
}

// smartProbeFireOne fires one due token's session-less probe and folds the
// outcome into its schedule: a 429 doubles the retry delay up to the
// configured cap (first 429 = twice the base); a transport error parks
// one quiet base interval without consuming a backoff step; any other
// outcome clears the schedule — the next fire waits for fresh activity or
// the next reset instant. The probe itself never installs cooldowns or
// touches ledgers: quota truth arrives through ProbeTokenDetailed's
// UpdateQuotaFromProbe write, everything else is scheduler-local.
func (p *Pool) smartProbeFireOne(ctx context.Context, idx int) {
	toks := p.roster.Load()
	if toks == nil || idx < 0 || idx >= len(*toks) {
		return
	}
	tok := (*toks)[idx]
	if !tok.probeInflight.CompareAndSwap(false, true) {
		return
	}
	defer tok.probeInflight.Store(false)
	fireCtx, cancel := context.WithTimeout(ctx, smartProbeFireTimeout)
	outcome, _, err := p.ProbeTokenDetailed(fireCtx, idx)
	cancel()
	now := time.Now()
	tok.probeLastAt.Store(now.UnixNano())
	tok.probeDirty.Store(false)
	var rle *upstream.RateLimitError
	if errors.As(err, &rle) || errors.Is(err, upstream.ErrRateLimited) || outcome.Status == "rate_limited" {
		// Refused: a parseable 429 body arrives as outcome rate_limited
		// with a nil error (the body IS the quota truth: limit,
		// recentCount, resetAt), while an unparseable 429 surfaces as
		// ErrRateLimited. Both step the same doubling schedule.
		step := tok.probeBackoffStep.Add(1)
		delay := smartProbeRetryDelay(step, smartProbeBackoffCap(p.cfg.Load()))
		tok.probeNextAt.Store(now.Add(delay).UnixNano())
		p.logger.Info("pool: smart probe refused, backing off", "token", idx+1, "outcome", outcome.Status, "retry_in", delay)
		return
	}
	if outcome.Status == "freebucks_exhausted" {
		// Quota truth with a known refill: sleep exactly until reset
		// instead of doubling blindly. The reset-instant trigger covers
		// drift; this parks the fire itself.
		tok.probeBackoffStep.Store(0)
		tok.probeNextAt.Store(smartProbeExhaustedResume(outcome, now).UnixNano())
		p.logger.Info("pool: smart probe found exhausted freebucks, sleeping to reset", "token", idx+1, "outcome", outcome.Status, "reset_at", outcome.ResetAt)
		return
	}
	if err != nil {
		// Transport failure: no quota truth arrived, so no backoff step —
		// but park one quiet interval instead of re-firing on the next
		// pass while upstream is unreachable.
		tok.probeNextAt.Store(now.Add(smartProbe429BaseInterval).UnixNano())
		p.logger.Debug("pool: smart probe errored, parking one interval", "token", idx+1, "outcome", outcome.Status, "err", err)
		return
	}
	tok.probeBackoffStep.Store(0)
	tok.probeNextAt.Store(0)
	p.logger.Debug("pool: smart probe ok", "token", idx+1, "outcome", outcome.Status)
}

// smartProbeExhaustedResume sleeps an exhausted account exactly until its
// known refill: the probe outcome's reset_at when it parses and lies ahead,
// else the next Pacific midnight (same fallback as recoverAtForProbe).
func smartProbeExhaustedResume(outcome ProbeTokenOutcome, now time.Time) time.Time {
	if t, err := time.Parse(time.RFC3339, outcome.ResetAt); err == nil && t.After(now) {
		return t
	}
	return nextPacificMidnight(now)
}
