// acquire_route.go - pooled acquire route: Acquire (strict index order via
// spillOrder) plus the leaseFromOrder spill walk (per-token skip
// gates, session and run admission, lease grant, bucket precedence).
package pool

import (
	"context"
	"errors"
	"fmt"
	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/notify"
	"freebuff-proxy/backend/internal/phasetiming"
	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/upstream"
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
	args := []any{"model", model, "retry_after", cp.RetryAfter}
	if !cp.ResetAt.IsZero() {
		args = append(args, "reset", cp.ResetAt.Format(time.RFC3339))
	}
	if li := p.indexOfEntry(tok); li >= 0 {
		args = append([]any{"token", li + 1}, args...)
	}
	// RememberModelRateLimit parks only refusals with a live expiry window;
	// an opaque refusal is a no-op there, so say so here too — the Info
	// line must mean the lane will actually be skipped. The predicate
	// mirrors RememberModelRateLimit's (which stays authoritative).
	now := time.Now()
	until := cp.ResetAt
	if cp.RetryAfter > 0 {
		until = now.Add(cp.RetryAfter)
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

// leaseFromOrder runs the token spill walk against the given order.
// Extracted from Acquire so tests can drive the walk with an explicit
// order without duplicating the loop.
func (p *Pool) leaseFromOrder(ctx context.Context, model string, agentID string, cfg *config.Config, toks *[]*tokenEntry, order []int, quotaLimited []rateLimitEntry) (*Lease, error) {
	var errs []string
	var waiting []*session.WaitingRoomError
	var rateLimited []rateLimitEntry
	var banned []*upstream.BanError
	// MASQ spill walk (spill_queue.go): lane-exhausted lanes move the
	// request to the next account silently, bounded by
	// MAX_SPILL_ACCOUNTS; the last signal surfaces end-of-chain below.
	spill := newSpillChain(cfg, model)
	// queueWait accumulates every lane's park duration across spill hops:
	// a waiter that parked on lane #1 then granted on lane #2 reports the
	// full wait, not just the granting lane's. Recorded only when the
	// request actually parked somewhere; a waiter that timed out or was
	// cancelled held no slot and reports nothing on that lane.
	var queueWait time.Duration
	// Terminal-cooldown hints (pool_state pool/cooldown/*, hint only): a
	// fresh hint skips one doomed probe when another ordered token can
	// serve. When every ordered token is hinted the walk ignores hints and
	// attempts upstream live — a hint alone never fails Acquire.
	skipHinted := p.cooldownHintSkippable(toks, order, time.Now())
	// Indexed (not range): a same-lane quota requeue (I5) rewinds oi to
	// retry the lane after its RetryAfter elapses — no spill hop consumed,
	// no failover. Every other path advances normally.
	for oi := 0; oi < len(order); oi++ {
		idx := order[oi]
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Defensive bounds check: spillOrder builds its order against the
		// SAME snapshot loaded above, but a removal racing this call must
		// never index past the slice it computed the order from. Skip
		// indices that are no longer present instead of panicking.
		if idx < 0 || idx >= len(*toks) {
			continue
		}
		tok := (*toks)[idx]
		// Administratively locked tokens are never eligible for leasing.
		if tok.locked.Load() {
			continue
		}
		name := fmt.Sprintf("token-%d", idx+1)
		// Terminal-cooldown hint: skip one doomed probe (see above). Live
		// cooldown/ban memory below stays authoritative; the hint only
		// covers what a restart forgot.
		if skipHinted && tok != nil && tok.token != "" && p.cooldownHintFresh(poolTokenHash(tok.token), time.Now()) {
			errs = append(errs, fmt.Sprintf("%s: terminal cooldown hint fresh, skipping one probe", name))
			p.logger.Debug("pool: token skipped (cooldown hint)", "token", idx+1)
			continue
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
			errs = append(errs, fmt.Sprintf("%s: quarantined (%s: %s)", name, q.reason, q.detail))
			p.logger.Debug("pool: token skipped (quarantined)", "token", idx+1, "state", q.reason, "reason", q.detail)
			switch terr := q.err.(type) {
			case *upstream.BanError:
				dup := false
				for _, existing := range banned {
					if existing.Error() == terr.Error() {
						dup = true
						break
					}
				}
				if !dup {
					banned = append(banned, terr)
				}
			}
			continue
		}
		// Single-pin routing (PIN_MODEL): a slot pinned to another model
		// is skipped before any session/run contact, so a request never
		// burns the wrong account's quota or churns its session. Sits
		// after the quarantine gate so terminal states keep their
		// error-bucket precedence, and mirrors the eligible() filter for
		// custom-order callers.
		if pinnedOut(cfg, p.reg, idx, model) {
			tok.pinSkips.Add(1)
			errs = append(errs, fmt.Sprintf("%s: model %q not pinned to this slot", name, model))
			p.logger.Debug("pool: token skipped (model pin)", "token", idx+1, "model", model)
			continue
		}

		if until := tok.runs.CooldownUntil(); time.Now().Before(until) || tok.runs.BanError() != nil {
			// Issue #155: if the cooldown was caused by a specific model's quota exhaustion,
			// and we are requesting a different model (e.g. fallback to mimo-v2.5),
			// do not block this token from serving the requested model.
			if canServeOtherModel(tok.runs.RateLimitError(), model) {
				// Token is only quota-capped for the remembered model, but can still serve `model`.
			} else {
				errs = append(errs, fmt.Sprintf("%s: cooling down until %s", name, until.Format(time.RFC3339)))
				p.logger.Debug("pool: token skipped (cooldown)", "token", idx+1, "until", until.Format(time.RFC3339))
				if be := tok.runs.BanError(); be != nil {
					dup := false
					for _, existing := range banned {
						if existing.Error() == be.Error() {
							dup = true
							break
						}
					}
					if !dup {
						banned = append(banned, be)
					}
				}
				if rle := tok.runs.RateLimitError(); rle != nil {
					rateLimited = appendRateLimitEntry(rateLimited, rle, idx)
				}
				continue
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
			rateLimited = appendRateLimitEntry(rateLimited, &tagged, idx)
			errs = append(errs, name+": "+tagged.Error()+" (remembered, no upstream contact)")
			p.logger.Debug("pool: token skipped (remembered model rate limit)", "token", idx+1, "model", model)
			continue
		}
		// Live-turn slot (SLOTS_PER_ACCOUNT per account-model lane,
		// slot_ledger.go): a lease is granted only while the token holds
		// fewer live turns for this model than the cap; otherwise the
		// caller parks FIFO until QUEUE_WAIT elapses. The slot is taken
		// BEFORE any upstream admission so a queued request never burns a
		// session slot or run START while it waits. A lane-exhausted
		// waiter spills silently to the next lane (spill_queue.go) - a
		// full lane is not an upstream refusal, so it writes no bucket
		// and no error string. The caller's own ctx expiry returns
		// as-is.
		var routeSlot *slotPermit
		slotCap, slotDepth, slotWait := slotParams(cfg)
		// SLOTS_PER_ACCOUNT=0 skips slot gating entirely: no
		// counter, no queue - the upstream quota/429 is the brake.
		if slotCap > 0 {
			parkStart := time.Now()
			permit, parked, slotErr := p.slotAcquire(ctx, slotKey{entry: tok, model: model}, idx+1, slotCap, slotDepth, slotWait)
			if slotErr != nil {
				if qerr, ok := slotErr.(*slotQueueExhaustedError); ok {
					// Trust the signal's carried lane wait: the
					// lane's own timer elapsed, so the park is the
					// configured laneWait — not the wall clock
					// around the call, which a failing assertion
					// in this file once zeroed.
					if qerr.Reason == "timeout" {
						queueWait += qerr.Wait
					}
					if spill.note(qerr, slotCap) {
						p.logger.Debug("pool: token lane spilled", "token", idx+1, "err", slotErr)
						continue
					}
					break
				}
				return nil, slotErr
			}
			// Queue-wait telemetry: the accumulated park rides the
			// request's phase accumulator (the server puts it on the
			// chat trace, inside the console's request card) and the
			// lease. Never recorded for a request that did not park.
			if parked {
				queueWait += time.Since(parkStart)
				phasetiming.FromContext(ctx).Since(phasetiming.QueueWaitMS, time.Now().Add(-queueWait))
			}
			routeSlot = permit
		}

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
			continue
		}
		sessionStart := time.Now()
		// Issue #94(b): WAITING_ROOM_CHAIN gate — when the upstream last
		// refused this token with 428 waiting_room_required, fire the
		// reference pre-session ad-chain + streak flow (best-effort, bounded
		// by the client's own chain timeout) before the next session create
		// so the admission does not bounce off the same 428 again.
		if cfg.WaitingRoomChain && tok.client.ConsumeWaitingRoomChain() {
			p.logger.Debug("pool: firing waiting-room pre-session chain", "token", idx+1)
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
				p.logger.Debug("pool: token auth rejected, continuing failover", "token", idx+1)
			}
			var wr *session.WaitingRoomError
			if errors.As(err, &wr) {
				waiting = append(waiting, wr)
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
						routeSlot.Release()
						p.logger.Debug("pool: quota requeue same lane", "token", idx+1, "model", model, "retry_after", rle.RetryAfter)
						rqStart := time.Now()
						_, parked, rerr := p.slotRequeue(ctx, slotKey{entry: tok, model: model}, idx+1, sCap, sDepth, sWait, notBefore)
						if rerr != nil {
							if qerr, ok := rerr.(*slotQueueExhaustedError); ok {
								if qerr.Reason == "timeout" {
									queueWait += qerr.Wait
								}
								if spill.note(qerr, sCap) {
									p.logger.Debug("pool: token lane spilled", "token", idx+1, "err", rerr)
									continue
								}
								break
							}
							return nil, rerr
						}
						if parked {
							queueWait += time.Since(rqStart)
							phasetiming.FromContext(ctx).Since(phasetiming.QueueWaitMS, time.Now().Add(-queueWait))
						}
						oi--
						continue
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
				rateLimited = appendRateLimitEntry(rateLimited, rle, idx)
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
				return nil, ice
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
				banned = appendBan(banned, be)
			}
			if cbe := c.countryBlocked; cbe != nil {
				// Correlative refusal like ip_capped: surface directly. The
				// hint only skips one future probe; the walk never fails over.
				p.storeCountryHint(tok, time.Now().Add(p.countryBlockWindow()))
				routeSlot.Release()
				return nil, cbe
			}
			if lie := c.limitedIp; lie != nil {
				// The egress IP cannot serve this model. The session row is
				// fine — it stays bound to its admitted model — so nothing
				// is invalidated. Surface a walk-local Model-stamped copy
				// (never mutate the single-flight-shared value), no
				// failover walk.
				routeSlot.Release()
				return nil, tagLimitedIPModel(lie, model)
			}
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
			routeSlot.Release()
			continue
		}
		tok.runs.ClearCooldowns()
		// Live admission proves the account healthy: drop any terminal hint
		// a previous window left behind.
		p.clearCooldownHintFor(tok)

		// Re-validate the token is still current: a concurrent
		// RemoveLastToken may have swapped the snapshot while the session
		// admission above was in flight. Leasing a removed token would
		// strand its run's inflight — LeaseRelease always releases through
		// the lease's own entry, but the run would belong to a drained,
		// retiring manager — so skip instead (the removal path drains the
		// retired entry once it observes the slip).
		if cur := p.roster.Load(); idx < 0 || idx >= len(*cur) || (*cur)[idx] != tok {
			routeSlot.Release()
			continue
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
				// terminal quarantine — see the admission path.
				p.logger.Debug("pool: token auth rejected, continuing failover", "token", idx+1)
			}
			// MASQ same-lane quota requeue (I5, spill_queue.go): a
			// short-window run-start quota jail is waited out on this
			// lane - no cooldown write, no failover, no spill hop.
			// Longer (or windowless) windows fall through to the per-lane
			// record below. Terminal mixes (ban, correlative
			// refusals, auth rejection) never requeue.
			if rle := c.rateLimited; rle != nil && !c.authRejected && c.banned == nil && c.ipCapped == nil && c.countryBlocked == nil && c.limitedIp == nil {
				if sCap, sDepth, sWait := slotParams(cfg); sCap > 0 {
					if notBefore, ok := quotaRequeueNotBefore(rle, sWait); ok {
						// No Model tag here: the refusal is discarded after
						// the requeue (only RetryAfter is read above) — see
						// the admission path.
						// Issue #122: count spend_limited on the ledger.
						if c.spendLimited {
							tok.ledger.recordSpendLimited()
						}
						routeSlot.Release()
						p.logger.Debug("pool: quota requeue same lane", "token", idx+1, "model", model, "retry_after", rle.RetryAfter, "phase", "run-start")
						rqStart := time.Now()
						_, parked, rerr := p.slotRequeue(ctx, slotKey{entry: tok, model: model}, idx+1, sCap, sDepth, sWait, notBefore)
						if rerr != nil {
							if qerr, ok := rerr.(*slotQueueExhaustedError); ok {
								if qerr.Reason == "timeout" {
									queueWait += qerr.Wait
								}
								if spill.note(qerr, sCap) {
									p.logger.Debug("pool: token lane spilled", "token", idx+1, "err", rerr)
									continue
								}
								break
							}
							return nil, rerr
						}
						if parked {
							queueWait += time.Since(rqStart)
							phasetiming.FromContext(ctx).Since(phasetiming.QueueWaitMS, time.Now().Add(-queueWait))
						}
						oi--
						oi--
						continue
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
				rateLimited = appendRateLimitEntry(rateLimited, rle, idx)
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
				return nil, ice
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
				banned = appendBan(banned, be)
			}
			if cbe := c.countryBlocked; cbe != nil {
				// Correlative refusal like ip_capped: surface directly.
				p.storeCountryHint(tok, time.Now().Add(p.countryBlockWindow()))
				routeSlot.Release()
				return nil, cbe
			}
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
			routeSlot.Release()
			continue
		}
		leaseAttrs := []any{
			"token", idx + 1, "model", effectiveModel, "agent", effectiveAgentID, "instance_id", instanceID,
			"country", ss.CountryCode,
		}
		if queueWait > 0 {
			// Queue-wait telemetry: this admission parked in the account's
			// FIFO live-turn queue before a slot was granted. Before this
			// line existed, a granted park was invisible — only the
			// timeout/exhausted path logged anything at all.
			leaseAttrs = append(leaseAttrs, "queue_wait_ms", queueWait.Milliseconds(), "queue_parked", true)
		}
		p.logger.Debug("pool: lease acquired", leaseAttrs...)
		if cur := p.roster.Load(); idx < 0 || idx >= len(*cur) || (*cur)[idx] != tok {
			tok.runs.Release(run)
			routeSlot.Release()
			continue
		}
		lease := &Lease{
			Token: idx, Model: effectiveModel, AgentID: effectiveAgentID, Run: run, SessionInstanceID: instanceID,
			entry: tok, routeSlot: routeSlot, QueueWait: queueWait, AcquiredAt: time.Now(),
		}
		// MASQ precious (precious.go): the account served this model, so its
		// live session is never proactively dropped from here on.
		p.markPrecious(tok, effectiveModel)
		// Track the activity and end any idle-maintenance pause: the next
		// maintain tick resumes rotation/refresh work.
		p.lastActiveMu.Lock()
		p.lastActive = time.Now()
		p.idleFinished = false
		p.lastActiveMu.Unlock()
		return lease, nil
	}

	// Spill precedence: when buckets are mixed the highest-precedence
	// non-empty bucket wins - ban > rate-limit > waiting-room >
	// spill-exhausted. Correlative refusals (ip_capped, country-blocked,
	// limited_ip) surface directly at the failing token and never reach
	// these buckets. Each bucket contributes its best error (first ban,
	// shortest rate window, lowest queue position, last spilled lane).
	// Only when every bucket is empty - all tokens failed with errors
	// outside the matrix - is the generic error surfaced.
	// Freebucks-capped tokens were excluded in spillOrder (never
	// attempted); their rate-limit reasons land here so a fully-capped pool
	// surfaces a real 429 with the earliest window reset instead of a
	// generic combined error.
	rateLimited = append(rateLimited, quotaLimited...)
	// Every slot pinned away from the model (direct-order callers reach
	// here via the loop gates): the dedicated routing error beats the
	// generic combined one.
	if allPinnedOut(toks, cfg, p.reg, model) {
		return nil, pinFailFastError(model, len(*toks))
	}
	if len(banned) > 0 {
		return nil, banned[0]
	}
	if len(rateLimited) > 0 {
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
				Event: "pool_exhausted", TokenIndex: 0, Model: model,
				Message: "all tokens are rate-limited; the pool cannot serve the request",
			})
		}
		return nil, bestRateLimitEntry(rateLimited)
	}
	if len(waiting) > 0 {
		wr := bestWaitingRoom(waiting)
		p.logger.Debug("pool: waiting room surfaced", "position", wr.Position, "queue_depth", wr.QueueDepth, "retry_after", wr.RetryAfter.String())
		return nil, wr
	}
	// Every lane spilled and nothing else was recorded: surface the last
	// lane's signal once as the existing 429 shape.
	if err := spill.exhausted(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("unable to acquire run from any token: %s", strings.Join(errs, "; "))
}
