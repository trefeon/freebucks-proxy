// acquire_route.go - pooled acquire route: Acquire (strict index order via
// spillOrder) plus the smart model-queue walk (arrival scan, work-conserving
// walk, global FIFO park on the first full lane, scale-out) with the legacy
// unlimited walk preserved for SLOTS_PER_ACCOUNT=0.
//
// A pooled Acquire resolves the model's agent, builds the strict index
// order, and takes the smart path: the arrival scan grants instantly on a
// lane with a free slot AND an already-usable session for the model;
// otherwise the walk admits instantly on the first free lane in index
// order (a cold lane creates its session inline — concurrent admissions on
// one lane collapse via single-flight) and parks on the first full lane's
// global FIFO until a Release hands it a slot, its single QUEUE_WAIT
// deadline elapses (then it scales out past the parking lane without
// parking again), or its ctx expires. A same-lane quota requeue (I5)
// sleeps the jail, retries on the held permit, and otherwise rejoins the
// model queue at the head. The end-of-chain precedence (ban > rate-limit >
// waiting-room > spill-exhausted > generic) is unchanged.

package pool

import (
	"context"
	"errors"
	"fmt"
	"freebucks-proxy/backend/internal/config"
	"freebucks-proxy/backend/internal/notify"
	"freebucks-proxy/backend/internal/phasetiming"
	"freebucks-proxy/backend/internal/session"
	"freebucks-proxy/backend/internal/upstream"
	"strings"
	"time"
)

// tagRateLimitModel stamps the requested model on a WALK-LOCAL copy of a
// rate-limit refusal when the upstream body omits it, so the surfaced
// error is self-describing. The *upstream.RateLimitError handed out by the
// session single-flight is SHARED by every waiter parked on the same
// refresh (Acquire documents the sharing above; the manager retains one
// refreshErr pointer for all of them): stamping Model in place races
// concurrent walks (concurrent read+write on one struct). The copy is
// confined to this goroutine; the shared value is never mutated. A refusal
// that already names a model is returned as-is (read-only downstream).
func tagRateLimitModel(rle *upstream.RateLimitError, model string) *upstream.RateLimitError {
	if rle == nil || rle.Model != "" {
		return rle
	}
	tagged := *rle
	tagged.Model = model
	return &tagged
}

// tagLimitedIPModel stamps the requested model on a WALK-LOCAL copy of a
// limited_ip refusal. Same single-flight sharing as tagRateLimitModel: the
// stamp must not land on the shared value. Unlike the rate-limit tag the
// pool always names the requested model, even when the upstream body
// carried one.
func tagLimitedIPModel(lie *upstream.LimitedIpError, model string) *upstream.LimitedIpError {
	if lie == nil {
		return nil
	}
	tagged := *lie
	tagged.Model = model
	return &tagged
}

// formatLogUntil renders a refusal-window expiry for log lines: RFC3339 when
// the window is live, "none" when it is unusable (zero or already past).
func formatLogUntil(t time.Time) string {
	if t.IsZero() {
		return "none"
	}
	return t.Format(time.RFC3339)
}

// rememberModelRateLimit records one lane's admission/run-start rate-limit
// refusal as that model's refusal memory (runs.RememberModelRateLimit), so
// the next same-model walk skips the dead lane without upstream contact.
// The Model is filled on a walk-local copy first: walk errors may be
// single-flight-shared and must never be mutated. The display index is
// resolved live — a dashboard reorder mid-flight must not mislabel the
// log line (same rule as the ban path).
func (p *Pool) rememberModelRateLimit(tok *tokenEntry, model string, rle *upstream.RateLimitError) {
	if tok == nil || rle == nil {
		return
	}
	cp := *rle
	if cp.Model == "" {
		cp.Model = model
	}
	tok.runs.RememberModelRateLimit(model, &cp)
	// RememberModelRateLimit parks only refusals with a live expiry window;
	// an opaque refusal is a no-op there, so say so here too — the Info
	// line must mean the lane will actually be skipped. The predicate
	// mirrors RememberModelRateLimit's (which stays authoritative).
	now := time.Now()
	until := cp.ResetAt
	if cp.RetryAfter > 0 {
		until = now.Add(cp.RetryAfter)
	}
	args := []any{"model", model, "retry_after", cp.RetryAfter, "until", formatLogUntil(until)}
	if !cp.ResetAt.IsZero() {
		args = append(args, "reset", cp.ResetAt.Format(time.RFC3339))
	}
	if li := p.indexOfEntry(tok); li >= 0 {
		args = append([]any{"token", li + 1}, args...)
	} else {
		// Entry left the roster mid-flight: fall back to the
		// non-reversible label so the line still carries attribution.
		args = append([]any{"token", tokenEntryLabel(tok)}, args...)
	}
	if until.IsZero() || !until.After(now) {
		p.logger.Debug("pool: admission rate limit not remembered (no usable expiry)", args...)
		return
	}
	p.logger.Info("pool: admission rate limit remembered", args...)
}

// Acquire resolves the model's agent and walks the strict index order
// until a token yields both a run and a session. Returns a lease on
// success. Registry misses (unknown model) are returned as-is.
func (p *Pool) Acquire(ctx context.Context, model string) (*Lease, error) {
	// Post-drain re-admission gate: once Shutdown starts draining, no new
	// session POST or run START may be admitted — an admission landing
	// after the drain would leak an owned session row upstream.
	if p.draining.Load() {
		return nil, errors.New("pool: shutting down")
	}

	toks := p.roster.Load()
	cfg := p.cfg.Load()
	if len(*toks) == 0 {
		return nil, errors.New("pool: no auth tokens configured")
	}
	agentID, err := p.reg.AgentForModel(model)
	if err != nil {
		return nil, err
	}

	// Single-pin fail-fast (PIN_MODEL): when every slot is pinned away
	// from the requested model, no admission can succeed — surface the
	// routing error without touching upstream at all.
	if allPinnedOut(toks, cfg, p.reg, model) {
		// The ordering/filter stages are bypassed entirely here, so count
		// the skip decision per slot (the counter tracks decisions, not
		// requests — a slot can count twice across filter + failover).
		for _, tok := range *toks {
			tok.pinSkips.Add(1)
		}
		return nil, pinFailFastError(model, len(*toks))
	}

	// Strict index order (spill_order.go): concurrent requests share the
	// per-entry single-flight in the session manager, so no leader gate
	// is needed to prevent duplicate session creates.
	order, quotaLimited := p.spillOrder(toks, model)
	return p.leaseFromOrder(ctx, model, agentID, cfg, toks, order, quotaLimited)
}

// walkState is one Acquire's mutable walk accumulators, threaded through
// the gates, the admission, and the tail precedence so the per-lane helpers
// share the request's buckets without a dozen parameters.
type walkState struct {
	ctx          context.Context
	model        string
	agentID      string
	cfg          *config.Config
	toks         *[]*tokenEntry
	order        []int
	quotaLimited []rateLimitEntry
	errs         []string
	waiting      []*session.WaitingRoomError
	rateLimited  []rateLimitEntry
	banned       []*upstream.BanError
	spill        *spillChain
	// queueWait accumulates every park duration across the walk: a waiter
	// that parked on the model queue then granted reports the full wait,
	// not just the granting lane's. Recorded only when the request
	// actually parked somewhere; a waiter that timed out or was
	// cancelled held no slot and reports nothing on that lane.
	queueWait time.Duration
	// Terminal-cooldown hints (pool_state pool/cooldown/*, hint only): a
	// fresh hint skips one doomed probe when another ordered token can
	// serve. When every ordered token is hinted the walk ignores hints and
	// attempts upstream live — a hint alone never fails Acquire.
	skipHinted bool
}

// laneOutcome is what one lane attempt asks its caller to do next.
type laneOutcome int

const (
	// laneGranted: admitOnLane returns a lease.
	laneGranted laneOutcome = iota
	// laneNext: try the next lane (the walk's old `continue`).
	laneNext
	// laneBreak: spill budget spent (the walk's old `break`) — tail decides.
	laneBreak
	// laneFail: return err directly (correlative refusals, ctx expiry).
	laneFail
	// laneRetry: an I5 requeue holds a permit for carry's lane — re-run
	// the gates and the admission there (the walk's old `oi--` retry,
	// without the old double slot take that leaked one live count per
	// cycle and without the run path's stray rewind to the previous
	// lane: the retry runs where its permit was taken).
	laneRetry
)

// laneResult is one admitOnLane attempt's answer.
type laneResult struct {
	outcome laneOutcome
	lease   *Lease
	err     error
	carry   *laneCarry
}

// leaseFromOrder runs the token spill walk against the given order.
// Extracted from Acquire so tests can drive the walk with an explicit
// order without duplicating the loop.
func (p *Pool) leaseFromOrder(ctx context.Context, model string, agentID string, cfg *config.Config, toks *[]*tokenEntry, order []int, quotaLimited []rateLimitEntry) (*Lease, error) {
	ws := &walkState{
		ctx:          ctx,
		model:        model,
		agentID:      agentID,
		cfg:          cfg,
		toks:         toks,
		order:        order,
		quotaLimited: quotaLimited,
		spill:        newSpillChain(cfg, model),
	}
	// MASQ spill walk (spill_queue.go): lane-exhausted lanes move the
	// request to the next account silently, bounded by
	// MAX_SPILL_ACCOUNTS; the last signal surfaces end-of-chain below.
	ws.skipHinted = p.cooldownHintSkippable(toks, order, time.Now())
	slotCap, slotDepth, slotWait := slotParams(cfg)
	// SLOTS_PER_ACCOUNT=0 skips slot gating entirely: no counter, no
	// queue — the legacy lane loop below, with the upstream quota/429 as
	// the only brake.
	if slotCap <= 0 {
		return p.legacyLoop(ws)
	}
	return p.smartAcquire(ws, slotCap, slotDepth, slotWait)
}

// walkGates runs one lane's eligibility gates with the walk's full
// recording: pin skips count, quarantine/ban/cooldown/rate-limit refusals
// land in the request's buckets and error strings. Reports whether the lane
// is skipped. The slot take and the admission stay with the caller.
func (p *Pool) walkGates(ws *walkState, idx int, tok *tokenEntry) (skip bool) {
	cfg := ws.cfg
	model := ws.model
	// Administratively locked tokens are never eligible for leasing.
	if tok.locked.Load() {
		return true
	}
	name := fmt.Sprintf("token-%d", idx+1)
	// Terminal-cooldown hint: skip one doomed probe (see walkState). Live
	// cooldown/ban memory below stays authoritative; the hint only
	// covers what a restart forgot.
	if ws.skipHinted && tok != nil && tok.token != "" && p.cooldownHintFresh(poolTokenHash(tok.token), time.Now()) {
		ws.errs = append(ws.errs, fmt.Sprintf("%s: terminal cooldown hint fresh, skipping one probe", name))
		p.logger.Debug("pool: token skipped (cooldown hint)", "token", idx+1, "model", model, "reason", "terminal cooldown hint fresh")
		return true
	}
	// Quarantined tokens (terminal account state: a live ban) are
	// permanently skipped — the pool never revives a dead account, so
	// they are never re-admitted. Their
	// remembered terminal error still feeds the failover buckets so a
	// fully-quarantined pool surfaces the right 403/401 instead of a
	// generic 502.
	// Lift-aware quarantine: a temporary ban's marker expires at its
	// resumes_at (the ban auto-lifts upstream), so a lifted token falls
	// through to the normal eligibility checks instead of staying
	// excluded forever.
	if q := tok.quarantine.Load(); q != nil && !p.clearLiftedQuarantine(tok) {
		ws.errs = append(ws.errs, fmt.Sprintf("%s: quarantined (%s: %s)", name, q.reason, q.detail))
		p.logger.Debug("pool: token skipped (quarantined)", "token", idx+1, "model", model, "state", q.reason, "reason", q.detail)
		switch terr := q.err.(type) {
		case *upstream.BanError:
			dup := false
			for _, existing := range ws.banned {
				if existing.Error() == terr.Error() {
					dup = true
					break
				}
			}
			if !dup {
				ws.banned = append(ws.banned, terr)
			}
		}
		return true
	}
	// Single-pin routing (PIN_MODEL): a slot pinned to another model
	// is skipped before any session/run contact, so a request never
	// burns the wrong account's quota or churns its session. Sits
	// after the quarantine gate so terminal states keep their
	// error-bucket precedence, and mirrors the eligible() filter for
	// custom-order callers.
	if pinnedOut(cfg, p.reg, idx, model) {
		tok.pinSkips.Add(1)
		ws.errs = append(ws.errs, fmt.Sprintf("%s: model %q not pinned to this slot", name, model))
		p.logger.Debug("pool: token skipped (model pin)", "token", idx+1, "model", model, "reason", "not pinned to this slot")
		return true
	}
	// Freebucks balance cap: skip tokens whose Freebucks allowance is exhausted
	// for this model, rather than attempting a doomed session admission.
	if capped, retryAfter := freebucksCapped(tok, model); capped {
		ws.rateLimited = appendRateLimitEntry(ws.rateLimited, freebucksLimitError(tok, model), idx)
		ws.errs = append(ws.errs, fmt.Sprintf("%s: freebucks balance exhausted for model %q (retry in %v)", name, model, retryAfter.Round(time.Second)))
		p.logger.Debug("pool: token skipped (freebucks capped)", "token", idx+1, "model", model, "retry_after", retryAfter, "reason", "freebucks balance exhausted")
		return true
	}

	if until := tok.runs.CooldownUntil(); time.Now().Before(until) || tok.runs.BanError() != nil {
		// Issue #155: if the cooldown was caused by a specific model's quota exhaustion,
		// and we are requesting a different model (e.g. fallback to mimo-v2.5),
		// do not block this token from serving the requested model.
		if canServeOtherModel(tok.runs.RateLimitError(), model) {
			// Token is only quota-capped for the remembered model, but can still serve `model`.
		} else {
			skipReason := "cooldown"
			if tok.runs.BanError() != nil {
				skipReason = "banned"
			} else if tok.runs.RateLimitError() != nil {
				skipReason = "rate_limited"
			}
			ws.errs = append(ws.errs, fmt.Sprintf("%s: cooling down until %s", name, until.Format(time.RFC3339)))
			p.logger.Debug("pool: token skipped (cooldown)", "token", idx+1, "until", formatLogUntil(until), "reason", skipReason)
			if be := tok.runs.BanError(); be != nil {
				dup := false
				for _, existing := range ws.banned {
					if existing.Error() == be.Error() {
						dup = true
						break
					}
				}
				if !dup {
					ws.banned = append(ws.banned, be)
				}
			}
			if rle := tok.runs.RateLimitError(); rle != nil {
				ws.rateLimited = appendRateLimitEntry(ws.rateLimited, rle, idx)
			}
			return true
		}
	}
	// Remembered per-model refusal (runs.RememberModelRateLimit): a
	// previous walk's admission/run-start 429 for THIS model is still
	// inside its window, so the lane is skipped with no upstream
	// contact and no slot taken — the bucket + errs records stay
	// truthful via the remembered refusal. Other models fall through
	// (per-model memory, never a blanket park); an expired window
	// reads nil and re-attempts live.
	if mrle := tok.runs.ModelRateLimit(model); mrle != nil {
		tagged := *mrle
		if tagged.Model == "" {
			tagged.Model = model
		}
		ws.rateLimited = appendRateLimitEntry(ws.rateLimited, &tagged, idx)
		ws.errs = append(ws.errs, name+": "+tagged.Error()+" (remembered, no upstream contact)")
		mrUntil := tagged.ResetAt
		if tagged.RetryAfter > 0 {
			mrUntil = time.Now().Add(tagged.RetryAfter)
		}
		p.logger.Debug("pool: token skipped (remembered model rate limit)", "token", idx+1, "model", model, "retry_after", tagged.RetryAfter, "until", formatLogUntil(mrUntil))
		return true
	}
	return false
}

// scanWarmFree is the arrival scan (contract rule 2): the first lane in
// strict index order that passes the eligibility gates, holds an
// already-usable session for the model (cached Snapshot read, no network
// I/O), and has a free slot grants instantly. The scan records nothing —
// gate skips are re-evaluated with full recording wherever the request goes
// next — so a scan hit that grants leaves the same records as the legacy
// walk, and a miss parks silently like a spill.
func (p *Pool) scanWarmFree(ws *walkState, cap int) (tok *tokenEntry, idx int, permit *slotPermit) {
	for _, i := range ws.order {
		if err := ws.ctx.Err(); err != nil {
			return nil, -1, nil
		}
		if i < 0 || i >= len(*ws.toks) {
			continue
		}
		t := (*ws.toks)[i]
		now := time.Now()
		if !p.laneAdmissible(t, i, ws.model, ws.cfg, now, ws.skipHinted) {
			continue
		}
		if !sessionUsableForModel(t, ws.model) {
			continue
		}
		if permit, _, ok := p.slotTry(slotKey{entry: t, model: ws.model}, cap); ok {
			return t, i, permit
		}
	}
	return nil, -1, nil
}

// posInOrder locates idx in the walk order for failover resume; a vanished
// index (roster swap mid-flight) reports -1 so the resume restarts at the
// head.
func posInOrder(ws *walkState, idx int) int {
	for pos, i := range ws.order {
		if i == idx {
			return pos
		}
	}
	return -1
}

// copyQueueSignal carries a park entry signal onto the current lane for the
// spill budget: the reason (and its client wording) is preserved, the scope
// names the lane that actually stayed full.
func copyQueueSignal(src *slotQueueExhaustedError, idx, cap, live int) *slotQueueExhaustedError {
	reason := "timeout"
	wait := time.Duration(0)
	if src != nil {
		reason = src.Reason
		wait = src.Wait
	}
	return &slotQueueExhaustedError{Reason: reason, Token: idx + 1, Cap: cap, Live: live, Wait: wait}
}

// smartAcquire is the pooled smart path: arrival scan, else an index-order
// walk that admits instantly on the first free lane (warm or cold) and
// parks on the first full one, else spill-style scale-out after the park.
func (p *Pool) smartAcquire(ws *walkState, cap, depth int, wait time.Duration) (*Lease, error) {
	// Arrival scan: a free slot plus an already-usable session grants
	// instantly without even running the walk gates.
	if tok, idx, permit := p.scanWarmFree(ws, cap); permit != nil {
		switch res := p.admitOnLane(ws, idx, tok, permit); res.outcome {
		case laneGranted:
			return res.lease, nil
		case laneFail:
			return nil, res.err
		case laneBreak:
			return p.walkTail(ws)
		case laneRetry:
			return p.smartRetry(ws, res.carry, cap, depth, wait)
		default: // laneNext: fail over past the scan lane, same discipline.
			return p.scaleoutFrom(ws, posInOrder(ws, idx)+1, cap, depth, wait, nil, true)
		}
	}
	// Miss: walk in index order — the first free lane admits instantly (a
	// cold lane creates its session inline; concurrent admissions on one
	// lane collapse via single-flight), the first full lane parks the
	// caller on the global FIFO. Walkers stick to the admitting lane
	// instead of spreading, so a burst converges onto one lane and extra
	// accounts stay cold; an all-gated walk surfaces at the tail without
	// parking.
	return p.scaleoutFrom(ws, 0, cap, depth, wait, nil, true)
}

// smartRetry runs an I5 requeue carry: the permit is already held, so the
// lane's gates re-run fresh (the jail may have changed state) and the
// admission runs without another take. A gate that now fails fails over
// past the lane.
func (p *Pool) smartRetry(ws *walkState, carry *laneCarry, cap, depth int, wait time.Duration) (*Lease, error) {
	if carry == nil || carry.tok == nil || carry.permit == nil {
		return p.scaleoutFrom(ws, 0, cap, depth, wait, nil, true)
	}
	if err := ws.ctx.Err(); err != nil {
		carry.permit.Release()
		return nil, err
	}
	if cur := p.roster.Load(); carry.idx < 0 || carry.idx >= len(*cur) || (*cur)[carry.idx] != carry.tok {
		carry.permit.Release()
		return p.scaleoutFrom(ws, 0, cap, depth, wait, nil, true)
	}
	if p.walkGates(ws, carry.idx, carry.tok) {
		carry.permit.Release()
		return p.scaleoutFrom(ws, posInOrder(ws, carry.idx)+1, cap, depth, wait, nil, true)
	}
	switch res := p.admitOnLane(ws, carry.idx, carry.tok, carry.permit); res.outcome {
	case laneGranted:
		return res.lease, nil
	case laneFail:
		return nil, res.err
	case laneBreak:
		return p.walkTail(ws)
	case laneRetry:
		return p.smartRetry(ws, res.carry, cap, depth, wait)
	default: // laneNext: fail over past the carry lane, same discipline.
		return p.scaleoutFrom(ws, posInOrder(ws, carry.idx)+1, cap, depth, wait, nil, true)
	}
}

// scaleoutFrom walks lanes in index order from start attempting slot-try +
// EnsureSessionForModel + run. park selects the full-lane discipline: a
// fresh attempt (arrival miss, scan-lane failover, post-jail retry) PARKS
// on the first full lane — walkers stick to the admitting lane instead of
// spreading, so a burst converges onto one lane's single-flight admission
// and extra accounts stay cold. A waiter that already paid a full
// QUEUE_WAIT park (park=false) spills past full lanes and surfaces at the
// tail instead of waiting twice. entryQerr is the park entry signal whose
// reason the budget copies preserve (nil on a fresh failover, where full
// lanes note a synthetic timeout like a lane that stayed full past its own
// wait). Refusals land in the request's buckets; the tail precedence
// surfaces them. Concurrent admissions on one lane collapse via the session
// manager's existing single-flight.
func (p *Pool) scaleoutFrom(ws *walkState, start, cap, depth int, wait time.Duration, entryQerr *slotQueueExhaustedError, park bool) (*Lease, error) {
	for pos := start; pos < len(ws.order); pos++ {
		idx := ws.order[pos]
		if err := ws.ctx.Err(); err != nil {
			return nil, err
		}
		// Defensive bounds check: spillOrder builds its order against the
		// SAME snapshot loaded above, but a removal racing this call must
		// never index past the slice it computed the order from. Skip
		// indices that are no longer present instead of panicking.
		if idx < 0 || idx >= len(*ws.toks) {
			continue
		}
		tok := (*ws.toks)[idx]
		if p.walkGates(ws, idx, tok) {
			continue
		}
		permit, live, ok := p.slotTry(slotKey{entry: tok, model: ws.model}, cap)
		if !ok {
			if park {
				return p.parkOnFullLane(ws, pos, cap, depth, wait)
			}
			// A full lane is not an upstream refusal, so it writes no
			// bucket and no error string — the spill stays silent. The
			// budget copy keeps the entry reason for the end-of-chain
			// surface.
			if !ws.spill.note(copyQueueSignal(entryQerr, idx, cap, live), cap) {
				return p.walkTail(ws)
			}
			continue
		}
		switch res := p.admitOnLane(ws, idx, tok, permit); res.outcome {
		case laneGranted:
			return res.lease, nil
		case laneFail:
			return nil, res.err
		case laneBreak:
			return p.walkTail(ws)
		case laneRetry:
			return p.smartRetry(ws, res.carry, cap, depth, wait)
		default: // laneNext: fail over past this lane, same park discipline.
			return p.scaleoutFrom(ws, pos+1, cap, depth, wait, nil, park)
		}
	}
	return p.walkTail(ws)
}

// parkOnFullLane parks the caller on the model's global FIFO at the first
// full lane the walk meets (tail join; the depth cap fails over at once,
// unchanged). A handoff grant admits on the granting lane; a failover past
// it resumes the walk with the same park discipline. A park timeout (or an
// exhausted spill budget) scales out past the parking lane WITHOUT
// parking again (park=false) — the single QUEUE_WAIT deadline is never
// paid twice — and all-gated walks never reach here.
func (p *Pool) parkOnFullLane(ws *walkState, pos, cap, depth int, wait time.Duration) (*Lease, error) {
	parkStart := time.Now()
	permit, lane, idx, parked, err := p.modelPark(ws.ctx, ws.model, cap, depth, wait, false)
	if err != nil {
		if qerr, ok := err.(*slotQueueExhaustedError); ok {
			// Trust the signal's carried wait: the queue's own timer
			// elapsed, so the park is the configured wait — not the
			// wall clock around the call.
			if qerr.Reason == "timeout" {
				ws.queueWait += qerr.Wait
			}
			if ws.spill.note(qerr, cap) {
				return p.scaleoutFrom(ws, pos+1, cap, depth, wait, qerr, false)
			}
			return p.walkTail(ws)
		}
		return nil, err
	}
	// Woken with a handoff permit: the queue granted before the deadline.
	// The accumulated park rides the request's phase accumulator (the
	// server puts it on the chat trace) and the lease below. Never
	// recorded for a request that did not park.
	if parked {
		ws.queueWait += time.Since(parkStart)
		phasetiming.FromContext(ws.ctx).Since(phasetiming.QueueWaitMS, time.Now().Add(-ws.queueWait))
	}
	switch res := p.admitOnLane(ws, idx, lane, permit); res.outcome {
	case laneGranted:
		return res.lease, nil
	case laneFail:
		return nil, res.err
	case laneBreak:
		return p.walkTail(ws)
	case laneRetry:
		return p.smartRetry(ws, res.carry, cap, depth, wait)
	default: // laneNext: fail over past the handoff lane WITHOUT parking
		// again (park=false) — the waiter already paid its park to earn this
		// grant; a fresh full wait would stack two QUEUE_WAITs.
		return p.scaleoutFrom(ws, posInOrder(ws, idx)+1, cap, depth, wait, nil, false)
	}
}

// modelRequeue parks the caller for a same-lane quota requeue (I5): it
// sleeps until notBefore (the quota jail expiry) with the lane's live-turn
// slot HELD, then retries the lane on the held permit — no retake, so no
// fresh arrival can steal the lane mid-jail (its arrival scan misses for
// want of a usable session, and its park waits for a Release). It never
// touches the spill chain (waiting out a quota window is not a spill hop)
// and writes no cooldown: the upstream RetryAfter is the only clock. The
// caller's ctx bounds the whole wait. It reports whether the caller waited
// at all (jail sleep or queue park) for queue-wait telemetry, and the held
// permit for the retry to consume without another take.
//
// When the entry vanished mid-sleep (roster removal) the orphan permit is
// dropped and the caller falls back to the immediate-grant scan (rule 2)
// and a head rejoin of the model queue (it already held a slot once, so
// it outranks fresh arrivals). A handoff granted while parked supersedes
// the held slot, which is released.
func (p *Pool) modelRequeue(ws *walkState, cap, depth int, wait time.Duration, notBefore time.Time, tok *tokenEntry, idx int, held *slotPermit) (carry *laneCarry, parked bool, err error) {
	slept := false
	if delay := time.Until(notBefore); delay > 0 {
		slept = true
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ws.ctx.Done():
			if held != nil {
				held.Release()
			}
			return nil, true, ws.ctx.Err()
		case <-timer.C:
		}
	}
	// Same-lane continuity: the permit stayed held across the sleep, so
	// the retry consumes it directly — no retake, no steal window. The
	// retry admission re-runs the gates, so a lane that turned ineligible
	// while the jail slept still fails over past it (the carry is
	// released on that path).
	if cur := p.roster.Load(); idx >= 0 && idx < len(*cur) && (*cur)[idx] == tok {
		return &laneCarry{tok: tok, idx: idx, permit: held}, slept, nil
	}
	// The entry is gone: drop the orphan permit, then scan + head rejoin.
	if held != nil {
		held.Release()
	}
	if tok, idx, permit := p.scanWarmFree(ws, cap); permit != nil {
		return &laneCarry{tok: tok, idx: idx, permit: permit}, slept, nil
	}
	permit, lane, idx, _, err := p.modelPark(ws.ctx, ws.model, cap, depth, wait, true)
	if err != nil {
		return nil, true, err
	}
	return &laneCarry{tok: lane, idx: idx, permit: permit}, true, nil
}

// admitOnLane runs one lane's session admission and run acquisition against
// an already-held live-turn slot and either grants a lease or reports how
// the caller resumes. The slot is taken BEFORE any upstream admission so a
// queued request never burns a session slot or run START while it waits. A
// lane-exhausted waiter scales out silently to the next lane — a full lane
// is not an upstream refusal, so it writes no bucket and no error string.
// The caller's own ctx expiry returns as-is.
func (p *Pool) admitOnLane(ws *walkState, idx int, tok *tokenEntry, routeSlot *slotPermit) laneResult {
	ctx := ws.ctx
	model := ws.model
	agentID := ws.agentID
	cfg := ws.cfg
	name := fmt.Sprintf("token-%d", idx+1)

	// Session admission: the live-turn slot above is the only local
	// concurrency bound here.

	// Re-validate the entry is still current BEFORE the admission POST:
	// a concurrent RemoveLastToken/RemoveAllTokens must never admit a
	// NEW session for an entry the pool no longer owns — a drained
	// entry's freshly-created session would leak upstream. The
	// post-admission check below stays: the removal can still land
	// during the create.
	if cur := p.roster.Load(); idx < 0 || idx >= len(*cur) || (*cur)[idx] != tok {
		routeSlot.Release()
		return laneResult{outcome: laneNext}
	}
	sessionStart := time.Now()
	// Issue #94(b): WAITING_ROOM_CHAIN gate — when the upstream last
	// refused this token with 428 waiting_room_required, fire the
	// reference pre-session ad-chain + streak flow (best-effort, bounded
	// by the client's own chain timeout) before the next session create
	// so the admission does not bounce off the same 428 again.
	if cfg.WaitingRoomChain && tok.client.ConsumeWaitingRoomChain() {
		p.logger.Debug("pool: firing waiting-room pre-session chain", "token", idx+1, "model", model)
		tok.client.FireWaitingRoomChain(ctx)
	}
	instanceID, err := tok.session.EnsureSessionForModel(ctx, model)
	p.markPersistDirty()
	phasetiming.FromContext(ctx).Since(phasetiming.SessionRefreshMS, sessionStart)
	if err != nil {
		c := p.classifyAndCooldown(tok.runs, err)
		if c.authRejected {
			// 401 invalid is a per-account credential refusal, never a
			// terminal quarantine: other accounts may still serve, so the
			// loop continues with no cooldown write.
			p.logger.Debug("pool: token auth rejected, continuing failover", "token", idx+1, "model", model, "err", err)
		}
		var wr *session.WaitingRoomError
		if errors.As(err, &wr) {
			ws.waiting = append(ws.waiting, wr)
		}
		if rle := c.rateLimited; rle != nil && !c.authRejected && c.banned == nil && c.ipCapped == nil && c.countryBlocked == nil && c.limitedIp == nil {
			// MASQ same-lane quota requeue (I5, spill_queue.go): a
			// short-window quota jail is waited out on this lane -
			// no cooldown write, no failover, no spill hop. Longer
			// (or windowless) windows fall through to the per-lane
			// record below.
			if sCap, sDepth, sWait := slotParams(cfg); sCap > 0 {
				if notBefore, ok := quotaRequeueNotBefore(rle, sWait); ok {
					// No Model tag here: the refusal is discarded after
					// the requeue (only RetryAfter is read above), and
					// rle is single-flight-shared — stamping it would
					// mutate state owned by every parked waiter.
					// Issue #122: count spend_limited on the ledger.
					if c.spendLimited {
						tok.ledger.recordSpendLimited()
					}
					// The lane's live-turn slot stays HELD across the
					// jail sleep below: a parked fresh arrival must not
					// steal the lane mid-jail (its scan misses — no
					// usable session — and its park waits for a Release
					// that only lands here), so the same-lane retry
					// below cannot lose the lane and fail over.
					p.logger.Debug("pool: quota requeue same lane", "token", idx+1, "model", model, "retry_after", rle.RetryAfter, "ms", time.Since(sessionStart).Milliseconds())
					rqStart := time.Now()
					carry, parked, rerr := p.modelRequeue(ws, sCap, sDepth, sWait, notBefore, tok, idx, routeSlot)
					if rerr != nil {
						if qerr, ok := rerr.(*slotQueueExhaustedError); ok {
							if qerr.Reason == "timeout" {
								ws.queueWait += qerr.Wait
							}
							if ws.spill.note(qerr, sCap) {
								p.logger.Debug("pool: token lane spilled", "token", idx+1, "model", model, "err", rerr)
								return laneResult{outcome: laneNext}
							}
							return laneResult{outcome: laneBreak}
						}
						return laneResult{outcome: laneFail, err: rerr}
					}
					if parked {
						ws.queueWait += time.Since(rqStart)
						phasetiming.FromContext(ctx).Since(phasetiming.QueueWaitMS, time.Now().Add(-ws.queueWait))
					}
					return laneResult{outcome: laneRetry, carry: carry}
				}
			}
		}
		if rle := c.rateLimited; rle != nil {
			// Issue #178: tag the refusal with the requested model when
			// the upstream body omits it, so the surfaced refusal is
			// self-describing. Tag a walk-local copy: rle is
			// single-flight-shared and concurrent walks must not
			// mutate it (data race on Model).
			rle = tagRateLimitModel(rle, model)
			ws.rateLimited = appendRateLimitEntry(ws.rateLimited, rle, idx)
			// Remember the lane's refusal for this model AFTER the
			// same-lane requeue above: a waited-out short jail stays
			// memory-free (keeper TestNatural429RequeuesNoParkNoFailover),
			// while a long window parks the lane contact-free until it
			// resets. Opaque refusals (no expiry) never stick.
			p.rememberModelRateLimit(tok, model, rle)
			// Issue #122: the fresh-admission spend ceiling is the
			// upstream's primary spend gate, so an admission-path
			// spend_limited counts on the ledger too (same counter as
			// the chat-path refusal in CooldownTokenRateLimit).
			if c.spendLimited {
				tok.ledger.recordSpendLimited()
			}
		}
		if ice := c.ipCapped; ice != nil {
			// Correlative refusal (one egress IP share): failover would
			// hit the same wall on the next account, so surface directly.
			routeSlot.Release()
			return laneResult{outcome: laneFail, err: ice}
		}
		if be := c.banned; be != nil {
			// Display index resolved live (see run-path below).
			if li := p.indexOfEntry(tok); li >= 0 {
				p.notifyBan(li+1, model)
			}
			// CooldownBan (hard, or a future resumes_at): an expired
			// temporary ban is already lifted upstream and must not
			// mark the token terminal.
			if tok.runs.BanError() != nil {
				p.quarantineToken(tok, "banned", err)
			}
			// Terminal-hint mirror (hint only): a live ban persists so a
			// restart skips one doomed probe; an already-lifted one
			// clears instead. 429/ip_capped/limited_ip write nothing.
			p.storeBanHint(tok)
			ws.banned = appendBan(ws.banned, be)
		}
		if cbe := c.countryBlocked; cbe != nil {
			// Correlative refusal like ip_capped: surface directly. The
			// hint only skips one future probe; the walk never fails over.
			p.storeCountryHint(tok, time.Now().Add(p.countryBlockWindow()))
			routeSlot.Release()
			return laneResult{outcome: laneFail, err: cbe}
		}
		if lie := c.limitedIp; lie != nil {
			// The egress IP cannot serve this model. The session row is
			// fine — it stays bound to its admitted model — so nothing
			// is invalidated. Surface a walk-local Model-stamped copy
			// (never mutate the single-flight-shared value), no
			// failover walk.
			routeSlot.Release()
			return laneResult{outcome: laneFail, err: tagLimitedIPModel(lie, model)}
		}
		ws.errs = append(ws.errs, fmt.Sprintf("%s: %v", name, err))
		routeSlot.Release()
		return laneResult{outcome: laneNext}
	}
	tok.runs.ClearCooldowns()
	// Live admission proves the account healthy: drop any terminal hint
	// a previous window left behind.
	p.clearCooldownHintFor(tok)

	// Re-validate the token is still current: a concurrent
	// RemoveLastToken may have swapped the snapshot out from under a
	// concurrent Acquire while the session admission above was in flight.
	// Leasing a removed token would strand its run's inflight —
	// LeaseRelease always releases through the lease's own entry, but the
	// run would belong to a drained, retiring manager — so skip instead
	// (the removal path drains the retired entry once it observes the
	// slip).
	if cur := p.roster.Load(); idx < 0 || idx >= len(*cur) || (*cur)[idx] != tok {
		routeSlot.Release()
		return laneResult{outcome: laneNext}
	}
	ss := tok.session.Snapshot()
	effectiveModel := model
	effectiveAgentID := agentID
	if ss.Model != "" && ss.Model != model {
		effectiveModel = ss.Model
		if p.reg != nil {
			if resolvedAgent, aerr := p.reg.AgentForModel(effectiveModel); aerr == nil {
				effectiveAgentID = resolvedAgent
			}
		}
	}

	// Issue #90a: pre-create the run at session admission (best-effort)
	// so the first chat on a freshly-admitted session does not pay the
	// START latency. When a run already exists this is a cheap no-op;
	// when the START fails here the Acquire below retries and surfaces
	// the real error through the normal failover path.
	_ = tok.runs.Precreate(ctx, effectiveAgentID)
	runStart := time.Now()
	run, err := tok.runs.Acquire(ctx, effectiveAgentID)
	phasetiming.FromContext(ctx).Since(phasetiming.RunAcquireMS, runStart)
	if err != nil {
		c := p.classifyAndCooldown(tok.runs, err)
		if c.authRejected {
			// 401 invalid is a per-account credential refusal, never a
			// terminal quarantine: other accounts may still serve, so the
			// loop continues with no cooldown write.
			p.logger.Debug("pool: token auth rejected, continuing failover", "token", idx+1, "model", model, "err", err)
		}
		var wr *session.WaitingRoomError
		if errors.As(err, &wr) {
			ws.waiting = append(ws.waiting, wr)
		}
		if rle := c.rateLimited; rle != nil && !c.authRejected && c.banned == nil && c.ipCapped == nil && c.countryBlocked == nil && c.limitedIp == nil {
			if sCap, sDepth, sWait := slotParams(cfg); sCap > 0 {
				if notBefore, ok := quotaRequeueNotBefore(rle, sWait); ok {
					// No Model tag here (see the admission path): the
					// refusal is discarded after the requeue, and rle is
					// single-flight-shared.
					if c.spendLimited {
						tok.ledger.recordSpendLimited()
					}
					// The lane's slot stays HELD across the jail sleep
					// (see the admission path): no fresh arrival can
					// steal the lane mid-jail.
					p.logger.Debug("pool: quota requeue same lane", "token", idx+1, "model", model, "retry_after", rle.RetryAfter, "phase", "run-start", "ms", time.Since(runStart).Milliseconds())
					rqStart := time.Now()
					carry, parked, rerr := p.modelRequeue(ws, sCap, sDepth, sWait, notBefore, tok, idx, routeSlot)
					if rerr != nil {
						if qerr, ok := rerr.(*slotQueueExhaustedError); ok {
							if qerr.Reason == "timeout" {
								ws.queueWait += qerr.Wait
							}
							if ws.spill.note(qerr, sCap) {
								p.logger.Debug("pool: token lane spilled", "token", idx+1, "model", model, "err", rerr)
								return laneResult{outcome: laneNext}
							}
							return laneResult{outcome: laneBreak}
						}
						return laneResult{outcome: laneFail, err: rerr}
					}
					if parked {
						ws.queueWait += time.Since(rqStart)
						phasetiming.FromContext(ctx).Since(phasetiming.QueueWaitMS, time.Now().Add(-ws.queueWait))
					}
					return laneResult{outcome: laneRetry, carry: carry}
				}
			}
		}
		if rle := c.rateLimited; rle != nil {
			// Issue #178: tag the refusal with the requested model when
			// the upstream body omits it, so the surfaced refusal is
			// self-describing. Tag a walk-local copy — see the
			// admission path (single-flight-shared values must never
			// be mutated by concurrent walks).
			rle = tagRateLimitModel(rle, model)
			ws.rateLimited = appendRateLimitEntry(ws.rateLimited, rle, idx)
			// Remember the lane's refusal for this model AFTER the
			// same-lane requeue above (see the admission path): a
			// waited-out short jail stays memory-free, while a long
			// window parks the lane contact-free until it resets.
			p.rememberModelRateLimit(tok, model, rle)
			if c.spendLimited {
				tok.ledger.recordSpendLimited()
			}
		}
		if ice := c.ipCapped; ice != nil {
			// Correlative refusal (one egress IP share): surface directly.
			routeSlot.Release()
			return laneResult{outcome: laneFail, err: ice}
		}
		if be := c.banned; be != nil {
			// Display index resolved live: a dashboard reorder
			// mid-flight must not mislabel the ban alert.
			if li := p.indexOfEntry(tok); li >= 0 {
				p.notifyBan(li+1, model)
			}
			// CooldownBan (hard, or a future resumes_at): an expired
			// temporary ban is already lifted upstream and must not
			// mark the token terminal.
			if tok.runs.BanError() != nil {
				p.quarantineToken(tok, "banned", err)
			}
			// Terminal-hint mirror (hint only — see the admission path).
			p.storeBanHint(tok)
			ws.banned = appendBan(ws.banned, be)
		}
		if cbe := c.countryBlocked; cbe != nil {
			// Correlative refusal like ip_capped: surface directly.
			p.storeCountryHint(tok, time.Now().Add(p.countryBlockWindow()))
			routeSlot.Release()
			return laneResult{outcome: laneFail, err: cbe}
		}
		ws.errs = append(ws.errs, fmt.Sprintf("%s: %v", name, err))
		routeSlot.Release()
		return laneResult{outcome: laneNext}
	}
	leaseAttrs := []any{
		"token", idx + 1, "model", effectiveModel, "agent", effectiveAgentID, "instance_id", instanceID,
		"country", ss.CountryCode, "ms", time.Since(sessionStart).Milliseconds(),
	}
	if ws.queueWait > 0 {
		// Queue-wait telemetry: this admission parked in the model's FIFO
		// live-turn queue before a slot was granted. Before this line
		// existed, a granted park was invisible — only the
		// timeout/exhausted path logged anything at all.
		leaseAttrs = append(leaseAttrs, "queue_wait_ms", ws.queueWait.Milliseconds(), "queue_parked", true)
	}
	p.logger.Debug("pool: lease acquired", leaseAttrs...)
	if cur := p.roster.Load(); idx < 0 || idx >= len(*cur) || (*cur)[idx] != tok {
		tok.runs.Release(run)
		routeSlot.Release()
		return laneResult{outcome: laneNext}
	}
	lease := &Lease{
		Token: idx, Model: effectiveModel, AgentID: effectiveAgentID, Run: run, SessionInstanceID: instanceID,
		entry: tok, routeSlot: routeSlot, QueueWait: ws.queueWait, AcquiredAt: time.Now(),
	}
	// MASQ precious (precious.go): the account served this model, so its
	// live session is never proactively dropped from here on.
	p.markPrecious(tok, effectiveModel)
	// Smart probe (smart_probe.go): the account served traffic, so its
	// cached quota earns a refresh once the guards pass.
	p.markProbeDirty(tok)
	// Track the activity and end any idle-maintenance pause: the next
	// maintain tick resumes rotation/refresh work.
	p.lastActiveMu.Lock()
	p.lastActive = time.Now()
	p.idleFinished = false
	p.lastActiveMu.Unlock()
	return laneResult{outcome: laneGranted, lease: lease}
}

// legacyLoop is the pre-smart lane walk, kept verbatim for
// SLOTS_PER_ACCOUNT=0 (unlimited): no counter, no queue — the upstream
// quota/429 is the brake. The per-lane gates, admission, run acquisition,
// I5 requeue (dead when uncapped — quotaRequeueNotBefore requires a
// positive cap), and bucket precedence are shared with the smart path via
// walkGates/admitOnLane.
func (p *Pool) legacyLoop(ws *walkState) (*Lease, error) {
	// Indexed (not range): a same-lane quota requeue (I5) rewinds the
	// walk to retry the lane after its RetryAfter elapses — no spill hop
	// consumed, no failover. Every other path advances normally.
	for oi := 0; oi < len(ws.order); oi++ {
		idx := ws.order[oi]
		if err := ws.ctx.Err(); err != nil {
			return nil, err
		}
		// Defensive bounds check: spillOrder builds its order against the
		// SAME snapshot loaded above, but a removal racing this call must
		// never index past the slice it computed the order from. Skip
		// indices that are no longer present instead of panicking.
		if idx < 0 || idx >= len(*ws.toks) {
			continue
		}
		tok := (*ws.toks)[idx]
		if p.walkGates(ws, idx, tok) {
			continue
		}
		switch res := p.admitOnLane(ws, idx, tok, nil); res.outcome {
		case laneGranted:
			return res.lease, nil
		case laneFail:
			return nil, res.err
		case laneBreak:
			return p.walkTail(ws)
		case laneRetry:
			// Uncapped I5 carries no slot (modelRequeue is unreachable
			// with sCap == 0 — the jail gate requires a positive cap),
			// so the retry re-runs the lane admission directly.
			// Defensive: a carry with a permit is released, never held.
			if res.carry != nil && res.carry.permit != nil {
				res.carry.permit.Release()
			}
			oi--
			continue
		default: // laneNext.
			continue
		}
	}
	return p.walkTail(ws)
}

// walkTail surfaces the walk's end with the spill precedence: when buckets
// are mixed the highest-precedence non-empty bucket wins - ban >
// rate-limit > waiting-room > spill-exhausted. Correlative refusals
// (ip_capped, country-blocked, limited_ip) surface directly at the failing
// token and never reach these buckets. Each bucket contributes its best
// error (first ban, shortest rate window, lowest queue position, last
// spilled lane). Only when every bucket is empty - all tokens failed with
// errors outside the matrix - is the generic error surfaced.
// Freebucks-capped tokens were excluded in spillOrder (never
// attempted); their rate-limit reasons land here so a fully-capped pool
// surfaces a real 429 with the earliest window reset instead of a
// generic combined error.
func (p *Pool) walkTail(ws *walkState) (*Lease, error) {
	ws.rateLimited = append(ws.rateLimited, ws.quotaLimited...)
	// Every slot pinned away from the model (direct-order callers reach
	// here via the loop gates): the dedicated routing error beats the
	// generic combined one.
	if allPinnedOut(ws.toks, ws.cfg, p.reg, ws.model) {
		return nil, pinFailFastError(ws.model, len(*ws.toks))
	}
	if len(ws.banned) > 0 {
		return nil, ws.banned[0]
	}
	if len(ws.rateLimited) > 0 {
		// Pool exhausted (issue #48): every token failed and the highest-
		// precedence bucket is rate-limit — no ban/country is present, so
		// this is the "all tokens are at their quota/window limit" state the
		// operator wants to be alerted about. Fire-and-forget webhook
		// (throttled per event type); the 429 still surfaces as usual.
		p.notifyMu.Lock()
		n := p.notify
		p.notifyMu.Unlock()
		if n != nil {
			n.Send(notify.Event{
				Event: "pool_exhausted", TokenIndex: 0, Model: ws.model,
				Message: "all tokens are rate-limited; the pool cannot serve the request",
			})
		}
		return nil, bestRateLimitEntry(ws.rateLimited)
	}
	if len(ws.waiting) > 0 {
		wr := bestWaitingRoom(ws.waiting)
		p.logger.Debug("pool: waiting room surfaced", "model", ws.model, "position", wr.Position, "queue_depth", wr.QueueDepth, "retry_after", wr.RetryAfter.String())
		return nil, wr
	}
	// Every lane spilled and nothing else was recorded: surface the last
	// lane's signal once as the existing 429 shape.
	if err := ws.spill.exhausted(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("unable to acquire run from any token: %s", strings.Join(ws.errs, "; "))
}
