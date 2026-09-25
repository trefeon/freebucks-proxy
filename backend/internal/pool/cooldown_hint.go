package pool

import (
	"encoding/json"
	"time"
)

// cooldown_hint.go — timestamped DB-hints for terminal cooldowns.
//
// Hints, never authoritative: readers treat a missing row as
// eligible and an expired row as absent, the request hot path reads memory
// only (never the store), and deleting the keys restores exactly today's
// behavior (mem-only). Raw tokens never cross the boundary: every key
// is a SHA-256 hex (poolTokenHash).

const (
	// Terminal hint kinds — the only kinds ever stored. 429s (including
	// spend_limited / freebucks windows), ip_capped, limited_ip and burst
	// timers are NEVER persisted: ip_capped is a per-IP wall and persisting
	// it per-token spreads one IP's limit to all accounts.
	cooldownHintKindBan            = "banned"
	cooldownHintKindCountryBlocked = "country_blocked"
)

// poolCooldownBlob is one token's terminal-cooldown hint. UntilMs is Unix
// millis UTC; 0 means a permanent terminal state (hard ban, no resumes_at)
// that only an operator action (UnlockToken) or a key delete lifts.
type poolCooldownBlob struct {
	Kind    string `json:"kind"`
	UntilMs int64  `json:"until_ms"`
}

// poolCooldownKey returns the pool_state key for one token's cooldown hint.
func poolCooldownKey(tokenHash string) string { return poolCooldownPrefix + tokenHash }

// poolPersistBackend returns the wired PoolPersist (nil disables). Small
// helper so hint/survivor writers share one lock discipline.
func (p *Pool) poolPersistBackend() PoolPersist {
	p.persistMu.Lock()
	defer p.persistMu.Unlock()
	return p.persist
}

// cooldownHintFresh reports whether tokenHash carries a live terminal hint:
// missing reads as eligible, expired reads as absent (lazily dropped).
// Memory-only: the request hot path never touches the store.
func (p *Pool) cooldownHintFresh(tokenHash string, now time.Time) bool {
	p.cooldownHintMu.Lock()
	defer p.cooldownHintMu.Unlock()
	if len(p.cooldownHints) == 0 {
		return false
	}
	b, ok := p.cooldownHints[tokenHash]
	if !ok {
		return false
	}
	if b.UntilMs > 0 && b.UntilMs <= now.UnixMilli() {
		delete(p.cooldownHints, tokenHash)
		return false
	}
	return true
}

// storeCooldownHint records blob for tokenHash in memory; the spill loop
// persists it behind via the dirty-flag flush (nil store = memory only).
// Unknown kinds are refused. No store I/O happens here: the request hot
// path never blocks on disk.
func (p *Pool) storeCooldownHint(tokenHash string, blob poolCooldownBlob) {
	if tokenHash == "" {
		return
	}
	if blob.Kind != cooldownHintKindBan && blob.Kind != cooldownHintKindCountryBlocked {
		return
	}
	p.cooldownHintMu.Lock()
	if p.cooldownHints == nil {
		p.cooldownHints = make(map[string]poolCooldownBlob)
	}
	p.cooldownHints[tokenHash] = blob
	p.cooldownHintMu.Unlock()
	p.markPersistDirty()
}

// clearCooldownHint drops tokenHash's hint from memory; the spill flush
// converges the row behind (orphan prune). Memory pops first so the common
// case (no hint) arms nothing. No store I/O happens here.
func (p *Pool) clearCooldownHint(tokenHash string) {
	if tokenHash == "" {
		return
	}
	p.cooldownHintMu.Lock()
	_, had := p.cooldownHints[tokenHash]
	delete(p.cooldownHints, tokenHash)
	p.cooldownHintMu.Unlock()
	if !had {
		return
	}
	p.markPersistDirty()
}

// clearCooldownHintFor drops the hint for a pooled entry (nil-safe).
func (p *Pool) clearCooldownHintFor(entry *tokenEntry) {
	if entry == nil || entry.token == "" {
		return
	}
	p.clearCooldownHint(poolTokenHash(entry.token))
}

// storeBanHint mirrors a pooled entry's live ban memory into the hint: a
// live ban stores (hard bans persist with UntilMs 0 = permanent); no live
// ban clears instead (an expired temporary ban is already lifted upstream,
// so keeping a hint would park a healthy token).
func (p *Pool) storeBanHint(entry *tokenEntry) {
	if entry == nil || entry.token == "" || entry.runs == nil {
		return
	}
	be := entry.runs.BanError()
	if be == nil {
		p.clearCooldownHint(poolTokenHash(entry.token))
		return
	}
	var untilMs int64
	if !be.ResumesAt.IsZero() {
		untilMs = be.ResumesAt.UnixMilli()
	}
	p.storeCooldownHint(poolTokenHash(entry.token), poolCooldownBlob{Kind: cooldownHintKindBan, UntilMs: untilMs})
}

// storeCountryHint mirrors a pooled entry's country-block window into the
// hint. A zero until stores nothing: the runs write was a no-op under a
// live ban, and the ban hint already owns the token.
func (p *Pool) storeCountryHint(entry *tokenEntry, until time.Time) {
	if entry == nil || entry.token == "" || until.IsZero() {
		return
	}
	p.storeCooldownHint(poolTokenHash(entry.token), poolCooldownBlob{Kind: cooldownHintKindCountryBlocked, UntilMs: until.UnixMilli()})
}

// countryBlockWindow is the hint window for a walk-observed country block:
// the configured country-block cooldown, 15m when unset (mirrors the runs
// default). The hint only needs to cover the live window approximately —
// expiry just re-probes live.
func (p *Pool) countryBlockWindow() time.Duration {
	if cfg := p.cfg.Load(); cfg != nil && cfg.CooldownCountryBlock > 0 {
		return cfg.CooldownCountryBlock
	}
	return 15 * time.Minute
}

// cooldownHintSkippable reports whether fresh hints may skip doomed probes
// on this walk: true unless every ordered token carries a fresh hint — then
// hints are ignored and the walk attempts upstream live, so a hint alone
// never fails Acquire.
func (p *Pool) cooldownHintSkippable(toks *[]*tokenEntry, order []int, now time.Time) bool {
	saw := false
	for _, idx := range order {
		if idx < 0 || idx >= len(*toks) || (*toks)[idx] == nil {
			continue
		}
		saw = true
		if !p.cooldownHintFresh(poolTokenHash((*toks)[idx].token), now) {
			return true
		}
	}
	return !saw
}

// parseCooldownHint validates one stored hint blob: unknown kinds and
// negative timestamps are corrupt (false).
func parseCooldownHint(raw []byte) (poolCooldownBlob, bool) {
	var b poolCooldownBlob
	if err := json.Unmarshal(raw, &b); err != nil {
		return poolCooldownBlob{}, false
	}
	if b.Kind != cooldownHintKindBan && b.Kind != cooldownHintKindCountryBlocked {
		return poolCooldownBlob{}, false
	}
	if b.UntilMs < 0 {
		return poolCooldownBlob{}, false
	}
	return b, true
}

// restoreCooldownHints loads pool/cooldown/* rows into the memory mirror,
// dropping expired/corrupt rows (best-effort delete) so a restart never
// resurrects a lapsed window. Missing rows stay eligible.
func (p *Pool) restoreCooldownHints(st PoolPersist, now time.Time) {
	rows, err := st.ListPoolState(poolCooldownPrefix)
	if err != nil {
		p.logger.Warn("pool: cooldown hint restore failed (starting fresh)", "error", err)
		return
	}
	nowMs := now.UnixMilli()
	p.cooldownHintMu.Lock()
	defer p.cooldownHintMu.Unlock()
	if p.cooldownHints == nil {
		p.cooldownHints = make(map[string]poolCooldownBlob)
	}
	for key, raw := range rows {
		hash := key[len(poolCooldownPrefix):]
		b, ok := parseCooldownHint(raw)
		if !ok || hash == "" {
			_ = st.DeletePoolState(key)
			continue
		}
		if b.UntilMs > 0 && b.UntilMs <= nowMs {
			delete(p.cooldownHints, hash)
			_ = st.DeletePoolState(key)
			continue
		}
		p.cooldownHints[hash] = b
	}
}

// snapshotCooldownHints copies the live hint mirror for the flush, pruning
// expired entries in place. The copy keeps the lock hold short: no store
// I/O happens under it.
func (p *Pool) snapshotCooldownHints(now time.Time) (staged []poolKV, live map[string]bool) {
	live = make(map[string]bool)
	nowMs := now.UnixMilli()
	p.cooldownHintMu.Lock()
	defer p.cooldownHintMu.Unlock()
	for hash, b := range p.cooldownHints {
		if b.UntilMs > 0 && b.UntilMs <= nowMs {
			delete(p.cooldownHints, hash)
			continue
		}
		key := poolCooldownKey(hash)
		live[key] = true
		staged = append(staged, poolKV{key: key, val: mustMarshalPool(b)})
	}
	return staged, live
}
