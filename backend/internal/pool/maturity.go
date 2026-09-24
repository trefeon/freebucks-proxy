// maturity.go — automated streak maintenance and on-demand streak touch.
//
// Automatically evaluates pooled accounts during the nightly window before
// Pacific-midnight reset (23:45–00:00 Pacific, as pinned by upstream vendor
// FREEBUFF_STREAK_TIME_ZONE).
// Ensures accounts maintain active streaks by sending a zero-cost touch turn
// on served unmetered models (such as upstage/solar-mini4), never consuming
// user Freebucks quota.
package pool

import (
	"context"
	cryptoRand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"freebucks-proxy/backend/internal/modelcat"
	"freebucks-proxy/backend/internal/upstream"
)

const (
	// maturityThrottle is the minimum gap between two touches on one token.
	maturityThrottle = 6 * time.Hour
	// maturityStreakFresh bounds streak-cache age for touch decisions.
	maturityStreakFresh = time.Hour
	// maturityRunWindow is the fixed nightly maintenance window: 15 minutes before reset (23:45–00:00 Pacific).
	maturityRunWindow = 15 * time.Minute
	// maturityFireGate is the firing tail of the nightly window: touches fire in the last 5 minutes (T-5m→T-1m).
	maturityFireGate = 5 * time.Minute
	// maturitySlotEndBuffer reserves the final minute before the reset.
	maturitySlotEndBuffer = time.Minute
	// maturityTouchPrompt is the minimal touch message to leave an active message turn upstream.
	maturityTouchPrompt = "ping"
)

var maturity429Backoff = 3 * time.Minute

func maturityLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.FixedZone("PST", -8*3600)
	}
	return loc
}

func pacificDayKey(now time.Time) string {
	return now.In(maturityLocation("America/Los_Angeles")).Format("2006-01-02")
}

func maturityWindowFor(now time.Time) (start, end time.Time) {
	loc := maturityLocation("America/Los_Angeles")
	y, m, d := now.In(loc).Date()
	end = time.Date(y, m, d, 0, 0, 0, 0, loc).AddDate(0, 0, 1)
	return end.Add(-maturityRunWindow), end
}

func maturityInWindow(now time.Time) bool {
	start, end := maturityWindowFor(now)
	return !now.Before(start) && now.Before(end)
}

func maturityFireWindowFor(now time.Time) (start, end time.Time) {
	_, wend := maturityWindowFor(now)
	return wend.Add(-maturityFireGate), wend.Add(-maturitySlotEndBuffer)
}

func maturityInFireGate(now time.Time) bool {
	start, end := maturityFireWindowFor(now)
	return !now.Before(start) && now.Before(end)
}

// MaturityWindow exposes tonight's maintenance window for dashboard countdowns.
func (p *Pool) MaturityWindow() (start, end time.Time) {
	return maturityWindowFor(time.Now())
}

func (p *Pool) maturityBackedOff(now time.Time) bool {
	p.maturityBackoffMu.Lock()
	defer p.maturityBackoffMu.Unlock()
	return !p.maturityBackoffUntil.IsZero() && now.Before(p.maturityBackoffUntil)
}

func (p *Pool) maturityNoteRateLimit(now time.Time) {
	p.maturityBackoffMu.Lock()
	p.maturityBackoffUntil = now.Add(maturity429Backoff)
	p.maturityBackoffMu.Unlock()
	p.logger.Warn("pool: maturity nightly walk backed off after 429", "backoff_until", p.maturityBackoffUntil)
}

// maturityResolveModel resolves an unmetered model for the streak touch.
func (p *Pool) maturityResolveModel(tok *tokenEntry, override string) (string, error) {
	// 1. Explicit override if not sentinel
	if override != "" && !modelcat.IsAutoTouchSentinel(override) {
		return override, nil
	}

	// 2. Resolve via modelcat.AutoUnmeteredTouchModel
	var prices map[string]float64
	exempt := false
	if snap := tok.session.Snapshot(); snap.Freebucks != nil {
		prices = snap.Freebucks.Prices
		exempt = snap.Freebucks.QuotaExempt
	}

	autoModel, reason := modelcat.AutoUnmeteredTouchModel(prices, exempt)
	if autoModel != "" {
		return autoModel, nil
	}

	// 3. Known served unmetered fallbacks
	for _, candidate := range []string{"upstage/solar-mini4", "z-ai/glm-5.3-flash"} {
		if p.reg != nil {
			if _, err := p.reg.AgentForModel(candidate); err == nil {
				return candidate, nil
			}
		}
	}

	return "", fmt.Errorf("no unmetered model available for maturity touch (%s)", reason)
}

// maturityTick is called periodically from maintainTick.
func (p *Pool) maturityTick(ctx context.Context) {
	p.maturityTickAt(ctx, time.Now())
}

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
	for i, tok := range *toks {
		if p.maturityTickOne(ctx, cfg.MaturityTouchModel, i, tok, now, false) {
			return
		}
	}
}

func (p *Pool) recordMaturitySkip(tok *tokenEntry, idx int, skipReason, today string) {
	tok.maturityMu.Lock()
	defer tok.maturityMu.Unlock()
	if tok.maturity == nil {
		tok.maturity = &MaturitySnapshot{
			Enabled:    true,
			Target:     7,
			Mode:       "unmetered",
			SlotDay:    today,
			LastResult: skipReason,
			ResultDay:  today,
		}
	} else {
		tok.maturity.LastResult = skipReason
		tok.maturity.ResultDay = today
	}
}

func (p *Pool) maturityTickOne(ctx context.Context, touchOverride string, idx int, tok *tokenEntry, now time.Time, forceNow bool) (rateLimited bool) {
	label := tok.Email()
	if label == "" {
		label = fmt.Sprintf("Account #%d", idx+1)
	}
	today := pacificDayKey(now)
	inWindow := maturityInWindow(now) || forceNow

	// 1. Health checks
	if tok.locked.Load() {
		if inWindow {
			p.recordMaturitySkip(tok, idx, "skip:locked", today)
		}
		return false
	}
	p.clearLiftedQuarantine(tok)
	if q := tok.quarantine.Load(); q != nil {
		if inWindow {
			p.recordMaturitySkip(tok, idx, "skip:quarantined", today)
		}
		return false
	}
	rs := tok.runs.Snapshot()
	if rs.BanError != nil && (rs.BannedUntil.IsZero() || now.Before(rs.BannedUntil)) {
		if inWindow {
			p.recordMaturitySkip(tok, idx, "skip:banned", today)
		}
		return false
	}
	if !rs.CooldownUntil.IsZero() && now.Before(rs.CooldownUntil) {
		if inWindow {
			p.recordMaturitySkip(tok, idx, "skip:cooling", today)
		}
		return false
	}
	if tok.runs.CountryBlockedError() != nil {
		if inWindow {
			p.recordMaturitySkip(tok, idx, "skip:country-blocked", today)
		}
		return false
	}

	// 2. Fetch fresh streak if stale
	cached := tok.Streak()
	if cached == nil || now.Sub(cached.UpdatedAt) > maturityStreakFresh {
		fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		st, err := tok.client.GetStreak(fetchCtx)
		cancel()
		if err == nil && st != nil {
			tok.SetStreak(st)
			cached = st
		}
	}

	// 3. Client traffic check
	if p.dayRequestCount(idx) > 0 {
		if inWindow {
			p.recordMaturitySkip(tok, idx, "skip:client-active", today)
		}
		return false
	}

	// 4. Upstream TodayUsed check
	if cached != nil && cached.TodayUsed {
		if inWindow {
			p.recordMaturitySkip(tok, idx, "skip:today-used", today)
		}
		return false
	}

	// 5. Already touched today check
	m := tok.MaturitySnapshot()
	if m != nil && m.TouchDay == today && !m.LastTouch.IsZero() && !strings.HasPrefix(m.LastResult, "skip:") {
		return false
	}
	if m != nil && !m.LastTouch.IsZero() && now.Sub(m.LastTouch) < maturityThrottle {
		return false
	}

	// 6. Timing window check
	if !forceNow {
		if !inWindow {
			return false
		}
		if !maturityInFireGate(now) {
			p.recordMaturitySkip(tok, idx, "skip:preflight", today)
			return false
		}
	}

	// 7. Fire touch
	model, err := p.maturityResolveModel(tok, touchOverride)
	if err != nil {
		p.logger.Warn("pool: maturity touch model resolution failed", "token", idx+1, "err", err)
		p.recordMaturitySkip(tok, idx, "skip:no-unmetered-model", today)
		return false
	}

	p.logger.Info("pool: firing streak touch", "token", idx+1, "model", model, "today", today)
	fireCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	touchErr := p.maturityTouchRun(fireCtx, tok, model, idx, label, now)
	result := "ok"
	if touchErr != nil {
		result = "error: " + touchErr.Error()
		rateLimited = errors.Is(touchErr, upstream.ErrRateLimited)
	}

	streakCount := 0
	if cached != nil {
		streakCount = cached.Streak
	}

	tok.maturityMu.Lock()
	tok.maturity = &MaturitySnapshot{
		Enabled:             true,
		Target:              7,
		Mode:                "unmetered",
		TouchModel:          model,
		SlotDay:             today,
		LastTouch:           now,
		TouchDay:            today,
		LastAction:          "admit",
		LastResult:          result,
		LastAdvanced:        "yes",
		StreakAtTouch:       streakCount,
		EffectiveTouchModel: model,
		AutoTouchModel:      model,
	}
	tok.maturityMu.Unlock()

	p.emitMaturity(idx, "touch", fmt.Sprintf("admit %s model=%s streak=%d", result, model, streakCount))

	if touchErr != nil {
		if rateLimited {
			p.maturityNoteRateLimit(now)
			return true
		}
		p.logger.Warn("pool: maturity touch failed", "token", idx+1, "err", touchErr)
		return false
	}

	p.asyncStreakFetch(tok)
	return false
}

func (p *Pool) maturityTouchRun(ctx context.Context, tok *tokenEntry, model string, idx int, label string, now time.Time) error {
	instanceID, err := tok.session.EnsureSessionForModel(ctx, model)
	if err != nil {
		return fmt.Errorf("ensure session: %w", err)
	}

	agentID := ""
	if p.reg != nil {
		if a, aerr := p.reg.AgentForModel(model); aerr == nil {
			agentID = a
		}
	}
	if agentID == "" {
		agentID = "base2-free-solar-mini4"
	}

	runID, err := tok.client.StartRun(ctx, agentID)
	if err != nil {
		return fmt.Errorf("start run: %w", err)
	}

	turnErr := p.maturityTouchTurn(ctx, tok, model, agentID, runID, instanceID)
	finStatus := "completed"
	// A failed turn records NO step: the vendor step enum allows only
	// running|completed|skipped (sdk/src/impl/database.ts pendingAgentStepSchema),
	// matching the runtime's own writers (run-agent-step.ts records
	// 'completed', run-programmatic-step.ts 'skipped'), and the CLI's
	// addAgentStep only ever records a real LLM step. The run-level status
	// carries the failure (agent-runtime finishAgentRun 'failed'). The old
	// status:"failed" step 400'd the whole FINISH — reproduced live:
	// "Invalid option: expected one of \"running\"|\"completed\"|\"skipped\"".
	var steps []upstream.RunStep
	if turnErr != nil {
		finStatus = "failed"
	} else {
		steps = []upstream.RunStep{{
			ID:         newTouchStepID(),
			StepNumber: 1,
			Status:     finStatus,
			StartTime:  now.UTC().Format(time.RFC3339Nano),
		}}
	}
	_ = tok.client.FinishRun(ctx, runID, finStatus, len(steps), steps, "")

	if turnErr != nil {
		return fmt.Errorf("touch turn: %w", turnErr)
	}
	return nil
}

// newTouchStepID mints the RFC 4122 v4 step id the FINISH wire schema
// requires: upstream/freebuff sdk/src/impl/database.ts pendingAgentStepSchema
// pins `id: z.string().uuid()`. A non-UUID id fails the whole FINISH with
// 400 {"error":"Invalid request body","details":{"steps":{"0":{"id":
// {"_errors":["Invalid UUID"]}}}}} (reproduced against the live gateway).
// Mirrors runs.newTraceSessionID: a crypto/rand failure falls back to a
// time-seeded hex id rather than panicking mid-touch.
func newTouchStepID() string {
	var b [16]byte
	if _, err := cryptoRand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

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
	defer func() { _ = rc.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 64*1024))
	return nil
}

// MaturityTouchResult represents the outcome of a manual streak touch.
type MaturityTouchResult struct {
	Token     int    `json:"token"`
	Email     string `json:"email"`
	Model     string `json:"model"`
	Status    string `json:"status"` // "touched", "skipped", "error", "rate_limited"
	Reason    string `json:"reason,omitempty"`
	Streak    int    `json:"streak"`
	Timestamp int64  `json:"timestamp"`
}

// ForceMaturityTouch runs streak touch immediately across pooled accounts.
func (p *Pool) ForceMaturityTouch(ctx context.Context, forceAll bool) []MaturityTouchResult {
	toks := p.roster.Load()
	if toks == nil || len(*toks) == 0 {
		return nil
	}
	cfg := p.cfg.Load()
	touchOverride := ""
	if cfg != nil {
		touchOverride = cfg.MaturityTouchModel
	}
	now := time.Now()
	today := pacificDayKey(now)
	var results []MaturityTouchResult

	for i, tok := range *toks {
		res := MaturityTouchResult{
			Token:     i + 1,
			Email:     tok.Email(),
			Timestamp: now.UnixMilli(),
		}
		if s := tok.Streak(); s != nil {
			res.Streak = s.Streak
		}

		// Check locks/quarantine/bans
		if tok.locked.Load() {
			res.Status = "skipped"
			res.Reason = "skip:locked"
			results = append(results, res)
			continue
		}
		rs := tok.runs.Snapshot()
		if rs.BanError != nil && (rs.BannedUntil.IsZero() || now.Before(rs.BannedUntil)) {
			res.Status = "skipped"
			res.Reason = "skip:banned"
			results = append(results, res)
			continue
		}

		// Check if already used today (unless forceAll is true)
		if !forceAll {
			if p.dayRequestCount(i) > 0 {
				res.Status = "skipped"
				res.Reason = "skip:client-active"
				results = append(results, res)
				continue
			}
			if st := tok.Streak(); st != nil && st.TodayUsed {
				res.Status = "skipped"
				res.Reason = "skip:today-used"
				results = append(results, res)
				continue
			}
			m := tok.MaturitySnapshot()
			if m != nil && m.TouchDay == today && !m.LastTouch.IsZero() && m.LastResult == "ok" {
				res.Status = "skipped"
				res.Reason = "skip:already-touched"
				results = append(results, res)
				continue
			}
			if m != nil && !m.LastTouch.IsZero() && now.Sub(m.LastTouch) < maturityThrottle {
				res.Status = "skipped"
				res.Reason = "skip:throttle"
				results = append(results, res)
				continue
			}
		}

		// Resolve model
		model, err := p.maturityResolveModel(tok, touchOverride)
		if err != nil {
			res.Status = "error"
			res.Reason = "resolve_model: " + err.Error()
			results = append(results, res)
			continue
		}
		res.Model = model

		// Execute touch
		p.logger.Info("pool: force maturity touch running", "token", i+1, "model", model)
		touchErr := p.maturityTouchRun(ctx, tok, model, i, tok.Email(), now)
		if touchErr != nil {
			if errors.Is(touchErr, upstream.ErrRateLimited) {
				res.Status = "rate_limited"
				res.Reason = touchErr.Error()
				p.maturityNoteRateLimit(now)
			} else {
				res.Status = "error"
				res.Reason = touchErr.Error()
			}
		} else {
			res.Status = "touched"
			res.Reason = "ok"
			tok.maturityMu.Lock()
			tok.maturity = &MaturitySnapshot{
				Enabled:             true,
				Target:              7,
				Mode:                "unmetered",
				TouchModel:          model,
				SlotDay:             today,
				LastTouch:           now,
				TouchDay:            today,
				LastAction:          "admit",
				LastResult:          "ok",
				LastAdvanced:        "yes",
				EffectiveTouchModel: model,
				AutoTouchModel:      model,
			}
			tok.maturityMu.Unlock()
			p.emitMaturity(i, "touch", fmt.Sprintf("admit ok model=%s", model))
			p.asyncStreakFetch(tok)
		}
		results = append(results, res)

		// Short stagger between accounts (1 second) so we don't burst upstream
		if i < len(*toks)-1 {
			select {
			case <-ctx.Done():
				return results
			case <-time.After(1 * time.Second):
			}
		}
	}
	return results
}
