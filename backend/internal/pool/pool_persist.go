package pool

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"time"
)

// pool_persist.go — pool runtime write-through cache (DB-unified-storage).
//
// The in-memory maps stay the hot path: every mutation only sets a dirty
// flag (markPersistDirty, a lock-free atomic store). A background flush —
// the maintain tick plus a best-effort pass in Shutdown — snapshots the
// allowlisted state and saves it through the PoolPersist interface. The
// pool never imports the store package (archtest leaf rule): the pool
// marshals its own opaque blobs, and the store package implements
// PoolPersist implicitly (no import in either direction). Nil store
// disables persistence (in-memory only). Save errors only warn and
// re-arm the dirty flag for the next pass — a DB failure degrades to
// live-only and never blocks the request hot path.
//
// Persist allowlist (parent-scoped): ledger counters, admissions counts,
// terminal-cooldown hints (ban/country-block only, cooldown_hint.go) and the
// bridge idle-eviction survivor blob. Never persisted: live handles (channels,
// sync.Once, WaitGroup, CancelFunc, atomic.Pointer, Logger, Registry,
// tokenEntry pointers); 429/ip_capped/limited_ip/burst windows (an ip_capped
// row would spread one egress IP's limit to all accounts); the live per-token
// quota cache — pool/probe/quota/* is retired (drained by the flush, never
// written): the sessions_persist row owns quota and the session manager
// restores it, so a second writer only adds dual-write staleness.
//
// Key namespace (stable strings; store never interprets values):
//
//	pool/ledger/<sha256hex(token)>       one AccountLedger blob per token
//	pool/admissions                       in-flight session admissions by model
//	pool/cooldown/<sha256hex(token)>      terminal-cooldown hint {kind,until_ms}
//	pool/bridge/survivors                 bounded bridge idle-eviction survivors
const (
	poolStateAdmissions    = "pool/admissions"
	poolLedgerPrefix       = "pool/ledger/"
	poolCooldownPrefix     = "pool/cooldown/"
	poolBridgeSurvivorsKey = "pool/bridge/survivors"
	// retiredProbeQuotaPrefix is the drained quota-cache namespace (see the
	// allowlist above): the flush deletes these keys and nothing writes them.
	retiredProbeQuotaPrefix = "pool/probe/quota/"
)

// PoolPersist is the persistence backend for pool runtime state. Values
// are opaque blobs the pool marshals itself; Load maps a missing row to
// ok=false (never an error).
type PoolPersist interface {
	SavePoolState(key string, value []byte) error
	LoadPoolState(key string) (value []byte, ok bool, err error)
	DeletePoolState(key string) error
	ListPoolState(prefix string) (map[string][]byte, error)
}

// poolSpendHit is one rolling-24h spend amount at Unix millis UTC.
type poolSpendHit struct {
	At     int64 `json:"at"`
	Tokens int64 `json:"tokens"`
}

// poolSpendBlob is the JSON-stable mirror of spendLedger. Field names stay
// snake_case and additive — old rows must still unmarshal after new
// counters land.
type poolSpendBlob struct {
	Rolling      []poolSpendHit `json:"rolling"`
	DayUsed      int64          `json:"day_used"`
	DayStart     int64          `json:"day_start"`
	WeekUsed     int64          `json:"week_used"`
	WeekStart    int64          `json:"week_start"`
	MonthUsed    int64          `json:"month_used"`
	MonthStart   int64          `json:"month_start"`
	SpendLimited int            `json:"spend_limited"`
}

// poolLedgerBlob is the JSON-stable mirror of AccountLedger. Timestamps
// are Unix millis UTC.
type poolLedgerBlob struct {
	Usage       []int64       `json:"usage"`
	Spend       poolSpendBlob `json:"spend"`
	ReqDayStart int64         `json:"req_day_start"`
	ReqDayCount int64         `json:"req_day_count"`
}

// poolTokenHash keys a token's per-token rows by the SHA-256 hex of the
// token value: raw tokens never cross the persistence boundary.
func poolTokenHash(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

// poolLedgerKey returns the pool_state key for one token's ledger blob.
func poolLedgerKey(tokenHash string) string { return poolLedgerPrefix + tokenHash }

// SetPoolPersist wires the runtime persistence backend (nil disables).
// Start restores the persisted state automatically after wiring, so
// counters, terminal-cooldown hints and bridge survivors survive restarts;
// direct RestorePoolPersist calls remain for tests and pre-Start restores.
func (p *Pool) SetPoolPersist(s PoolPersist) {
	p.persistMu.Lock()
	defer p.persistMu.Unlock()
	p.persist = s
}

// markPersistDirty arms the background flush. Lock-free so the request hot
// path never blocks on persistence.
func (p *Pool) markPersistDirty() { p.persistDirty.Store(true) }

// poolKV is one key/blob pair staged for save.
type poolKV struct {
	key string
	val []byte
}

func mustMarshalPool(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		// Pool blobs are plain ints/strings/slices: unmarshalable only on
		// programmer error. Fall back to an empty object so one bad shape
		// cannot wedge the whole flush (restore skips it as corrupt).
		return []byte(`{}`)
	}
	return raw
}

// FlushPoolPersist snapshots the allowlisted state and saves it. Locks are
// never held across store I/O: each subsystem is copied under its own
// lock, marshalled, then written. Errors re-arm the dirty flag for the
// next pass and are returned for tests; background callers (maintain tick,
// Shutdown) log-and-continue so a DB failure degrades to live-only.
func (p *Pool) FlushPoolPersist() error {
	p.persistMu.Lock()
	st := p.persist
	p.persistMu.Unlock()
	if st == nil || !p.persistDirty.Swap(false) {
		return nil
	}
	staged, liveLedgers, liveCooldowns := p.snapshotPoolState()
	for _, kv := range staged {
		if err := st.SavePoolState(kv.key, kv.val); err != nil {
			p.persistDirty.Store(true)
			p.logger.Warn("pool: runtime persist flush failed (live-only until next pass)", "key", kv.key, "error", err)
			return err
		}
	}
	// Best-effort orphan prune: per-token rows whose token left the roster
	// (config reload, RemoveLastToken) must not accumulate across
	// restarts. Failure is not fatal — the next pass retries.
	if rows, err := st.ListPoolState(poolLedgerPrefix); err == nil {
		for key := range rows {
			if !liveLedgers[key] {
				_ = st.DeletePoolState(key)
			}
		}
	}
	if rows, err := st.ListPoolState(poolCooldownPrefix); err == nil {
		for key := range rows {
			if !liveCooldowns[key] {
				_ = st.DeletePoolState(key)
			}
		}
	}
	// Retired namespace drain: pool/probe/quota/* lost its writer to the
	// sessions_persist single-writer cutover — delete whatever older builds
	// left behind. Failure is not fatal; the next pass retries.
	if rows, err := st.ListPoolState(retiredProbeQuotaPrefix); err == nil {
		for key := range rows {
			_ = st.DeletePoolState(key)
		}
	}
	return nil
}

// snapshotPoolState copies the allowlisted state under each subsystem's
// own lock and marshals it. No store I/O happens here. It also returns
// the live per-token key sets for orphan pruning.
func (p *Pool) snapshotPoolState() (staged []poolKV, liveLedgers, liveCooldowns map[string]bool) {
	liveLedgers = make(map[string]bool)

	// Per-token ledgers (roster lock; entry pointers stay in memory —
	// only the counters cross into blobs). The roster mutex is held for the
	// typed slice clone only; the JSON encode runs after it is released
	// (the request completion path takes this same mutex, and the blob
	// grows with the 24h chat count — issue #656).
	p.roster.mu.Lock()
	captures := make([]ledgerCapture, 0, len(*p.roster.toks.Load()))
	keys := make([]string, 0, len(*p.roster.toks.Load()))
	for _, entry := range *p.roster.toks.Load() {
		if entry == nil || entry.ledger == nil {
			continue
		}
		key := poolLedgerKey(poolTokenHash(entry.token))
		captures = append(captures, captureLedger(entry.ledger))
		keys = append(keys, key)
		liveLedgers[key] = true
	}
	p.roster.mu.Unlock()
	ledgers := make([]poolKV, 0, len(captures))
	for i, cap := range captures {
		ledgers = append(ledgers, poolKV{key: keys[i], val: mustMarshalPool(marshalLedgerCapture(cap))})
	}
	staged = append(staged, ledgers...)

	// Terminal-cooldown hints (memory mirror; expired pruned here).
	var hintStaged []poolKV
	hintStaged, liveCooldowns = p.snapshotCooldownHints(time.Now())
	staged = append(staged, hintStaged...)

	// Bridge idle-eviction survivors (single blob).
	staged = append(staged, p.snapshotBridgeSurvivors()...)

	// Admissions (transient in-flight counts; restored as-is, self-heals
	// on the next admission cycle).
	p.admissionsMu.Lock()
	adm := make(map[string]int, len(p.admissions))
	for m, idx := range p.admissions {
		adm[m] = idx
	}
	p.admissionsMu.Unlock()
	staged = append(staged, poolKV{key: poolStateAdmissions, val: mustMarshalPool(adm)})

	return staged, liveLedgers, liveCooldowns
}

// ledgerCapture is the lock-free copy of one ledger's persisted counters,
// taken while the roster (or bridge) mutex is held. marshalLedgerCapture
// turns it into the JSON blob after the lock is released.
type ledgerCapture struct {
	usage       []time.Time
	reqDayStart int64
	reqDayCount int64
	spend       *spendCapture
}

// spendCapture mirrors spendLedger's persisted fields.
type spendCapture struct {
	rolling      []spendEntry
	dayUsed      int64
	dayStart     int64
	weekUsed     int64
	weekStart    int64
	monthUsed    int64
	monthStart   int64
	spendLimited int
}

// captureLedger clones one ledger's persisted state. Caller holds the
// roster (or bridge) mutex; slices are cloned (typed memcpy) so the JSON
// encode and its per-element allocations run outside the lock.
func captureLedger(l *AccountLedger) ledgerCapture {
	c := ledgerCapture{
		usage:       slices.Clone(l.usage),
		reqDayStart: l.reqDayStart,
		reqDayCount: l.reqDayCount,
	}
	if l.spend != nil {
		c.spend = &spendCapture{
			rolling:      slices.Clone(l.spend.rolling),
			dayUsed:      l.spend.dayUsed,
			dayStart:     l.spend.dayStart,
			weekUsed:     l.spend.weekUsed,
			weekStart:    l.spend.weekStart,
			monthUsed:    l.spend.monthUsed,
			monthStart:   l.spend.monthStart,
			spendLimited: l.spend.spendLimited,
		}
	}
	return c
}

// marshalLedgerCapture builds one ledger's blob form from its capture.
// No lock is required; call it after releasing the roster (or bridge) mutex.
func marshalLedgerCapture(c ledgerCapture) poolLedgerBlob {
	blob := poolLedgerBlob{
		ReqDayStart: c.reqDayStart,
		ReqDayCount: c.reqDayCount,
	}
	for _, t := range c.usage {
		blob.Usage = append(blob.Usage, t.UnixMilli())
	}
	if c.spend != nil {
		sp := poolSpendBlob{
			DayUsed:      c.spend.dayUsed,
			DayStart:     c.spend.dayStart,
			WeekUsed:     c.spend.weekUsed,
			WeekStart:    c.spend.weekStart,
			MonthUsed:    c.spend.monthUsed,
			MonthStart:   c.spend.monthStart,
			SpendLimited: c.spend.spendLimited,
		}
		for _, e := range c.spend.rolling {
			sp.Rolling = append(sp.Rolling, poolSpendHit{At: e.at.UnixMilli(), Tokens: e.tokens})
		}
		blob.Spend = sp
	}
	return blob
}

// RestorePoolPersist loads persisted runtime state into the live maps.
// Start calls it automatically after the owner wires the store with
// SetPoolPersist; direct calls remain for tests and pre-Start restores.
// Missing rows stay zero-valued (a fresh boot behaves exactly as before);
// corrupt rows warn and are skipped — restore never fails the boot.
// TTL/expiry is enforced on the way in: out-of-window usage timestamps are
// dropped and stale spend buckets roll, so a restart never resurrects
// expired windows. Terminal-cooldown hints and bridge survivors restore the
// same way (hints only, expiry-checked, never authoritative); the session
// quota cache warms through the sessions_persist row instead (single
// writer — see the allowlist above).
func (p *Pool) RestorePoolPersist() {
	p.persistMu.Lock()
	st := p.persist
	p.persistMu.Unlock()
	if st == nil {
		return
	}
	now := time.Now()
	p.restoreLedgers(st, now)
	p.restoreAdmissions(st)
	p.restoreCooldownHints(st, now)
	p.restoreBridgeSurvivors(st, now)
}

func (p *Pool) restoreLedgers(st PoolPersist, now time.Time) {
	// Index the live roster by ledger key first (no store I/O under the
	// roster lock).
	p.roster.mu.Lock()
	byKey := make(map[string]*AccountLedger)
	for _, entry := range *p.roster.toks.Load() {
		if entry == nil {
			continue
		}
		if entry.ledger == nil {
			entry.ledger = newAccountLedger()
		}
		byKey[poolLedgerKey(poolTokenHash(entry.token))] = entry.ledger
	}
	p.roster.mu.Unlock()

	for key, ledger := range byKey {
		raw, ok, err := st.LoadPoolState(key)
		if err != nil {
			p.logger.Warn("pool: runtime persist restore failed (starting fresh)", "key", key, "error", err)
			continue
		}
		if !ok {
			continue
		}
		var blob poolLedgerBlob
		if err := json.Unmarshal(raw, &blob); err != nil {
			p.logger.Warn("pool: runtime persist row corrupt (starting fresh)", "key", key, "error", err)
			continue
		}
		p.roster.mu.Lock()
		installLedger(ledger, blob, now)
		p.roster.mu.Unlock()
	}
}

// installLedger replaces a ledger's counters from its blob, dropping
// expired window timestamps and rolling stale spend buckets. Caller holds
// the roster (or bridge) mutex.
func installLedger(l *AccountLedger, blob poolLedgerBlob, now time.Time) {
	usageCutoff := now.Add(-usageWindow)
	l.usage = l.usage[:0]
	for _, ms := range blob.Usage {
		if t := time.UnixMilli(ms); !t.Before(usageCutoff) {
			l.usage = append(l.usage, t)
		}
	}
	// Pacific-day bucket: dayRequestCount rolls a stale bucket on read as a
	// side effect, so discard the return and keep the normalized state.
	l.reqDayStart, l.reqDayCount = blob.ReqDayStart, blob.ReqDayCount
	_ = l.dayRequestCount(now)
	if l.spend == nil {
		l.spend = newSpendLedger()
	}
	sp := l.spend
	sp.rolling = sp.rolling[:0]
	for _, e := range blob.Spend.Rolling {
		if t := time.UnixMilli(e.At); !t.Before(usageCutoff) && e.Tokens > 0 {
			sp.rolling = append(sp.rolling, spendEntry{at: t, tokens: e.Tokens})
		}
	}
	// Recompute the incremental rolling total from the restored rows
	// (invariant: rollingTotal == sum of rolling[].tokens).
	sp.rollingTotal = 0
	for _, e := range sp.rolling {
		sp.rollingTotal += e.tokens
	}
	sp.dayUsed, sp.dayStart = rollSpendBucket(blob.Spend.DayUsed, blob.Spend.DayStart, "day", now)
	sp.weekUsed, sp.weekStart = rollSpendBucket(blob.Spend.WeekUsed, blob.Spend.WeekStart, "week", now)
	sp.monthUsed, sp.monthStart = rollSpendBucket(blob.Spend.MonthUsed, blob.Spend.MonthStart, "month", now)
	sp.spendLimited = blob.Spend.SpendLimited
}

// rollSpendBucket replays one spend period bucket at restore: a rolled-over
// window resets to the current bucket instead of resurrecting a stale sum.
func rollSpendBucket(used, start int64, period string, now time.Time) (int64, int64) {
	if needsRollover(start, period, now) {
		return 0, bucketStart(now, period)
	}
	return used, start
}

func (p *Pool) restoreAdmissions(st PoolPersist) {
	raw, ok, err := st.LoadPoolState(poolStateAdmissions)
	if err != nil {
		p.logger.Warn("pool: runtime persist restore failed (starting fresh)", "key", poolStateAdmissions, "error", err)
		return
	}
	if !ok {
		return
	}
	var adm map[string]int
	if err := json.Unmarshal(raw, &adm); err != nil {
		p.logger.Warn("pool: runtime persist row corrupt (starting fresh)", "key", poolStateAdmissions, "error", err)
		return
	}
	p.admissionsMu.Lock()
	defer p.admissionsMu.Unlock()
	if p.admissions == nil {
		p.admissions = make(map[string]int)
	}
	for m, idx := range adm {
		p.admissions[m] = idx
	}
}
