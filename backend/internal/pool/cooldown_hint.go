package pool

import (
	"encoding/json"
	"time"
)

// cooldown_hint.go — timestamped DB-hints for terminal cooldowns + bridge
// idle-eviction survivors (data-architecture "Changes proposed" §2).
//
// Both are hints, never authoritative: readers treat a missing row as
// eligible and an expired row as absent, the request hot path reads memory
// only (never the store), and deleting the keys restores exactly today's
// behavior (mem-only). Raw tokens/keys never cross the boundary: every key
// is a SHA-256 hex (poolTokenHash) or the bridge map key (tokenKey).

const (
	// Terminal hint kinds — the only kinds ever stored. 429s (including
	// spend_limited / freebucks windows), ip_capped, limited_ip and burst
	// timers are NEVER persisted: ip_capped is a per-IP wall and persisting
	// it per-token spreads one IP's limit to all accounts.
	cooldownHintKindBan            = "banned"
	cooldownHintKindCountryBlocked = "country_blocked"

	// maxBridgeSurvivors caps the survivor list: captures past the cap drop
	// the oldest, so the blob stays small and recent.
	maxBridgeSurvivors = 64
)

// poolCooldownBlob is one token's terminal-cooldown hint. UntilMs is Unix
// millis UTC; 0 means a permanent terminal state (hard ban, no resumes_at)
// that only an operator action (UnlockToken) or a key delete lifts.
type poolCooldownBlob struct {
	Kind    string `json:"kind"`
	UntilMs int64  `json:"until_ms"`
}

// bridgeSurvivor is one idle-evicted bridge entry's usage contribution. Key
// is the bridge map key (tokenKey: SHA-256 hex truncated to 32 chars),
// never the raw client token. AtMs bounds the record's life: it expires one
// usageWindow after eviction. Chats/Day carry the evicted entry's in-window
// counters so a restart does not reset its contribution to the bridge usage
// accounting; DayStart (Pacific-day bucket start, unix seconds) scopes Day
// to the day it was counted on.
type bridgeSurvivor struct {
	Key      string `json:"key"`
	AtMs     int64  `json:"at_ms"`
	Chats    int    `json:"chats"`
	Day      int    `json:"day"`
	DayStart int64  `json:"day_start"`
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

// storeCooldownHint records blob for tokenHash in memory and best-effort to
// the store (nil store = memory only). Unknown kinds are refused.
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
	if st := p.poolPersistBackend(); st != nil {
		if err := st.SavePoolState(poolCooldownKey(tokenHash), mustMarshalPool(blob)); err != nil {
			p.logger.Debug("pool: cooldown hint write failed (memory hint kept)", "error", err)
		}
	}
}

// clearCooldownHint drops tokenHash's hint from memory and best-effort from
// the store. Memory pops first so the common case (no hint) does zero I/O.
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
	if st := p.poolPersistBackend(); st != nil {
		_ = st.DeletePoolState(poolCooldownKey(tokenHash))
	}
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

// captureBridgeSurvivor appends one idle-evicted entry's usage contribution.
// The entry is already unlinked from the cache (no new traffic can target
// it) and carries no in-flight lease, so its ledger is stable without
// bridgeMu. Entries with no in-window usage record nothing (the eviction
// itself still proceeds). Raw tokens never cross: the record is keyed by
// the entry's SHA map key.
func (p *Pool) captureBridgeSurvivor(mapKey string, entry *bridgeEntry, now time.Time) {
	if mapKey == "" || entry == nil || entry.ledger == nil {
		return
	}
	cutoff := now.Add(-usageWindow)
	chats := 0
	for _, t := range entry.ledger.usage {
		if !t.Before(cutoff) {
			chats++
		}
	}
	day := 0
	var dayStart int64
	if entry.ledger.reqDayCount > 0 {
		if start := bucketStart(now, "day"); start == entry.ledger.reqDayStart {
			day = int(entry.ledger.reqDayCount)
			dayStart = start
		}
	}
	if chats == 0 && day == 0 {
		return
	}
	rec := bridgeSurvivor{Key: mapKey, AtMs: now.UnixMilli(), Chats: chats, Day: day, DayStart: dayStart}
	p.bridgeSurvivorMu.Lock()
	p.bridgeSurvivors = append(p.bridgeSurvivors, rec)
	if len(p.bridgeSurvivors) > maxBridgeSurvivors {
		p.bridgeSurvivors = append([]bridgeSurvivor(nil), p.bridgeSurvivors[len(p.bridgeSurvivors)-maxBridgeSurvivors:]...)
	}
	p.bridgeSurvivorMu.Unlock()
	p.markPersistDirty()
	if st := p.poolPersistBackend(); st != nil {
		// Best-effort immediate write: an eviction between flushes must
		// not lose the contribution. The flush stages the same blob.
		_ = st.SavePoolState(poolBridgeSurvivorsKey, p.marshalBridgeSurvivors())
	}
}

// marshalBridgeSurvivors renders the survivor blob (oldest first).
func (p *Pool) marshalBridgeSurvivors() []byte {
	p.bridgeSurvivorMu.Lock()
	defer p.bridgeSurvivorMu.Unlock()
	out := p.bridgeSurvivors
	if out == nil {
		out = []bridgeSurvivor{}
	}
	return mustMarshalPool(out)
}

// snapshotBridgeSurvivors stages the survivor blob for the flush (nil when
// the list is empty: nothing to write).
func (p *Pool) snapshotBridgeSurvivors() []poolKV {
	p.bridgeSurvivorMu.Lock()
	defer p.bridgeSurvivorMu.Unlock()
	if len(p.bridgeSurvivors) == 0 {
		return nil
	}
	cp := make([]bridgeSurvivor, len(p.bridgeSurvivors))
	copy(cp, p.bridgeSurvivors)
	return []poolKV{{key: poolBridgeSurvivorsKey, val: mustMarshalPool(cp)}}
}

// restoreBridgeSurvivors loads the survivor blob into memory, dropping
// records evicted over one usageWindow ago and enforcing the cap (newest
// win). It converges the row when anything was dropped, so one boot
// settles the namespace.
func (p *Pool) restoreBridgeSurvivors(st PoolPersist, now time.Time) {
	raw, ok, err := st.LoadPoolState(poolBridgeSurvivorsKey)
	if err != nil {
		p.logger.Warn("pool: bridge survivor restore failed (starting fresh)", "error", err)
		return
	}
	if !ok {
		return
	}
	var list []bridgeSurvivor
	if err := json.Unmarshal(raw, &list); err != nil {
		p.logger.Warn("pool: bridge survivor row corrupt (starting fresh)", "error", err)
		_ = st.DeletePoolState(poolBridgeSurvivorsKey)
		return
	}
	cutoff := now.Add(-usageWindow)
	kept := make([]bridgeSurvivor, 0, len(list))
	for _, r := range list {
		if r.Key == "" || time.UnixMilli(r.AtMs).Before(cutoff) {
			continue
		}
		kept = append(kept, r)
	}
	if len(kept) > maxBridgeSurvivors {
		kept = kept[len(kept)-maxBridgeSurvivors:]
	}
	p.bridgeSurvivorMu.Lock()
	p.bridgeSurvivors = kept
	p.bridgeSurvivorMu.Unlock()
	if len(kept) != len(list) {
		if len(kept) == 0 {
			_ = st.DeletePoolState(poolBridgeSurvivorsKey)
		} else {
			_ = st.SavePoolState(poolBridgeSurvivorsKey, mustMarshalPool(kept))
		}
	}
}

// bridgeSurvivorUsage folds survivors into the bridge usage accounting as of
// now: in-window Chats plus today's Day counts. Display only — no local cap
// reads it (upstream quota/429 is the enforcement).
func (p *Pool) bridgeSurvivorUsage(now time.Time) (chats24h, pacificDay int) {
	cutoff := now.Add(-usageWindow)
	today := bucketStart(now, "day")
	p.bridgeSurvivorMu.Lock()
	defer p.bridgeSurvivorMu.Unlock()
	for _, r := range p.bridgeSurvivors {
		if time.UnixMilli(r.AtMs).Before(cutoff) {
			continue
		}
		chats24h += r.Chats
		if r.DayStart == today && r.DayStart != 0 {
			pacificDay += r.Day
		}
	}
	return chats24h, pacificDay
}

// BridgeSurvivorUsage is the exported survivor-accounting view (dashboard,
// tests): surviving in-window usage of idle-evicted bridge entries.
func (p *Pool) BridgeSurvivorUsage() (chats24h, pacificDay int) {
	return p.bridgeSurvivorUsage(time.Now())
}
