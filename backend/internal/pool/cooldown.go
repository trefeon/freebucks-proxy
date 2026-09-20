package pool

import (
	"errors"
	"fmt"
	"freebucks-proxy/backend/internal/notify"
	"freebucks-proxy/backend/internal/runs"
	"freebucks-proxy/backend/internal/upstream"
	"time"
)

// CooldownToken puts token in a cooldown window of duration d (auth-reject
// recovery). Out-of-range tokens are ignored.
func (p *Pool) CooldownToken(token int, d time.Duration) {
	toks := p.roster.Load()
	if token < 0 || token >= len(*toks) {
		return
	}
	(*toks)[token].runs.Cooldown(d)
}

// CooldownTokenRateLimit applies a rate-limit cooldown to token
// (remembered so Acquire surfaces 429 + Retry-After during the window).
// Out-of-range tokens are ignored. When the refusal is spend_limited
// (issue #122), the event is also counted on the token's spend ledger —
// the $ ceiling is server-enforced, so the ledger only records the event.
func (p *Pool) CooldownTokenRateLimit(token int, rle *upstream.RateLimitError) {
	toks := p.roster.Load()
	if token < 0 || token >= len(*toks) || rle == nil {
		return
	}
	(*toks)[token].runs.CooldownRateLimit(rle)
	// Smart probe (smart_probe.go): a 429 refusal marks refresh interest —
	// the remembered reset instant schedules the re-probe once the
	// cooldown window lifts.
	p.markProbeDirty((*toks)[token])
	if rle.Status == "spend_limited" {
		p.recordSpendLimited(token)
	}
	p.recordMismatchEscalation(token+1, rle) // 1-based key: 0 is the bridge-shared window
}

// CooldownTokenBan applies a ban cooldown to token (remembered so
// Acquire surfaces 403 banned + resumes-at during the window) and fires the
// token_banned webhook alert (issue #48, throttled per event type).
func (p *Pool) CooldownTokenBan(token int, be *upstream.BanError) {
	toks := p.roster.Load()
	if token < 0 || token >= len(*toks) || be == nil {
		return
	}
	tok := (*toks)[token]
	tok.runs.CooldownBan(be)
	p.notifyBan(token+1, "")
	// Quarantine only while the ban is still live after CooldownBan (hard,
	// or a future resumes_at): an expired temporary ban — resumes_at in the
	// past — is already lifted upstream, CooldownBan kept no ban memory,
	// and marking the token terminal would kill a healthy account.
	if tok.runs.BanError() != nil {
		p.quarantineToken(tok, "banned", be)
	}
	// Terminal-hint mirror (hint only, never authoritative): a live ban
	// persists, an already-lifted one clears instead.
	p.storeBanHint(tok)
}

// CooldownTokenCountryBlocked applies a country-block cooldown to token
// (remembered so Acquire surfaces the region-block error during the window
// instead of re-hitting upstream). Time-bound park only, never a terminal
// quarantine: short region/egress transients ride out the window and the
// cooldown expiry revives the token automatically.
func (p *Pool) CooldownTokenCountryBlocked(token int, cbe *upstream.CountryBlockedError) {
	toks := p.roster.Load()
	if token < 0 || token >= len(*toks) || cbe == nil {
		return
	}
	(*toks)[token].runs.CooldownCountryBlocked(cbe)
	// Terminal-hint mirror (hint only): the live window persists so a
	// restart skips one doomed probe; expiry re-probes.
	p.storeCountryHint((*toks)[token], (*toks)[token].runs.CooldownUntil())
}

// indexOfEntry resolves entry's CURRENT 0-based roster position (-1 when
// retired): display indices and positional mismatch keys stay correct
// across dashboard reorders. Cooldown/quarantine state itself lives on the
// entry, so the wrong position can never punish the wrong account.
func (p *Pool) indexOfEntry(entry *tokenEntry) int {
	if entry == nil {
		return -1
	}
	toks := p.roster.Load()
	for i, t := range *toks {
		if t == entry {
			return i
		}
	}
	return -1
}

// CooldownLease puts the lease's own entry in a cooldown window of
// duration d (swap-safe — see InvalidateLeaseSession).
func (p *Pool) CooldownLease(lease *Lease, d time.Duration) {
	if lease == nil || lease.entry == nil {
		return
	}
	lease.entry.runs.Cooldown(d)
}

// CooldownLeaseRateLimit applies a rate-limit cooldown to the lease's own
// entry (swap-safe). Spend counting hits the entry ledger directly;
// mismatch escalation keeps the entry's CURRENT 1-based position.
func (p *Pool) CooldownLeaseRateLimit(lease *Lease, rle *upstream.RateLimitError) {
	if lease == nil || lease.entry == nil || rle == nil {
		return
	}
	lease.entry.runs.CooldownRateLimit(rle)
	if rle.Status == "spend_limited" {
		lease.entry.ledger.recordSpendLimited()
	}
	if idx := p.indexOfEntry(lease.entry); idx >= 0 {
		p.recordMismatchEscalation(idx+1, rle) // 1-based key: 0 is the bridge-shared window
	}
}

// CooldownLeaseBan applies a ban cooldown to the lease's own entry and
// fires the token_banned webhook alert (swap-safe). Quarantine takes the
// entry directly; the display index is resolved live.
func (p *Pool) CooldownLeaseBan(lease *Lease, be *upstream.BanError) {
	if lease == nil || lease.entry == nil || be == nil {
		return
	}
	tok := lease.entry
	tok.runs.CooldownBan(be)
	if idx := p.indexOfEntry(tok); idx >= 0 {
		p.notifyBan(idx+1, "")
	}
	if tok.runs.BanError() != nil {
		p.quarantineToken(tok, "banned", be)
	}
	// Terminal-hint mirror (hint only, never authoritative).
	p.storeBanHint(tok)
}

// CooldownLeaseCountryBlocked applies a country-block cooldown to the
// lease's own entry (swap-safe — see CooldownLeaseBan). Time-bound park
// only, never a terminal quarantine (see CooldownTokenCountryBlocked).
func (p *Pool) CooldownLeaseCountryBlocked(lease *Lease, cbe *upstream.CountryBlockedError) {
	if lease == nil || lease.entry == nil || cbe == nil {
		return
	}
	lease.entry.runs.CooldownCountryBlocked(cbe)
	// Terminal-hint mirror (hint only).
	p.storeCountryHint(lease.entry, lease.entry.runs.CooldownUntil())
}

// CooldownBridge puts the bridge entry's token in a cooldown window of
// duration d (auth-reject recovery).
func (p *Pool) CooldownBridge(lease *Lease, d time.Duration) {
	if lease == nil || lease.Bridge == nil {
		return
	}
	lease.Bridge.runs.Cooldown(d)
}

// CooldownBridgeRateLimit applies a rate-limit cooldown to the bridge entry
// (remembered so AcquireBridge surfaces 429 + Retry-After). When the refusal
// is spend_limited (issue #122), the event is also counted on the entry's
// spend ledger — the $ ceiling is server-enforced, so the ledger only
// records the event.
func (p *Pool) CooldownBridgeRateLimit(lease *Lease, rle *upstream.RateLimitError) {
	if lease == nil || lease.Bridge == nil || rle == nil {
		return
	}
	lease.Bridge.runs.CooldownRateLimit(rle)
	if rle.Status == "spend_limited" {
		p.bridgeMu.Lock()
		defer p.bridgeMu.Unlock()
		p.bridgeRecordSpendLimited(lease.Bridge)
	}
	p.recordMismatchEscalation(0, rle) // bridge: tokenIndex 0, shared window
}

// CooldownBridgeBan applies a ban cooldown to the bridge entry (remembered
// so AcquireBridge surfaces 403 banned + resumes-at during the window) and
// fires the token_banned webhook alert (issue #48, throttled).
func (p *Pool) CooldownBridgeBan(lease *Lease, be *upstream.BanError) {
	if lease == nil || lease.Bridge == nil || be == nil {
		return
	}
	lease.Bridge.runs.CooldownBan(be)
	p.notifyBan(0, "")
}

// CooldownBridgeCountryBlocked applies a country-block cooldown to the
// bridge entry (remembered so AcquireBridge surfaces the region-block error
// during the ~15m window instead of re-hitting upstream).
func (p *Pool) CooldownBridgeCountryBlocked(lease *Lease, cbe *upstream.CountryBlockedError) {
	if lease == nil || lease.Bridge == nil || cbe == nil {
		return
	}
	lease.Bridge.runs.CooldownCountryBlocked(cbe)
}

// notifyBan fires the token_banned webhook (issue #48). tokenIndex is the
// 1-based pooled token index (0 = bridge). model is the requested model
// when the caller knows it ("" otherwise). Throttled by the sender.
func (p *Pool) notifyBan(tokenIndex int, model string) {
	p.notifyMu.Lock()
	defer p.notifyMu.Unlock()
	n := p.notify
	if n == nil {
		return
	}
	n.Send(notify.Event{
		Event: "token_banned", TokenIndex: tokenIndex, Model: model,
		Message: "a FreeBuff token was classified banned upstream (403)",
	})
}

// classifyTarget selects the mode-specific recovery policy in
// classifyAndCooldown (issue #260): pooled quarantines a live-banned
// account while bridge evicts a dead token on 401 and never quarantines
// (per-request client tokens are never marked terminal). Country blocks
// and 401 invalids are time-bound parks, never quarantine.

// classifiedError carries the classification result of one upstream error:
// the mode-agnostic recovery policy is applied first (Cooldown*), and the
// site-specific recovery (pooled quarantine vs bridge eviction, webhook
// notify index/model, spend_limited ledger, quota fallback, bucket
// aggregation) reads the flags at the calling site.
type classifiedError struct {
	authRejected   bool
	rateLimited    *upstream.RateLimitError
	ipCapped       *upstream.IpCappedError
	banned         *upstream.BanError
	countryBlocked *upstream.CountryBlockedError
	limitedIp      *upstream.LimitedIpError
	spendLimited   bool
}

// classifyAndCooldown runs the shared upstream-error classification cascade
// against one entry's run manager (issue #260): asType(err) sets the
// matching classifiedError flag for the caller. Only a terminal ban writes
// token state (runs.CooldownBan, so the ban is remembered and the caller
// can quarantine); every other refusal only classifies — 429s carry their
// upstream RetryAfter to the caller for surfacing, never a cooldown write.
func (p *Pool) classifyAndCooldown(runsMgr *runs.RunManager, err error) *classifiedError {
	c := &classifiedError{}
	if err == nil {
		return c
	}
	if errors.Is(err, upstream.ErrAuthRejected) {
		c.authRejected = true
	}
	if rle := asRateLimit(err); rle != nil {
		c.rateLimited = rle
		c.spendLimited = rle.Status == "spend_limited"
	}
	if ice := asIpCapped(err); ice != nil {
		c.ipCapped = ice
	}
	if be := asBan(err); be != nil {
		c.banned = be
		runsMgr.CooldownBan(be)
	}
	if cbe := asCountryBlocked(err); cbe != nil {
		c.countryBlocked = cbe
	}
	if lie := asLimitedIp(err); lie != nil {
		c.limitedIp = lie
	}
	return c
}

// appendBan adds be to dst unless an equivalent error is present.
func appendBan(dst []*upstream.BanError, be *upstream.BanError) []*upstream.BanError {
	for _, existing := range dst {
		if existing.Error() == be.Error() {
			return dst
		}
	}
	return append(dst, be)
}

// mismatchEscalation is the issue #140 escalation guard's state for one
// token: free_mode_invalid_agent_model 403s inside stormWindow mean the
// registry is serving an id upstream retired — exactly how the v0.11.3-era
// PREFER_MAX_MODELS over-upgrade escalated accounts to `banned`. N hits in
// the window fire ONE operator webhook (the sender throttles repeats); the
// per-hit bounded cooldown already stops the request-path amplification.
type mismatchEscalation struct {
	hits      []time.Time // refusal timestamps, oldest first
	lastStorm time.Time   // zero = no webhook fired yet in this storm
}

const (
	mismatchWindow    = 60 * time.Second
	mismatchThreshold = 3
)

// recordMismatchEscalation counts one free_mode_invalid_agent_model hit for
// token and fires agent_model_mismatch_escalation once per window when the
// count crosses the threshold. Bridge entries share the pooled path through
// CooldownTokenRateLimit/CooldownBridgeRateLimit, so both surfaces alert.
// tokenIndex is the 1-based pooled token index (0 = bridge, the shared
// window) — the same convention as notifyBan and the
// RemoveTokenAt/RemoveLastToken reindex — so a pooled token never shares
// its window with the bridge entries.
func (p *Pool) recordMismatchEscalation(tokenIndex int, rle *upstream.RateLimitError) {
	fire, model := p.roster.recordMismatch(tokenIndex, rle)
	if !fire {
		return
	}
	p.notifyMu.Lock()
	n := p.notify
	p.notifyMu.Unlock()
	if n == nil {
		return
	}
	n.Send(notify.Event{
		Event: "agent_model_mismatch_escalation", TokenIndex: tokenIndex,
		Model:   model,
		Message: "3+ free_mode_invalid_agent_model refusals in 60s on one token — the registry is likely serving a model upstream retired; refresh/restart or check MODELS_ALLOW before upstream escalates to ban",
	})
}

// UnlockToken clears any cooldown/rate-limit/ban lock on token so Acquire
// can use it again (dashboard unlock action). The dashboard unlock is the
// operator's explicit action to restore a token after appealing the account
// upstream, so it also clears any terminal-quarantine marker.
func (p *Pool) UnlockToken(token int) error {
	toks := p.roster.Load()
	if token < 0 || token >= len(*toks) {
		return fmt.Errorf("pool: token %d out of range", token)
	}
	(*toks)[token].runs.ClearCooldowns()
	(*toks)[token].quarantine.Store(nil)
	// The operator restored the account: drop its terminal hint too, or a
	// restart would keep skipping one healthy probe per walk.
	p.clearCooldownHintFor((*toks)[token])
	return nil
}

// quarantineToken marks the fixed pooled token entry permanently
// ineligible for leasing: its account reached a terminal state (a live ban)
// that the pool must never revive. Country blocks and 401 invalids are
// time-bound parks, never quarantine (see CooldownTokenCountryBlocked and
// the acquire paths). The marker survives across Acquire calls (the
// failover loop skips it every pass, so no re-admission attempts) and is
// cleared by ban-expiry lift, by UnlockToken, or by the entry rebuild an
// AUTH_TOKENS change triggers (SetConfig replaces the whole entry). It is
// a no-op for bridge entries (per-request tokens are never quarantined — a
// bridge refusal surfaces to the client as today).
//
// The caller passes the ENTRY it holds, not an index: an index could be
// reused by a concurrent RemoveLastToken+AddToken, and quarantining by
// index would mark the fresh healthy token instead of the dead one. Logs
// exactly one slog.Warn per token: CompareAndSwap guards the single fire,
// so a token that re-hits the same terminal refusal while already
// quarantined does not re-log.
func (p *Pool) quarantineToken(tok *tokenEntry, reason string, err error) {
	if tok == nil {
		return
	}
	rec := &quarantineState{reason: reason, err: err}
	if err != nil {
		rec.detail = err.Error()
		// Lift-aware quarantine: a ban carrying a FUTURE resumes_at is a
		// time-limited upstream state — the account auto-lifts at that
		// instant (CooldownBan keeps the ban memory only until then, and
		// banView renders it as "temporary"). Record the lift on the marker
		// so it expires with the window and the pool re-admits after the
		// unban, exactly as the "quarantine only while the ban is still
		// live" caller comments state. Hard bans (no resumes_at) stay
		// permanent (liftAt zero). The "country_blocked" and "invalid"
		// reasons below are legacy: no caller produces them anymore.
		var be *upstream.BanError
		if reason == "banned" && errors.As(err, &be) && !be.ResumesAt.IsZero() && be.ResumesAt.After(time.Now()) {
			rec.liftAt = be.ResumesAt
		}
	}
	if tok.quarantine.CompareAndSwap(nil, rec) {
		p.logger.Warn("pool: token quarantined (terminal account state)",
			"token_label", tokenEntryLabel(tok), "state", reason, "reason", rec.detail)
	}
}

// clearLiftedQuarantine clears the quarantine marker of a token whose
// time-limited terminal state (temporary ban) has lifted: the upstream
// unban is automatic at resumes_at, so the account is serviceable again and
// the pool must not keep treating it as terminal. A hard ban keeps its
// marker — only an operator action (UnlockToken) or an AUTH_TOKENS slot
// replacement clears it.
// Returns true when a marker was cleared. CompareAndSwap-guarded:
// concurrent callers race harmlessly and exactly one logs the lift.
func (p *Pool) clearLiftedQuarantine(tok *tokenEntry) bool {
	if tok == nil {
		return false
	}
	q := tok.quarantine.Load()
	if q == nil || q.liftAt.IsZero() || time.Now().Before(q.liftAt) {
		return false
	}
	if tok.quarantine.CompareAndSwap(q, nil) {
		p.logger.Info("pool: quarantine lifted (temporary ban expired)",
			"token_label", tokenEntryLabel(tok), "state", q.reason)
		// The upstream unban lifted the terminal state: the ban hint is
		// stale, drop it so the token re-admits immediately.
		p.clearCooldownHintFor(tok)
		return true
	}
	return false
}

// LockToken administratively excludes token from Acquire without clearing
// its cooldown state (dashboard lock action).
func (p *Pool) LockToken(token int) error {
	toks := p.roster.Load()
	if token < 0 || token >= len(*toks) {
		return fmt.Errorf("pool: token %d out of range", token)
	}
	(*toks)[token].locked.Store(true)
	return nil
}

// UnlockLockToken clears the administrative lock on token so Acquire can
// use it again (dashboard unlock-lock action).
func (p *Pool) UnlockLockToken(token int) error {
	toks := p.roster.Load()
	if token < 0 || token >= len(*toks) {
		return fmt.Errorf("pool: token %d out of range", token)
	}
	(*toks)[token].locked.Store(false)
	return nil
}

// LockBridgeEntry administratively excludes a bridge entry from
// AcquireBridge. key is the SHA-256 hash prefix (tokenKey) (#187).
func (p *Pool) LockBridgeEntry(key string) error {
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	e, ok := p.bridge[key]
	if !ok {
		return fmt.Errorf("pool: bridge entry %s not found", key)
	}
	e.locked.Store(true)
	return nil
}

// UnlockBridgeEntry clears the administrative lock on a bridge entry so
// AcquireBridge can use it again (#187).
func (p *Pool) UnlockBridgeEntry(key string) error {
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	e, ok := p.bridge[key]
	if !ok {
		return fmt.Errorf("pool: bridge entry %s not found", key)
	}
	e.locked.Store(false)
	return nil
}
