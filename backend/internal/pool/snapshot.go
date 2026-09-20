// snapshot.go — pool-wide health snapshots for /healthz and /metrics.
package pool

import (
	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/upstream"
	"time"
)

// BridgeTokenSnapshot is a dashboard-ready view of one bridge entry (#187).
type BridgeTokenSnapshot struct {
	Key           string                           `json:"key"` // raw client token (hashed for display)
	LastUsed      time.Time                        `json:"last_used"`
	ActiveRuns    int                              `json:"active_runs"`
	Requests      int                              `json:"requests"`
	Locked        bool                             `json:"locked"`
	CooldownUntil time.Time                        `json:"cooldown_until"`
	SessionActive bool                             `json:"session_active"`
	Model         string                           `json:"model"`
	AccessTier    string                           `json:"access_tier,omitempty"`
	QuotaByModel  map[string]session.QuotaSnapshot `json:"quota_by_model,omitempty"`
	// Freebucks is the upstream Freebucks allowance block (issue #232); nil
	// when the bridge entry has no Freebucks quota.
	Freebucks *upstream.FreebucksInfo `json:"freebucks,omitempty"`
	SpendDay  float64                 `json:"spend_day"`
	// BanType / BannedUntil mirror TokenSnapshot's active-ban view
	// (issues #198/#199): "temporary" (auto-lifts at BannedUntil) vs
	// "hard" (never self-heals); zero values when no ban is active.
	BanType     string    `json:"ban_type,omitempty"`
	BannedUntil time.Time `json:"banned_until,omitempty"`
}

// banView derives the snapshot ban view from a remembered runs ban
// (issues #198/#199). A hard ban (zero ResumesAt) is PERMANENT —
// runs.CooldownBan keeps no timed window for it, so BannedUntil stays zero
// and the type is read off BanError.ResumesAt directly; a temporary ban
// renders with its ResumesAt deadline. Returns ""/zero when no ban is
// active.
func banView(ban *upstream.BanError, until time.Time) (string, time.Time) {
	if ban == nil || (!until.IsZero() && !time.Now().Before(until)) {
		return "", time.Time{}
	}
	if ban.ResumesAt.IsZero() {
		return "hard", time.Time{}
	}
	return "temporary", ban.ResumesAt
}

// MaturitySnapshot is the streak-maturity automation view carried on
// TokenSnapshot. The automation itself is excised (Fase E): the pool never
// populates it (always nil from Snapshot) and the dashboard renders no
// card. The type is kept so historical payloads, the dashboard mapper, and
// its tests still compile.
type MaturitySnapshot struct {
	Enabled bool   `json:"enabled"`
	Target  int    `json:"target"`
	Mode    string `json:"mode"`
	// TouchModel is the per-token touch-model override ("" = automatic).
	// Omitted on the wire when unset so never-enrolled tokens keep
	// their existing payload shape.
	TouchModel string    `json:"touch_model,omitempty"`
	Badge      string    `json:"badge"`
	Slot       time.Time `json:"slot,omitempty"`
	// SlotDay is the account-timezone calendar day the Slot belongs to
	// ("2006-01-02").
	SlotDay   string    `json:"slot_day,omitempty"`
	LastTouch time.Time `json:"last_touch,omitempty"`
	// TouchDay is the account-timezone calendar day of the last touch
	// ("2006-01-02"): TouchDay == SlotDay means touched today.
	TouchDay     string `json:"touch_day,omitempty"`
	LastAction   string `json:"last_action,omitempty"`
	LastResult   string `json:"last_result,omitempty"`
	LastAdvanced string `json:"last_advanced,omitempty"`
	// ResultDay is the Pacific calendar day ("2006-01-02") the last
	// ledger write belongs to.
	ResultDay string `json:"result_day,omitempty"`
	// EffectiveTouchModel is the model the next touch would actually admit.
	EffectiveTouchModel string `json:"effective_touch_model,omitempty"`
	// AutoTouchModel is the automatic pick with AutoTouchReason naming why.
	AutoTouchModel  string `json:"auto_touch_model,omitempty"`
	AutoTouchReason string `json:"auto_touch_reason,omitempty"`
}

// cloneLimitedModelOffers detaches the offer slice from the session
// snapshot: TokenSnapshot is consumed outside the pool (dashboard/healthz)
// and must never alias pooled live state, the same copy-bug class
// cloneFreebucksInfo guards for the persisted snapshot. Nil stays nil —
// never zero-allocated.
func cloneLimitedModelOffers(in []upstream.LimitedModelOffer) []upstream.LimitedModelOffer {
	if len(in) == 0 {
		return nil
	}
	out := make([]upstream.LimitedModelOffer, len(in))
	copy(out, in)
	return out
}

// Snapshot returns the per-token healthz view.
func (p *Pool) Snapshot() []TokenSnapshot {
	toks := p.roster.Load()
	out := make([]TokenSnapshot, 0, len(*toks))
	// Single-pin view (PIN_MODEL): per-slot pins for the dashboard +
	// metrics. Read once per snapshot; hot-reload safe.
	var pinModel map[int]string
	if c := p.cfg.Load(); c != nil {
		pinModel = c.PinModel
	}
	for i, tok := range *toks {
		rs := tok.runs.Snapshot()
		ss := tok.session.Snapshot()
		// One roster lock acquisition for all three ledger counters (issue
		// #656): this loop runs on every dashboard SSE tick and read
		// endpoint, and each extra acquisition serialized with the request
		// completion path.
		msgs, spend, reqsPerDay := p.ledgerSnapshot(i)

		// Region view: the session snapshot carries the last admitted country; an
		// active country-block cooldown overrides it with the remembered
		// block (the session never admitted after a block, so its snapshot
		// would be empty for the blocked country).
		countryCode, countryReason := ss.CountryCode, ss.CountryBlockReason
		if cbe := tok.runs.CountryBlockedError(); cbe != nil {
			if cbe.CountryCode != "" {
				countryCode = cbe.CountryCode
			}
			if cbe.CountryBlockReason != "" {
				countryReason = cbe.CountryBlockReason
			}
		}

		// Countdown: prefer the server-authored absolute expiry over wire
		// remainingMs. The expiry is monotonic and survives compact polls
		// (which omit remainingMs — savedRemainingMs would otherwise freeze
		// the countdown at the admission value). RemainingMs is only trusted
		// when the server never sent an expiry (legacy state): when
		// ExpiresAt is set and already past, the session is dead — falling
		// back to the frozen admission RemainingMs would resurrect a
		// zombie "3600s remaining" row (the exact stale-state report behind
		// the 0m 0s-remaining drawer on an expired session).
		sessionRemaining := int64(0)
		sessionStatus := ss.Status
		if ss.Status == "active" && !ss.ExpiresAt.IsZero() {
			if rem := time.Until(ss.ExpiresAt); rem > 0 {
				sessionRemaining = int64(rem.Seconds())
			} else if !ss.GracePeriodEndsAt.IsZero() && time.Now().Before(ss.GracePeriodEndsAt) {
				// Expiry crossed but the grace drain is still open: the
				// row serves in-flight runs until graceEndsAt. Report the
				// drain honestly rather than a live window.
				sessionStatus = "grace"
			} else {
				// Expiry and grace both passed with the cache still
				// "active": report the honest terminal state instead of a
				// live row. The pool re-admits on the next request
				// (sessionUsable → false) or the next liveness poll
				// observes it once polls resume.
				sessionStatus = "expired"
			}
		}
		if sessionRemaining == 0 && ss.ExpiresAt.IsZero() && ss.RemainingMs > 0 {
			sessionRemaining = ss.RemainingMs / 1000
		}

		// No live instance: the row carries no live-session facts. The
		// manager stashes the last-seen countdown across invalidation by
		// design (restart resume), so without this the row would render a
		// stale model/countdown/expiry for a session that no longer exists.
		// A synthesized "expired" row is terminal too: expiry and grace both
		// passed while the cache still says "active" (polls stopped, so no
		// poll/store invalidation observed it), and every terminal path —
		// Invalidate (commit(nil)), poll past-grace, store load — drops the
		// slot. Render it like IDLE instead of the stale cache values.
		sessionModel := ss.Model
		sessionExpiresAt := ss.ExpiresAt
		sessionInstanceID := ss.InstanceID
		sessionQueuePosition := ss.QueuePosition
		sessionQueueDepth := ss.QueueDepth
		if ss.InstanceID == "" || sessionStatus == "expired" {
			sessionInstanceID = ""
			sessionModel = ""
			sessionRemaining = 0
			sessionExpiresAt = time.Time{}
			sessionQueuePosition = 0
			sessionQueueDepth = 0
		}
		// Active-ban view for healthz/dashboard consumers (issues #198/#199).
		banType, bannedUntil := banView(rs.BanError, rs.BannedUntil)
		q := tok.quarantine.Load()
		quarantineReason := ""
		if q != nil {
			// Lift-aware quarantine: a temporary ban's marker may have
			// timed out; clear it before rendering so the dashboard never
			// reports a lifted account as terminal.
			if p.clearLiftedQuarantine(tok) {
				q = nil
			}
			if q != nil {
				quarantineReason = q.reason
			}
		}
		// Streak is a pure cache read here: backfills run on the maintain
		// loop, never on the Snapshot path (which must not issue upstream
		// traffic). Nil streak simply renders no streak card.
		var streak int
		var todayUsed bool
		var lastUsage string
		var streakUpdated time.Time
		if st := tok.Streak(); st != nil {
			streak = st.Streak
			todayUsed = st.TodayUsed
			lastUsage = st.LastUsageDate
			streakUpdated = st.UpdatedAt
		}

		liveTurns, queuedWaiters, oldestWait := p.slotEntryStats(tok)

		out = append(out, TokenSnapshot{
			Token:                   i,
			Email:                   tok.Email(),
			AccountID:               tok.AccountID(),
			CooldownUntil:           rs.CooldownUntil,
			CooldownKind:            rs.RateLimitKind,
			CooldownWindowHours:     rs.RateLimitWindowHours,
			CooldownResetsAt:        rs.RateLimitResetsAt,
			ActiveRuns:              rs.ActiveRuns,
			Requests:                rs.Requests,
			Messages24h:             msgs,
			LiveTurns:               liveTurns,
			QueuedWaiters:           queuedWaiters,
			OldestWaiterMS:          oldestWait.Milliseconds(),
			RequestsPerDay:          reqsPerDay,
			SessionStatus:           sessionStatus,
			SessionInstanceID:       sessionInstanceID,
			SessionQueuePosition:    sessionQueuePosition,
			SessionQueueDepth:       sessionQueueDepth,
			SessionModel:            sessionModel,
			SessionRemainingSeconds: sessionRemaining,
			SessionExpiresAt:        sessionExpiresAt,
			CountryCode:             countryCode,
			CountryBlockReason:      countryReason,
			AccessTier:              ss.AccessTier,
			SubscriptionTierID:      ss.SubscriptionTierID,
			LimitedModelOffers:      cloneLimitedModelOffers(ss.LimitedModelOffers),
			LimitedOfferReason:      ss.LimitedOfferReason,
			SessionActiveUsersForIP: ss.ActiveUsersForIP,
			QuotaByModel:            ss.QuotaByModel,
			QuotaStale:              ss.QuotaStale,
			QuotaSavedAt:            ss.QuotaSavedAt,
			Entitlement:             ss.Entitlement,
			GlmPromo:                ss.GlmPromo,
			Standing:                ss.Standing,
			Referral:                ss.Referral,
			Freebucks:               ss.Freebucks,
			LastRefund:              ss.LastRefund,
			PendingRefund:           ss.PendingRefund,
			Streak:                  streak,
			TodayUsed:               todayUsed,
			LastUsageDate:           lastUsage,
			StreakUpdatedAt:         streakUpdated,
			UpgradeHint:             ss.UpgradeHint,
			ServerMessage:           ss.ServerMessage,
			Locked:                  tok.locked.Load(),
			Quarantined:             q != nil,
			QuarantineReason:        quarantineReason,
			PinnedModel:             pinModel[i],
			PinSkips:                tok.pinSkips.Load(),
			TransientRetries:        tok.client.TransientRetries(),
			FingerprintRotations:    tok.client.FingerprintRotations(),
			RateLimitEvents:         tok.client.RateLimitEvents(),
			ModelLocked:             tok.session.ModelLocked(),
			Spend24h:                spend.Rolling24h,
			SpendDay:                spend.Day,
			SpendWeek:               spend.Week,
			SpendMonth:              spend.Month,
			SpendDayStart:           spend.DayStart,
			SpendWeekStart:          spend.WeekStart,
			SpendMonthStart:         spend.MonthStart,
			SpendLimited:            spend.SpendLimited,
			BanType:                 banType,
			BannedUntil:             bannedUntil,
		})
	}
	return out
}

// PoolSnapshot is the pool-wide metrics view: aggregate transient-retry
// counters summed across every fixed token's client, plus the per-token rows
// (same shape as Snapshot). Bridge-mode entries are not counted in the
// per-token rows (they are per-client-token ephemeral slots), but live
// bridge clients' retry/rotation counters are summed in, and RequestsServed
// is mode-independent (every successful upstream chat).
type PoolSnapshot struct {
	TransientRetries     int64
	FingerprintRotations int64
	RequestsServed       uint64
	Tokens               []TokenSnapshot
	// Quarantined is the count of fixed pooled tokens currently in
	// terminal-quarantine (live bans). Surfaced so the operator can see at
	// a glance how many accounts the pool has permanently stopped leasing.
	Quarantined int
}

// PoolSnapshot returns the pool-wide snapshot with aggregate counters.
func (p *Pool) PoolSnapshot() PoolSnapshot {
	ps := PoolSnapshot{Tokens: p.Snapshot(), RequestsServed: p.requestsServed.Load()}
	toks := p.roster.Load()
	for _, tok := range *toks {
		ps.TransientRetries += tok.client.TransientRetries()
		ps.FingerprintRotations += tok.client.FingerprintRotations()
		if tok.quarantine.Load() != nil {
			ps.Quarantined++
		}
	}
	// Live bridge entries: their counters survive while the entry is cached
	// (LRU eviction drops old ones — the view is "recent bridge activity").
	p.bridgeMu.Lock()
	for _, be := range p.bridge {
		ps.TransientRetries += be.client.TransientRetries()
		ps.FingerprintRotations += be.client.FingerprintRotations()
	}
	p.bridgeMu.Unlock()
	return ps
}
