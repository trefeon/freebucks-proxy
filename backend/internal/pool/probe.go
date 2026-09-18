package pool

import (
	"context"
	"errors"
	"fmt"
	"freebuff-proxy/backend/internal/upstream"
	"math"
	"sync"
	"time"
)

// ProbeTokenOutcome describes the status and Freebucks balance of a single pooled token.
type ProbeTokenOutcome struct {
	Index        int     `json:"index"`
	Email        string  `json:"email,omitempty"`
	Status       string  `json:"status"` // "ok", "banned", "freebucks_exhausted", "auth_rejected", "country_blocked", "rate_limited", "error"
	Detail       string  `json:"detail,omitempty"`
	SpendableFB  float64 `json:"spendable_freebucks"`
	DailyLimitFB float64 `json:"daily_limit_freebucks"`
	DailySpentFB float64 `json:"daily_spent_freebucks"`
	Quarantined  bool    `json:"quarantined"`
	Cooling      bool    `json:"cooling"`
	CoolingUntil string  `json:"cooldown_until,omitempty"`
	ResetAt      string  `json:"reset_at,omitempty"`
}

// probeFreebucksExhausted mirrors freebucksCappedForSnapshot's cap decision
// without naming a model: the price floor is the minimum across the wire
// Prices map (nothing can start below it; a model-specific admission may
// still cap above it). The monthly dollar allowance gates even when the
// account is quota-exempt, exactly like admission (issue #330). With no
// Prices entries the floor is unknown, so only a zero balance (legacy
// conservative gate) or a spent monthly period exhausts. Recovery is the
// earliest future instant among the daily reset, wallet bonus, and (when the
// monthly period blocks) monthly reset, falling back to the next Pacific
// midnight like admission does without timestamps. Probe data is live by
// construction, so unlike admission there is no stale-window escape:
// admission stays the live per-request authority.
func probeFreebucksExhausted(fb *upstream.FreebucksInfo) (exhausted bool, reason string, recoverAt time.Time) {
	if fb == nil {
		return false, "", time.Time{}
	}
	monthlySpent := fb.Monthly != nil && fb.Monthly.RemainingUsd <= 0
	if fb.QuotaExempt && !monthlySpent {
		return false, "", time.Time{}
	}
	spendable := fb.Spendable()
	if len(fb.Prices) == 0 {
		if monthlySpent {
			return true, "monthly Freebucks allowance exhausted", recoverAtForProbe(fb, true)
		}
		if spendable <= 0 {
			return true, "0 Freebucks left", recoverAtForProbe(fb, false)
		}
		return false, "", time.Time{}
	}
	floor := math.Inf(1)
	for _, price := range fb.Prices {
		if price < floor {
			floor = price
		}
	}
	if monthlySpent {
		return true, "monthly Freebucks allowance exhausted", recoverAtForProbe(fb, true)
	}
	if spendable < floor {
		return true, fmt.Sprintf("spendable %g below cheapest model price %g", spendable, floor), recoverAtForProbe(fb, false)
	}
	return false, "", time.Time{}
}

// recoverAtForProbe returns the earliest future recovery instant among the
// account's refill windows, or the next Pacific midnight when none is known.
func recoverAtForProbe(fb *upstream.FreebucksInfo, monthlySpent bool) time.Time {
	now := time.Now()
	earliest := time.Time{}
	candidates := []time.Time{fb.Daily.ResetAt, fb.Wallet.NextBonusAt}
	if monthlySpent && fb.Monthly != nil {
		candidates = append(candidates, fb.Monthly.ResetAt)
	}
	for _, t := range candidates {
		if t.IsZero() || !t.After(now) {
			continue
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	if earliest.IsZero() {
		return nextPacificMidnight(now)
	}
	return earliest
}

// ProbeTokenDetailed probes token index without claiming or creating any session slot
// and without touching any model. It synchronizes quotas, standing, and Freebucks,
// quarantines banned accounts until unbanned, and locks/cooldowns accounts whose
// Freebucks are exhausted until their daily refill.
func (p *Pool) ProbeTokenDetailed(ctx context.Context, token int) (ProbeTokenOutcome, *upstream.SessionState, error) {
	toks := p.roster.Load()
	if toks == nil || token < 0 || token >= len(*toks) {
		return ProbeTokenOutcome{Index: token, Status: "error", Detail: "token index out of range"}, nil, fmt.Errorf("pool: token %d out of range", token)
	}
	tok := (*toks)[token]

	outcome := ProbeTokenOutcome{
		Index: token,
		Email: tok.Email(),
	}

	st, err := tok.client.ProbeAccount(ctx)

	// Case 1: Account is BANNED upstream
	var be *upstream.BanError
	isBanned := errors.As(err, &be) || errors.Is(err, upstream.ErrBanned) || (st != nil && st.Status == "banned")
	if isBanned {
		if be == nil {
			be = &upstream.BanError{Body: "upstream account banned"}
		}
		// Defense-in-depth: a banned status observed via state alone (nil error)
		// still reports as a refusal, so callers never see (banned, nil).
		if err == nil {
			err = be
		}
		p.CooldownTokenBan(token, be)
		outcome.Status = "banned"
		outcome.Quarantined = true
		outcome.Detail = "upstream account banned; locked in quarantine until unbanned"
		return outcome, st, err
	}

	// Case 2: Country blocked
	var cbe *upstream.CountryBlockedError
	if errors.As(err, &cbe) || errors.Is(err, upstream.ErrCountryBlocked) {
		if cbe != nil {
			p.CooldownTokenCountryBlocked(token, cbe)
		}
		outcome.Status = "country_blocked"
		outcome.Detail = "upstream country blocked"
		return outcome, st, err
	}

	// Case 3: Auth rejected (401 invalid credentials)
	if errors.Is(err, upstream.ErrAuthRejected) {
		outcome.Status = "auth_rejected"
		outcome.Detail = "upstream authentication rejected (invalid token)"
		return outcome, st, err
	}

	// Case 4: Rate limited (429)
	var rle *upstream.RateLimitError
	if errors.As(err, &rle) || errors.Is(err, upstream.ErrRateLimited) {
		outcome.Status = "rate_limited"
		outcome.Detail = "upstream rate limited"
		return outcome, st, err
	}

	// Other network / transport errors (excluding ErrNoActiveSession)
	if err != nil && !errors.Is(err, upstream.ErrNoActiveSession) {
		outcome.Status = "error"
		outcome.Detail = err.Error()
		return outcome, st, err
	}

	// Case 5: Token is valid and healthy (err == nil), or valid-but-idle
	// (errors.Is(err, upstream.ErrNoActiveSession) with a non-nil state:
	// the probe returns the pre-join meter ALONGSIDE the sentinel). Either
	// way the live meter below is authoritative — persist it, so an
	// idle-with-balance token populates quota/standing/freebucks without
	// ever holding a session slot. Ban/country/auth/rate-limit arms above
	// are untouched. The sentinel is swallowed here (nil error below):
	// ProbeToken re-emits it alongside the state for legacy callers.
	// If previously quarantined for a ban, the account is now confirmed unbanned! Lift quarantine.
	if q := tok.quarantine.Load(); q != nil && q.reason == "banned" {
		tok.quarantine.Store(nil)
		tok.runs.ClearCooldowns()
		p.clearCooldownHintFor(tok)
		p.logger.Info("pool: quarantine lifted (probe confirmed account unbanned)", "token", token+1)
	}

	// Update quota, standing, referral, and freebucks from the live probe
	// response — including the idle-with-balance state that rides with
	// ErrNoActiveSession.
	if st != nil {
		tok.session.UpdateQuotaFromProbe(st)
	}

	// Discover email if not yet known
	if tok.email.Load() == nil {
		p.asyncAccountInfoFetch(tok)
	}
	if email := tok.Email(); email != "" {
		outcome.Email = email
	}

	// Check Freebucks position against the same semantics admission gates on
	// (cheapest-price floor + monthly allowance, exempt-aware).
	if st != nil && st.Freebucks != nil {
		fb := st.Freebucks
		spendable := fb.Spendable()
		outcome.SpendableFB = spendable
		outcome.DailyLimitFB = fb.Daily.Limit
		outcome.DailySpentFB = fb.Daily.Spent

		if exhausted, reason, recoverAt := probeFreebucksExhausted(fb); exhausted {
			// Exhausted Freebucks: lock until recovery
			resetAt := recoverAt
			if resetAt.IsZero() || !resetAt.After(time.Now()) {
				resetAt = nextPacificMidnight(time.Now())
			}
			d := time.Until(resetAt)
			if d > 0 {
				p.CooldownToken(token, d)
			}
			outcome.Status = "freebucks_exhausted"
			outcome.Cooling = true
			outcome.CoolingUntil = time.Now().Add(d).Format(time.RFC3339)
			outcome.ResetAt = resetAt.UTC().Format(time.RFC3339)
			outcome.Detail = fmt.Sprintf("%s; locked until reset at %s", reason, resetAt.UTC().Format("15:04 UTC"))
		} else {
			// Freebucks available: clear any freebucks-exhaustion cooldown
			if tok.runs.BanError() == nil {
				p.clearCooldownHintFor(tok)
			}
			outcome.Status = "ok"
			outcome.Detail = fmt.Sprintf("%.1f Freebucks available", spendable)
		}
	} else {
		outcome.Status = "ok"
		outcome.Detail = "token valid (no active session)"
	}

	return outcome, st, nil
}

// ProbeAllTokens probes all tokens in the roster concurrently with bounded concurrency.
func (p *Pool) ProbeAllTokens(ctx context.Context) ([]ProbeTokenOutcome, error) {
	toks := p.roster.Load()
	if toks == nil || len(*toks) == 0 {
		return []ProbeTokenOutcome{}, nil
	}
	count := len(*toks)
	outcomes := make([]ProbeTokenOutcome, count)

	concurrency := 4
	if concurrency > count {
		concurrency = count
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for i := range count {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()

			probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			outcome, _, _ := p.ProbeTokenDetailed(probeCtx, idx)
			cancel()

			mu.Lock()
			outcomes[idx] = outcome
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	return outcomes, nil
}
