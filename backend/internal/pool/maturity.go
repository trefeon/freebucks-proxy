// maturity.go — nightly streak-maintenance automation (one global run in
// the 15 minutes before the Pacific-midnight reset, replacing the old
// per-token all-day slots).
//
// The window splits in two: T-15m→T-5m is pre-flight (streak refresh,
// advance accounting, and skip classification run, but nothing fires —
// fire-ready tokens ledger skip:preflight), and only the final
// maturityFireGate before the slot end buffer (T-5m→T-1m) fires touches,
// oldest-streak-first. A token still eligible when the gate closes
// ledgers skip:window-exhausted instead of silently dropping.
//
// Universal automatic: every account is enrolled, gated only by the global
// MATURITY_ENABLED kill-switch. There is no per-account enrollment — the
// stored per-token enabled flag is dead input (kept for API compat,
// ignored by the run). Each account gets one daily low-cost touch inside
// the firing gate unless client traffic already used the account
// today. No lock transitions anywhere: an operator-manually-locked token
// is skipped by the run and stays locked.
//
// Safety posture: global kill-switch (MATURITY_ENABLED, default on with
// live touches), unmetered touch models only (never burns premium
// quota — the fire path fails closed on priced rows), per-token slots
// staggered with jitter inside the 15m window, restart-safe idempotency
// (touchDay/slotDay plus the upstream todayUsed flag), and a 429
// abort+backoff that pauses the walk instead of hammering. A fire-path
// refusal (a config or meter fact, e.g. no unmetered row to admit) records
// its history event and log once per night instead of once per 60s tick.
// The run rides the 60s maintainTick pass — no new goroutine — and never
// touches quarantined, banned, cooling, country-blocked, or locked accounts.
// Window math is America/Los_Angeles wall-clock (never a fixed offset), so
// the run tracks Pacific midnight across DST.
package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"freebuff-proxy/backend/internal/modelcat"
	"freebuff-proxy/backend/internal/upstream"
)

// Maturity touch modes.
const (
	// MaturityModeUnmetered admits the configured unmetered model
	// (MATURITY_TOUCH_MODEL): reservation on an unpriced row, free,
	// plus a live session real traffic can reuse.
	MaturityModeUnmetered = "unmetered"
	// MaturityModePremiumShort admits one short session on a premium row
	// instead. It is billed in Freebucks and stays opt-in per token.
	MaturityModePremiumShort = "premium-short"
)

const (
	// maturityThrottle is the restart-safe minimum gap between two touches
	// on one token: a restart re-rolls the day's slot, and touchDay plus
	// the upstream todayUsed flag bound the worst case to one extra cheap
	// touch.
	maturityThrottle = 6 * time.Hour
	// maturityStreakFresh bounds streak-cache age for touch decisions: the
	// number moves daily, and a touch must never fire blind off stale data.
	maturityStreakFresh = time.Hour
	// maturityRunWindow is the fixed nightly maintenance window: the 15
	// minutes before the Pacific-midnight reset (23:45–00:00). One
	// collapsed pre-reset window for every account (not per-token all-day
	// slots), so touches land right before upstream rolls the daily
	// streak. Tight by design: a 60m window sprayed early-evening slots
	// that rescued nothing; 15m keeps every slot inside the expiring
	// day's final stretch while still fitting 3 sequential touches plus
	// a backoff retry.
	maturityRunWindow = 15 * time.Minute
	// maturityFireGate is the firing tail of the nightly window: touches
	// fire only in the last 5 minutes before the slot end buffer
	// (T-5m→T-1m). The window head (T-15m→T-5m) is pre-flight —
	// classify only, never fire — so every slot lands inside the
	// expiring day's final stretch while firing stays clear of the
	// reset.
	maturityFireGate = 5 * time.Minute
	// maturitySlotEndBuffer reserves the final stretch before the reset
	// from slot starts: a touch carries a 30s upstream timeout, so a
	// slot opening with less than a minute to midnight could bleed
	// past the reset and credit the wrong day.
	maturitySlotEndBuffer = time.Minute
)

// maturity429Backoff pauses the nightly walk after a rate-limited
// touch: the walk aborts and no further touch fires until this long
// after the 429, instead of hammering a throttled upstream. Kept
// well under the window so one 429 still leaves retry room the
// same night. Tunable via MATURITY_BACKOFF_MS (live-applied from
// pool.SetConfig); the default preserves the 3m pause.
var maturity429Backoff = 3 * time.Minute

// MaturitySnapshot is the dashboard-ready per-token maturity view. Nil on
// TokenSnapshot until maturity is first enabled for the token, so tokens
// that never opt in carry no new payload.
type MaturitySnapshot struct {
	Enabled bool   `json:"enabled"`
	Target  int    `json:"target"`
	Mode    string `json:"mode"`
	// TouchModel is the per-token touch-model override ("" = automatic:
	// the cheapest served unmetered row, falling back to the global
	// MATURITY_TOUCH_MODEL when no unmetered served row exists).
	// Omitted on the wire when unset so never-enrolled tokens keep
	// their existing payload shape.
	TouchModel string    `json:"touch_model,omitempty"`
	Badge      string    `json:"badge"`
	Slot       time.Time `json:"slot,omitempty"`
	// SlotDay is the account-timezone calendar day the Slot belongs to
	// ("2006-01-02"): the dashboard derives the next-touch countdown
	// and the done/pending day-strip from Slot/SlotDay/LastTouch
	// without a new scheduler.
	SlotDay   string    `json:"slot_day,omitempty"`
	LastTouch time.Time `json:"last_touch,omitempty"`
	// TouchDay is the account-timezone calendar day of the last touch
	// ("2006-01-02"): TouchDay == SlotDay means touched today.
	TouchDay     string `json:"touch_day,omitempty"`
	LastAction   string `json:"last_action,omitempty"`
	LastResult   string `json:"last_result,omitempty"`
	LastAdvanced string `json:"last_advanced,omitempty"`
	// ResultDay is the Pacific calendar day ("2006-01-02") the last
	// ledger write belongs to: every maturityRecord stamps it, so the
	// dashboard can scope per-account Skipped rows to tonight's run
	// while the last-run ledger stays historical.
	ResultDay string `json:"result_day,omitempty"`
	// EffectiveTouchModel is the model the next touch will actually
	// admit (manual override, premium-short pool head, auto pick, or
	// explicit global fallback, in that precedence).
	EffectiveTouchModel string `json:"effective_touch_model,omitempty"`
	// AutoTouchModel is the automatic pick (cheapest served unmetered
	// row) with AutoTouchReason naming why ("auto:unmetered" or
	// "fallback:no-unmetered-served"). Shown by the UI next to the
	// manual dropdown so the Auto default is inspectable.
	AutoTouchModel  string `json:"auto_touch_model,omitempty"`
	AutoTouchReason string `json:"auto_touch_reason,omitempty"`
}

// maturityState is the mutable per-token automation state, guarded by
// tokenEntry.maturityMu. Zero value = disabled.
type maturityState struct {
	enabled bool
	target  int
	mode    string
	// touchModel overrides the global MATURITY_TOUCH_MODEL for this
	// token only. Empty means "use the global fallback".
	touchModel   string
	slot         time.Time
	slotDay      string
	lastTouch    time.Time
	lastAction   string
	lastResult   string
	lastAdvanced string
	// resultDay is the Pacific day of the last ledger write (see
	// ResultDay on MaturitySnapshot). Empty on pre-upgrade rows.
	resultDay     string
	lastStreak    int
	streakAtTouch int
	touchDay      string
	// refusalDay/refusalKey bound the fire-path refusal record: the
	// Pacific day and the exact refusal content already emitted, so a
	// config- or meter-level refusal that cannot change inside the night
	// lands once per night instead of once per 60s maintain tick.
	refusalDay string
	refusalKey string
}

// maturityPersisted is the JSON-stable mirror of maturityState for the
// maturity_json blob (DB column, not wire: field names stay snake_case and
// additive — old rows must still unmarshal after new counters land).
type maturityPersisted struct {
	Enabled       bool      `json:"enabled"`
	Target        int       `json:"target"`
	Mode          string    `json:"mode"`
	TouchModel    string    `json:"touch_model,omitempty"`
	Slot          time.Time `json:"slot,omitempty"`
	SlotDay       string    `json:"slot_day,omitempty"`
	LastTouch     time.Time `json:"last_touch,omitempty"`
	LastAction    string    `json:"last_action,omitempty"`
	LastResult    string    `json:"last_result,omitempty"`
	LastAdvanced  string    `json:"last_advanced,omitempty"`
	ResultDay     string    `json:"result_day,omitempty"`
	LastStreak    int       `json:"last_streak,omitempty"`
	StreakAtTouch int       `json:"streak_at_touch,omitempty"`
	TouchDay      string    `json:"touch_day,omitempty"`
	RefusalDay    string    `json:"refusal_day,omitempty"`
	RefusalKey    string    `json:"refusal_key,omitempty"`
}

func (m maturityState) marshalMaturity() (string, error) {
	raw, err := json.Marshal(maturityPersisted{
		Enabled:       m.enabled,
		Target:        m.target,
		Mode:          m.mode,
		TouchModel:    m.touchModel,
		Slot:          m.slot,
		SlotDay:       m.slotDay,
		LastTouch:     m.lastTouch,
		LastAction:    m.lastAction,
		LastResult:    m.lastResult,
		LastAdvanced:  m.lastAdvanced,
		ResultDay:     m.resultDay,
		LastStreak:    m.lastStreak,
		StreakAtTouch: m.streakAtTouch,
		TouchDay:      m.touchDay,
		RefusalDay:    m.refusalDay,
		RefusalKey:    m.refusalKey,
	})
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func unmarshalMaturity(raw string) (maturityState, error) {
	var stored maturityPersisted
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return maturityState{}, err
	}
	return maturityState{
		enabled:       stored.Enabled,
		target:        stored.Target,
		mode:          stored.Mode,
		touchModel:    stored.TouchModel,
		slot:          stored.Slot,
		slotDay:       stored.SlotDay,
		lastTouch:     stored.LastTouch,
		lastAction:    stored.LastAction,
		lastResult:    stored.LastResult,
		lastAdvanced:  stored.LastAdvanced,
		resultDay:     stored.ResultDay,
		lastStreak:    stored.LastStreak,
		streakAtTouch: stored.StreakAtTouch,
		touchDay:      stored.TouchDay,
		refusalDay:    stored.RefusalDay,
		refusalKey:    stored.RefusalKey,
	}, nil
}

// SetMaturity stores per-token streak-maintenance preferences (compat API).
// The run is universal automatic: the enabled flag is stored and served
// but ignored by eligibility, which keys only on the global switch plus
// the health gates. mode/touchModel still resolve per token; target is
// stored but unused (MATURITY_TARGET_DAYS is hidden/deprecated).
// mode "" means unmetered.
// mode premium-short spends from the account's metered pool and stays opt-in
// per token.
// touchModel is the per-token touch-model override; "" (or "auto") selects
// the automatic default (cheapest served unmetered row, falling back to the
// global MATURITY_TOUCH_MODEL when no unmetered served row exists). Any
// other non-empty value must be a provider/model id (shape only —
// served/unmetered semantics stay in the fire path, which fails closed on
// misconfigured models). The override is stored on disable too, so
// re-enabling restores it.
func (p *Pool) SetMaturity(token int, enabled bool, target int, mode string, touchModel string) error {
	toks := p.roster.Load()
	if toks == nil || token < 0 || token >= len(*toks) {
		return fmt.Errorf("pool: token %d out of range", token)
	}
	if mode == "" {
		mode = MaturityModeUnmetered
	}
	if mode != MaturityModeUnmetered && mode != MaturityModePremiumShort {
		return fmt.Errorf("pool: unknown maturity mode %q (want %q or %q)", mode, MaturityModeUnmetered, MaturityModePremiumShort)
	}
	touchModel = strings.TrimSpace(touchModel)
	if modelcat.IsAutoTouchSentinel(touchModel) {
		touchModel = ""
	}
	if touchModel != "" && !strings.Contains(touchModel, "/") {
		return fmt.Errorf("pool: maturity touch model %q must be a provider/model id (e.g. upstage/solar-pro4)", touchModel)
	}
	if target < 0 || target > 28 {
		return fmt.Errorf("pool: maturity target %d out of range (want 0..28, 0 = global MATURITY_TARGET_DAYS default)", target)
	}
	if enabled && target <= 0 {
		target = p.maturityDefaultTarget()
	}
	tok := (*toks)[token]
	tok.maturityMu.Lock()
	tok.maturity.enabled = enabled
	tok.maturity.touchModel = touchModel
	if enabled {
		tok.maturity.target = target
		tok.maturity.mode = mode
		if tok.maturity.slot.IsZero() {
			now := time.Now()
			tok.maturity.slot, tok.maturity.slotDay = rollMaturitySlotInWindow(pacificDayKey(now), now)
		}
	}
	tok.maturityMu.Unlock()
	p.saveMaturity(token, tok)
	if enabled {
		detail := fmt.Sprintf("enabled target=%d mode=%s", target, mode)
		if touchModel != "" {
			detail += " touch=" + touchModel
		}
		p.emitMaturity(token, "config", detail)
	} else {
		p.emitMaturity(token, "config", "disabled")
	}
	return nil
}

// maturityAutoFor resolves the automatic touch-model pick for one token
// from its live Freebucks meter: the cheapest IsServedModel row with
// price 0 or a quota exemption. Honeypot, god-only, eval, paused, and
// priced rows can never win (the IsServed gate plus the premium and live
// price gates inside modelcat, never a naive price sort). The served set
// only changes on registry sync (wiregen), so the pick is stable across
// touches; per-touch meter movement stays enforced by the fail-closed
// fire path, not by re-sorting here.
func maturityAutoFor(tok *tokenEntry) (string, string) {
	var prices map[string]float64
	exempt := false
	if tok != nil && tok.sessionMgr() != nil {
		if snap := tok.sessionMgr().Snapshot(); snap.Freebucks != nil {
			prices = snap.Freebucks.Prices
			exempt = snap.Freebucks.QuotaExempt
		}
	}
	return modelcat.AutoUnmeteredTouchModel(prices, exempt)
}

// maturityResolveEffective resolves the model the next touch will
// actually admit, plus the auto pick and its reason for display.
// Precedence: manual per-token override, premium-short pool head,
// auto pick when the global is the auto sentinel ("auto"/""), else the
// explicit global fallback. Empty effective means fail closed
// (skip:touch-model) — never an invented model.
func (p *Pool) maturityResolveEffective(st maturityState, tok *tokenEntry, global string) (effective, auto, reason string) {
	auto, reason = maturityAutoFor(tok)
	if st.touchModel != "" {
		return st.touchModel, auto, reason
	}
	if st.mode == MaturityModePremiumShort {
		if premium := modelcat.SharedPremiumModels(); len(premium) > 0 {
			return premium[0], auto, reason
		}
		return "", auto, reason
	}
	if modelcat.IsAutoTouchSentinel(global) {
		return auto, auto, reason
	}
	return global, auto, reason
}

// maturityDefaultTarget resolves the fallback streak target: the configured
// MATURITY_TARGET_DAYS default, 7 when unset (direct Config construction in
// tests bypasses Load).
func (p *Pool) maturityDefaultTarget() int {
	if cfg := p.cfg.Load(); cfg != nil && cfg.MaturityTargetDays > 0 {
		return cfg.MaturityTargetDays
	}
	return 7
}

func (p *Pool) maturityCopy(tok *tokenEntry) maturityState {
	tok.maturityMu.Lock()
	defer tok.maturityMu.Unlock()
	return tok.maturity
}

// maturityTick runs one maturity pass over the fixed tokens. It is called
// from maintainTick (both the active and the idle-stretch paths): idle
// accounts are exactly the ones whose streaks need keeping.
func (p *Pool) maturityTick(ctx context.Context) {
	p.maturityTickAt(ctx, time.Now())
}

// maturityTickAt is maturityTick with the clock injected (fake-clock tests).
// The nightly walk aborts on the first rate-limited touch (429 backoff):
// remaining tokens keep last night's ledger until the backoff lifts.
// The sweep walks oldest-streak-first (ascending cached streak): with a
// short firing gate, the lowest-streak accounts claim firing room first.
func (p *Pool) maturityTickAt(ctx context.Context, now time.Time) {
	cfg := p.cfg.Load()
	if cfg == nil || !cfg.MaturityEnabled {
		return
	}
	if p.maturityBackedOff(now) {
		return
	}
	toks := p.roster.Load()
	if toks == nil {
		return
	}
	for _, i := range maturitySweepOrder(*toks) {
		if p.maturityTickOne(ctx, cfg.MaturityTouchModel, i, (*toks)[i], now) {
			return
		}
	}
}

// maturitySweepOrder returns roster indices in sweep order: ascending
// cached streak (oldest-streak-first), ties broken by roster index for
// determinism. A missing cache reads as 0 — the pass refreshes stale
// readings as it walks, so the key is best-available, never truth.
func maturitySweepOrder(toks []*tokenEntry) []int {
	order := make([]int, len(toks))
	for i := range toks {
		order[i] = i
	}
	streakOf := func(e *tokenEntry) int {
		if s := e.Streak(); s != nil {
			return s.Streak
		}
		return 0
	}
	sort.SliceStable(order, func(a, b int) bool {
		return streakOf(toks[order[a]]) < streakOf(toks[order[b]])
	})
	return order
}

// maturityTickOne evaluates and possibly fires one token's nightly touch.
// It reports whether the touch was rate-limited (429): the nightly walk
// aborts on the first 429 and backs off instead of hammering.
// Firing is fire-gate only (T-5m→T-1m): pre-flight passes run the same
// skip classification but ledger skip:preflight instead of firing, and
// post-gate passes ledger skip:window-exhausted for still-eligible tokens.
func (p *Pool) maturityTickOne(ctx context.Context, touchModel string, idx int, tok *tokenEntry, now time.Time) (rateLimited bool) {
	// Persist-on-exit: every mutation below (skips, touches) lands in the
	// maturity_json blob on the way out. Never-touched tokens no-op
	// inside saveMaturity.
	defer p.saveMaturity(idx, tok)
	// Universal automatic: every account is enrolled. The per-token
	// enabled flag is dead input (kept stored + served for API compat,
	// ignored here); mode + touch-model override still resolve per token.
	st := p.maturityCopy(tok)
	label := tokenEntryLabel(tok)
	inWindow := maturityInWindow(now)
	// today is the Pacific calendar day this pass ledgers under: every
	// skip below stamps it as its result day. It derives from the
	// injected now (fake-clock tests), never wall time.
	today := pacificDayKey(now)

	// Operator lock beats automation: a manually locked token stays out of
	// both serving rotation (acquire_order.go) and the nightly run, and
	// the run never locks or unlocks — the lock survives the pass.
	if tok.locked.Load() {
		if inWindow {
			p.maturityRecord(tok, "", "skip:locked", "", today)
		}
		return false
	}

	// Health gates: quarantined, banned, cooling, or country-blocked
	// accounts are never touched — automation must not poke an account
	// upstream already flagged.
	p.clearLiftedQuarantine(tok)
	if q := tok.quarantine.Load(); q != nil {
		if inWindow {
			p.maturityRecord(tok, "", "skip:quarantined", "", today)
		}
		return false
	}
	rs := tok.runs.Snapshot()
	if rs.BanError != nil && (rs.BannedUntil.IsZero() || now.Before(rs.BannedUntil)) {
		if inWindow {
			p.maturityRecord(tok, "", "skip:banned", "", today)
		}
		return false
	}
	if !rs.CooldownUntil.IsZero() && now.Before(rs.CooldownUntil) {
		if inWindow {
			p.maturityRecord(tok, "", "skip:cooling", "", today)
		}
		return false
	}
	if tok.runs.CountryBlockedError() != nil {
		if inWindow {
			p.maturityRecord(tok, "", "skip:country-blocked", "", today)
		}
		return false
	}

	// Fresh streak or skip: decisions below need truth no older than an
	// hour. backfillLoop usually keeps it fresh; refresh synchronously on
	// the rare stale pass, bounded so one slow account cannot stall the
	// maintain tick for long.
	cached := tok.Streak()
	if cached == nil || now.Sub(cached.UpdatedAt) > maturityStreakFresh {
		var err error
		cached, err = p.maturityRefreshStreak(ctx, tok)
		if err != nil || cached == nil {
			if inWindow {
				p.maturityRecord(tok, "", "skip:streak-stale", "stale", today)
			}
			return false
		}
	}

	// Advance accounting: a touch that moved the streak records it for the
	// ledger (last_advanced + the advance history event).
	p.maturityAccountAdvance(idx, tok, cached)

	// Outside the nightly window: bookkeeping above stays fresh, but the
	// last-run ledger is untouched until tonight.
	if !inWindow {
		return false
	}

	// Client-traffic activity skip: one successful client chat since the
	// last Pacific reset means the account is already alive today — no
	// touch needed. The signal is the local Pacific-day ledger, fed ONLY
	// by the Chat success path: internal probes and warming touches never
	// record here, so automation can never self-skip every account.
	if p.dayRequestCount(idx) > 0 {
		p.maturityRecord(tok, "", "skip:client-active", "", today)
		return false
	}

	// Restart-safe idempotency, local half: this token already fired today
	// (touchDay/slotDay survive restarts in the maturity_json blob), so a
	// reboot inside the window must not double-touch. When the ledger
	// already carries today's fire outcome (ok or error), keep it: the
	// guard holds the re-fire either way, but recording another skip
	// would overwrite the fire with skip:today-used on every later pass
	// in the same window, making a touched account read as skipped.
	tok.maturityMu.Lock()
	touchedToday := tok.maturity.touchDay == today && !tok.maturity.lastTouch.IsZero()
	firedToday := touchedToday && !strings.HasPrefix(tok.maturity.lastResult, "skip:")
	tok.maturityMu.Unlock()
	if firedToday {
		return false
	}
	if touchedToday {
		p.maturityRecord(tok, "", "skip:today-used", "", today)
		return false
	}

	// Restart-safe idempotency, upstream half: any activity today (client
	// traffic or an earlier firing) marks the day used, making the day
	// indistinguishable from human use. Zero extra traffic.
	if cached.TodayUsed {
		tok.maturityMu.Lock()
		tok.maturity.lastStreak = cached.Streak
		tok.maturity.lastAdvanced = "yes"
		if tok.maturity.lastResult == "" || strings.HasPrefix(tok.maturity.lastResult, "skip:") {
			tok.maturity.lastResult = "skip:today-used"
			tok.maturity.resultDay = today
		}
		tok.maturityMu.Unlock()
		return false
	}

	// Nightly slot inside the pre-reset window, staggered with jitter and
	// re-rolled every Pacific day (restart-safe via touchDay/todayUsed +
	// the 6h throttle: a re-roll can only cause one extra cheap touch).
	tok.maturityMu.Lock()
	if tok.maturity.slot.IsZero() || tok.maturity.slotDay != today {
		tok.maturity.slot, tok.maturity.slotDay = rollMaturitySlotInWindow(today, now)
	}
	slot := tok.maturity.slot
	lastTouch := tok.maturity.lastTouch
	tok.maturityMu.Unlock()
	if now.Before(slot) {
		p.maturityRecord(tok, "", "skip:slot", "", today)
		return false
	}
	if !lastTouch.IsZero() && now.Sub(lastTouch) < maturityThrottle {
		p.maturityRecord(tok, "", "skip:throttle", "", today)
		return false
	}
	// Fire gate: touches fire only inside the firing tail of the window
	// (T-5m→T-1m). Earlier is pre-flight — the classification above
	// runs, unarrived slots ledger skip:slot, and fire-ready tokens
	// ledger skip:preflight — but nothing fires. Past the gate's close
	// the night is over for unfired tokens: fire-ready ones honestly
	// ledger skip:window-exhausted instead of silently dropping.
	if !maturityInFireGate(now) {
		if _, gateEnd := maturityFireWindowFor(now); !now.Before(gateEnd) {
			p.maturityRecord(tok, "", "skip:window-exhausted", "", today)
		} else {
			p.maturityRecord(tok, "", "skip:preflight", "", today)
		}
		return false
	}

	// Precedence: per-token override, premium-short pool head, auto pick
	// when the global is the auto sentinel, else the explicit global
	// fallback. Empty resolves fail closed in the fire path.
	effective, _, pickReason := p.maturityResolveEffective(st, tok, touchModel)
	if p.maturityFire(ctx, effective, pickReason, idx, tok, label, cached, today, now) {
		p.maturityNoteRateLimit(now)
		return true
	}
	return false
}

// maturityRefreshStreak fetches one token's streak synchronously (bounded)
// and caches it. Only the stale path calls it; the hot path stays a pure
// cache read.
func (p *Pool) maturityRefreshStreak(ctx context.Context, tok *tokenEntry) (*upstream.StreakInfo, error) {
	fetch, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	st, err := tok.client.GetStreak(fetch)
	if err != nil || st == nil {
		if err == nil {
			err = fmt.Errorf("pool: empty streak response")
		}
		return nil, err
	}
	tok.SetStreak(st)
	return st, nil
}

// maturityAccountAdvance records streak movement against the fresh reading
// for the run ledger: a touch that moved the streak past its at-touch
// value marks last_advanced and emits the advance history event.
func (p *Pool) maturityAccountAdvance(idx int, tok *tokenEntry, cached *upstream.StreakInfo) {
	tok.maturityMu.Lock()
	m := &tok.maturity
	advanced := false
	m.lastStreak = cached.Streak
	if m.streakAtTouch > 0 && cached.Streak > m.streakAtTouch {
		m.lastAdvanced = "yes"
		m.streakAtTouch = 0
		advanced = true
	}
	tok.maturityMu.Unlock()
	// History emits happen outside the token mutex: the sink must never run
	// under pool locks.
	if advanced {
		p.emitMaturity(idx, "advance", fmt.Sprintf("streak=%d", cached.Streak))
	}
}

// maturityRefuse ledgers one fire-path refusal. The ledger result is
// written on every pass — the dashboard's last-run line must always name
// tonight's truth — while the history event and the warn log, the two
// writers that grow or spam per tick, land at most once per Pacific day per
// distinct refusal. A config- or meter-level refusal is a window-level fact
// that cannot change inside the night, so the first fire-gate tick records
// it and the rest of the window stays quiet. Pre-fix every 60s maintain
// pass re-entered the fire path and appended another maturity_events row:
// prod 2026-09-15 grew kind=touch detail="admit skip:touch-model model=" at
// exactly 60s intervals for the whole firing gate (37 rows for the one
// token whose meter priced every served unmetered row).
//
// The key is the refusal content (result + detail), never the clock, so it
// is immune to the tick count while a real change inside the window —
// different model, new price, config edit, meter movement — still records
// the transition. Nothing here can swallow a fire: a successful touch never
// reaches this path.
//
// logMsg "" suppresses the warn line (callers that record without logging).
func (p *Pool) maturityRefuse(tok *tokenEntry, idx int, result, detail, today, logMsg string, attrs ...any) {
	p.maturityRecord(tok, "admit", result, "", today)
	key := result + "\x00" + detail
	tok.maturityMu.Lock()
	recorded := tok.maturity.refusalDay == today && tok.maturity.refusalKey == key
	if !recorded {
		tok.maturity.refusalDay = today
		tok.maturity.refusalKey = key
	}
	tok.maturityMu.Unlock()
	if recorded {
		return
	}
	p.emitMaturity(idx, "touch", detail)
	if logMsg != "" {
		p.logger.Warn(logMsg, attrs...)
	}
}

// maturityFire performs one live touch: admit → one minimal turn → release
// through the token's own session manager and upstream client —
// wire-identical to a user opening the CLI and sending one message, because
// upstream advances streaks on agent-run message rows, not bare admission.
// It reports whether the touch was rate-limited (429): the nightly walk
// aborts on the first 429 and backs off. The IsServedModel honeypot
// rejection and the fail-closed priced-touch skips below stay untouched.
//
// pickReason is the automatic pick's resolution reason
// (maturityResolveEffective); it is consumed only when nothing resolved, so
// the refusal names why there is no model instead of a bare model=.
func (p *Pool) maturityFire(ctx context.Context, touchModel, pickReason string, idx int, tok *tokenEntry, label string, cached *upstream.StreakInfo, today string, now time.Time) (rateLimited bool) {
	st := p.maturityCopy(tok)
	model := touchModel
	if st.mode == MaturityModePremiumShort {
		premium := modelcat.SharedPremiumModels()
		if len(premium) == 0 {
			p.maturityRefuse(tok, idx, "skip:no-premium-model", "admit skip:no-premium-model", today, "")
			return
		}
		model = premium[0]
	} else if reason := maturityGuardTouchModel(model); reason != "" {
		if model == "" {
			// Nothing resolved: the meter (no price-0 served row) and
			// MATURITY_TOUCH_MODEL (itself the auto sentinel) are the
			// operator's levers, so name the resolution reason.
			p.maturityRefuse(tok, idx, reason,
				fmt.Sprintf("admit %s reason=%s", reason, pickReason), today,
				"pool: maturity touch has no admittable model, skipping",
				"token", idx+1, "token_label", label, "reason", pickReason)
			return
		}
		p.maturityRefuse(tok, idx, reason,
			fmt.Sprintf("admit %s model=%s", reason, model), today,
			"pool: maturity touch misconfigured (not a served unmetered model), skipping",
			"token", idx+1, "token_label", label, "model", model)
		return
	}
	// Meter-aware lane (issue #350 adaptation): the touch rides the
	// unmetered lane, never the meter. A touch on a model this account
	// meters (price > 0, no server exemption) skips instead of spending —
	// maturity preserves streaks, it never buys sessions.
	if snap := tok.sessionMgr().Snapshot(); snap.Freebucks != nil {
		if price, ok := snap.Freebucks.Prices[model]; ok && price > 0 && !snap.Freebucks.QuotaExempt {
			p.maturityRefuse(tok, idx, "skip:touch-priced",
				fmt.Sprintf("admit skip:touch-priced model=%s price=%v", model, price), today,
				"pool: maturity touch model is metered on this account, skipping",
				"token", idx+1, "token_label", label, "model", model, "price", price)
			return
		}
	}

	fire, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	action := "admit"
	err := p.maturityTouchRun(fire, tok, model, idx, label, now)
	result := "ok"
	if err != nil {
		result = "error:" + firstLine(err.Error())
		rateLimited = errors.Is(err, upstream.ErrRateLimited)
	}
	tok.maturityMu.Lock()
	tok.maturity.lastTouch = now
	tok.maturity.lastAction = action
	tok.maturity.lastResult = result
	tok.maturity.resultDay = today
	tok.maturity.touchDay = today
	if err == nil {
		tok.maturity.streakAtTouch = cached.Streak
	}
	tok.maturityMu.Unlock()
	p.emitMaturity(idx, "touch", fmt.Sprintf("%s %s model=%s streak=%d", action, result, model, cached.Streak))
	if err != nil {
		if rateLimited {
			p.logger.Warn("pool: maturity touch rate-limited, aborting nightly walk",
				"token", idx+1, "token_label", label, "action", action, "err", err)
		} else {
			p.logger.Warn("pool: maturity touch failed", "token", idx+1, "token_label", label, "action", action, "err", err)
		}
		return rateLimited
	}
	p.logger.Info("pool: maturity touch fired", "token", idx+1, "token_label", label,
		"action", action, "model", model, "streak", cached.Streak)
	return false
}

const (
	// maturityTouchPrompt is the trivial touch-turn user message: it exists
	// only to leave one agent-run message row upstream (bare admission
	// leaves none, so it never advances the streak).
	maturityTouchPrompt = "ping"
	// maturityRefundReplays bounds the pending-refund DELETE replay loop
	// after a touch release: the manager parks a pending receipt for later
	// EndSessions anyway, so the touch replays inline a few times and leaves
	// the rest to that path.
	maturityRefundReplays = 3
)

// maturityTouchRun performs the live streak touch: admit the touch model,
// run one minimal agent turn bound to the session instance (effort none —
// the reasoning_effort key stays absent, the silent-turn contract), finish
// the run, then release the session with pending-refund replay. The turn is
// the load-bearing step: upstream advances streaks on agent-run message
// rows, not bare admission.
//
// The turn rides the token's upstream client directly — never Pool.Chat —
// so it cannot feed the Pacific-day activity ledger (recordDayRequest): the
// client-active skip stays a pure client-traffic signal. A failed turn keeps
// the admitted session for real traffic to reuse (the old admit-only
// behavior); only a successful turn releases it, through the session
// manager's real refund handler (the vendor port's recordReleaseReceipt +
// RefreshRefund), never a body-discarding DELETE.
func (p *Pool) maturityTouchRun(ctx context.Context, tok *tokenEntry, model string, idx int, label string, now time.Time) error {
	instanceID, err := tok.session.EnsureSessionForModel(ctx, model)
	if err != nil {
		return err
	}
	agentID := ""
	if p.reg != nil {
		if a, aerr := p.reg.AgentForModel(model); aerr == nil {
			agentID = a
		}
	}
	if agentID == "" {
		p.logger.Warn("pool: maturity touch has no agent for model, keeping session for reuse",
			"token", idx+1, "token_label", label, "model", model)
		return fmt.Errorf("pool: maturity touch: no agent for model %s", model)
	}
	runID, err := tok.client.StartRun(ctx, agentID)
	if err != nil {
		return err
	}
	turnErr := p.maturityTouchTurn(ctx, tok, model, agentID, runID, instanceID)
	finStatus := "completed"
	if turnErr != nil {
		finStatus = "failed"
	}
	step := upstream.RunStep{
		ID:         fmt.Sprintf("maturity-touch-%d", now.UnixNano()),
		StepNumber: 1,
		Status:     finStatus,
		StartTime:  now.UTC().Format(time.RFC3339Nano),
	}
	// Every START gets its FINISH (the runs drain path does the same): the
	// failure path still closes the run, best-effort, without masking the
	// turn error.
	ferr := tok.client.FinishRun(ctx, runID, finStatus, 1, []upstream.RunStep{step}, "")
	if turnErr != nil {
		return turnErr
	}
	if ferr != nil {
		return ferr
	}
	if rerr := p.maturityReleaseTouch(ctx, tok, label, idx); rerr != nil {
		// The streak goal (one message row) is already met and the manager
		// keeps any parked refund replayable — release trouble stays
		// warn-only.
		p.logger.Warn("pool: maturity touch release troubled, streak turn already landed",
			"token", idx+1, "token_label", label, "err", rerr)
	}
	return nil
}

// maturityTouchTurn sends the one minimal touch turn: a trivial prompt with
// no reasoning_effort key (effort none), bound to the admitted instance and
// the fresh run. The response body is drained (bounded) and closed; only its
// 2xx matters — the message row it leaves upstream is the streak advance.
func (p *Pool) maturityTouchTurn(ctx context.Context, tok *tokenEntry, model, agentID, runID, instanceID string) error {
	body, err := json.Marshal(map[string]any{
		"model":    model,
		"stream":   false,
		"messages": []map[string]string{{"role": "user", "content": maturityTouchPrompt}},
	})
	if err != nil {
		return err
	}
	rc, err := tok.client.ChatCompletions(ctx, upstream.ChatOptions{
		Model:             model,
		RunID:             runID,
		SessionInstanceID: instanceID,
		AgentID:           agentID,
		StepNumber:        1,
	}, body)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 64*1024))
	_ = rc.Close()
	return nil
}

// maturityReleaseTouch releases the touch session through the session
// manager — the port's real refund handler — then replays a parked
// pending-refund DELETE inline (bounded): a settled replay records
// lastRefund, a still-pending one stays parked for a later EndSession.
func (p *Pool) maturityReleaseTouch(ctx context.Context, tok *tokenEntry, label string, idx int) error {
	if err := tok.session.EndSession(ctx); err != nil {
		return err
	}
	for i := 0; i < maturityRefundReplays && tok.session.Snapshot().PendingRefund != ""; i++ {
		if err := tok.session.RefreshRefund(ctx); err != nil {
			return err
		}
	}
	return nil
}

// maturityGuardTouchModel fails a misconfigured unmetered touch model
// closed: the model must be a served, non-premium catalog row, else the
// tick (and the manual touch, which funnels through the same fire path)
// records skip:touch-model before any upstream admission. An empty model
// fails closed too — there is nothing to admit, and IsServed("") is false —
// so a night with no resolvable model never invents one.
func maturityGuardTouchModel(model string) string {
	if !modelcat.IsServed(model) || modelcat.IsPremium(model) {
		return "skip:touch-model"
	}
	return ""
}

// MaturityTouchNow fires one manual maturity touch outside the nightly
// window (API lever; no UI wires it). Slot wait, window gate, and 6h
// throttle are bypassed; health gates, streak freshness, and todayUsed
// still apply. Universal automatic: the per-token enabled flag is ignored.
// It returns the action (probe/admit) and result for the confirmation line.
func (p *Pool) MaturityTouchNow(ctx context.Context, token int) (string, string, error) {
	toks := p.roster.Load()
	if toks == nil || token < 0 || token >= len(*toks) {
		return "", "", fmt.Errorf("pool: token %d out of range", token)
	}
	cfg := p.cfg.Load()
	if cfg == nil || !cfg.MaturityEnabled {
		return "", "", fmt.Errorf("pool: maturity automation is disabled (MATURITY_ENABLED=0)")
	}
	tok := (*toks)[token]
	now := time.Now()
	today := pacificDayKey(now)
	st := p.maturityCopy(tok)
	p.clearLiftedQuarantine(tok)
	if q := tok.quarantine.Load(); q != nil {
		return "", "skip:quarantined", fmt.Errorf("pool: token %d is quarantined (%s)", token, q.reason)
	}
	rs := tok.runs.Snapshot()
	if rs.BanError != nil && (rs.BannedUntil.IsZero() || now.Before(rs.BannedUntil)) {
		return "", "skip:banned", fmt.Errorf("pool: token %d is banned", token)
	}
	if !rs.CooldownUntil.IsZero() && now.Before(rs.CooldownUntil) {
		return "", "skip:cooling", fmt.Errorf("pool: token %d is cooling down", token)
	}
	if tok.runs.CountryBlockedError() != nil {
		return "", "skip:country-blocked", fmt.Errorf("pool: token %d is country-blocked", token)
	}
	cached := tok.Streak()
	if cached == nil || now.Sub(cached.UpdatedAt) > maturityStreakFresh {
		var err error
		cached, err = p.maturityRefreshStreak(ctx, tok)
		if err != nil || cached == nil {
			p.maturityRecord(tok, "", "skip:streak-stale", "stale", today)
			return "", "skip:streak-stale", fmt.Errorf("pool: token %d streak unavailable", token)
		}
	}
	if cached.TodayUsed {
		p.maturityRecord(tok, "", "skip:today-used", "yes", today)
		return "", "skip:today-used", fmt.Errorf("pool: token %d already used today", token)
	}
	effective, _, pickReason := p.maturityResolveEffective(st, tok, cfg.MaturityTouchModel)
	p.maturityFire(ctx, effective, pickReason, token, tok, tokenEntryLabel(tok), cached, today, now)
	p.saveMaturity(token, tok)
	fin := p.maturityCopy(tok)
	return fin.lastAction, fin.lastResult, nil
}

// maturityRecord stores a skip/result marker without touching touch times.
// Universal automatic: no enabled gate — every account is enrolled. Every
// write stamps today's Pacific day as the result day (from the caller,
// which owns the clock): the dashboard scopes per-account Skipped rows to
// tonight's run while the last-run ledger stays historical. Empty today
// falls back to the wall-clock Pacific day, never a stale stamp.
func (p *Pool) maturityRecord(tok *tokenEntry, action, result, advanced, today string) {
	tok.maturityMu.Lock()
	defer tok.maturityMu.Unlock()
	if action != "" {
		tok.maturity.lastAction = action
	}
	tok.maturity.lastResult = result
	if today == "" {
		today = pacificDayKey(time.Now())
	}
	tok.maturity.resultDay = today
	if advanced != "" {
		tok.maturity.lastAdvanced = advanced
	}
}

// maturitySnapshot builds the dashboard view for one entry (nil until first
// enabled). Badge: Mature when the streak reached target, Warming while an
// enabled token still climbs, Cold when an enabled token sits at zero.
// SlotDay/TouchDay (one Pacific calendar shared by the whole pool) plus the
// resolved effective/auto models let the dashboard render the streak chip,
// the last-run ledger, and the Auto pick with its reason without a new
// scheduler.
func (p *Pool) maturitySnapshot(tok *tokenEntry, streak int) *MaturitySnapshot {
	tok.maturityMu.Lock()
	m := tok.maturity
	tok.maturityMu.Unlock()
	// A drafted touch-model override counts as state: operators pre-configure
	// it while disabled, and the card must echo it back after refresh.
	if !m.enabled && m.lastAction == "" && m.lastResult == "" && m.touchModel == "" {
		return nil
	}
	target := m.target
	if target <= 0 {
		target = p.maturityDefaultTarget()
	}
	badge := ""
	switch {
	case streak >= target && target > 0:
		badge = "Mature"
	case m.enabled && streak > 0:
		badge = "Warming"
	case m.enabled:
		badge = "Cold"
	}
	mode := m.mode
	if mode == "" {
		mode = MaturityModeUnmetered
	}
	global := ""
	if cfg := p.cfg.Load(); cfg != nil {
		global = cfg.MaturityTouchModel
	}
	effective, auto, reason := p.maturityResolveEffective(maturityState{enabled: m.enabled, target: m.target, mode: mode, touchModel: m.touchModel}, tok, global)
	return &MaturitySnapshot{
		Enabled:             m.enabled,
		Target:              target,
		Mode:                mode,
		TouchModel:          m.touchModel,
		Badge:               badge,
		Slot:                m.slot,
		SlotDay:             m.slotDay,
		LastTouch:           m.lastTouch,
		TouchDay:            m.touchDay,
		LastAction:          m.lastAction,
		LastResult:          m.lastResult,
		LastAdvanced:        m.lastAdvanced,
		ResultDay:           m.resultDay,
		EffectiveTouchModel: effective,
		AutoTouchModel:      auto,
		AutoTouchReason:     reason,
	}
}

// maturityWindowFor returns the nightly maintenance window for now: end is
// the upcoming Pacific midnight, start is maturityRunWindow earlier. One
// collapsed pre-reset window for every account, so touches land right
// before upstream rolls the daily streak.
//
// DST-safe: the boundary is America/Los_Angeles wall-clock midnight via
// time.Date calendar math (never a fixed UTC offset — July midnight is
// 07:00Z, January is 08:00Z). Pacific midnight never falls in a DST gap
// (LA transitions at 02:00), and AddDate re-resolves the offset for the
// new date, so 23h/25h DST days still end at the right instant.
func maturityWindowFor(now time.Time) (start, end time.Time) {
	loc := maturityLocation("America/Los_Angeles")
	y, m, d := now.In(loc).Date()
	end = time.Date(y, m, d, 0, 0, 0, 0, loc).AddDate(0, 0, 1)
	return end.Add(-maturityRunWindow), end
}

// maturityInWindow reports whether now falls inside tonight's maintenance
// window ([start, end): touches fire at/after start, never at/after the
// reset).
func maturityInWindow(now time.Time) bool {
	start, end := maturityWindowFor(now)
	return !now.Before(start) && now.Before(end)
}

// maturityFireWindowFor returns the firing tail of tonight's window:
// [end-maturityFireGate, end-maturitySlotEndBuffer) (T-5m→T-1m). Slots
// draw only up to end-maturitySlotEndBuffer, so every slot arrives at
// or before the gate's close — a token still unfired past the close is
// genuinely out of night, never early.
func maturityFireWindowFor(now time.Time) (start, end time.Time) {
	_, wend := maturityWindowFor(now)
	return wend.Add(-maturityFireGate), wend.Add(-maturitySlotEndBuffer)
}

// maturityInFireGate reports whether touches may fire now: inside the
// window's final maturityFireGate, clear of the slot end buffer.
func maturityInFireGate(now time.Time) bool {
	start, end := maturityFireWindowFor(now)
	return !now.Before(start) && now.Before(end)
}

// MaturityWindow exposes tonight's maintenance window for the dashboard
// countdown (absolute instants: the SPA only formats them).
func (p *Pool) MaturityWindow() (start, end time.Time) {
	return maturityWindowFor(time.Now())
}

// pacificDayKey renders the Pacific calendar day containing now
// ("2006-01-02"): slotDay/touchDay live in this day so the whole pool
// shares one pre-reset calendar.
func pacificDayKey(now time.Time) string {
	return now.In(maturityLocation("America/Los_Angeles")).Format("2006-01-02")
}

// rollMaturitySlotInWindow draws one token's staggered slot uniformly
// inside tonight's window, reserving the final maturitySlotEndBuffer
// before the reset: every enrolled token fires once per night at a
// different minute, and a restart re-roll can only cause one extra cheap
// touch (touchDay/todayUsed + the 6h throttle stay the idempotency bound).
func rollMaturitySlotInWindow(today string, now time.Time) (time.Time, string) {
	start, end := maturityWindowFor(now)
	end = end.Add(-maturitySlotEndBuffer)
	if span := end.Sub(start); span > 0 {
		return start.Add(time.Duration(sessionRand() % uint64(span))), today
	}
	return start, today
}

// maturityBackedOff reports whether the nightly walk is paused after a 429.
func (p *Pool) maturityBackedOff(now time.Time) bool {
	p.maturityBackoffMu.Lock()
	defer p.maturityBackoffMu.Unlock()
	return !p.maturityBackoffUntil.IsZero() && now.Before(p.maturityBackoffUntil)
}

// maturityNoteRateLimit pauses the nightly walk after a rate-limited touch
// (429 abort+backoff): the walk stops for maturity429Backoff instead of
// hammering a throttled upstream. In-memory only — a restart clears it,
// and touchDay/todayUsed still prevent double-touches.
func (p *Pool) maturityNoteRateLimit(now time.Time) {
	p.maturityBackoffMu.Lock()
	p.maturityBackoffUntil = now.Add(maturity429Backoff)
	p.maturityBackoffMu.Unlock()
	p.logger.Warn("pool: maturity nightly walk backed off after 429",
		"backoff", maturity429Backoff.String())
}

// maturityLocation resolves the account timezone for slot math: the streak
// response's IANA name, Pacific fallback (upstream rolls its daily windows
// at Pacific midnight).
func maturityLocation(tz string) *time.Location {
	if tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		return time.UTC
	}
	return loc
}

// firstLine truncates an error for the compact last_result field.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if len(s) > 120 {
		return s[:120]
	}
	return s
}
