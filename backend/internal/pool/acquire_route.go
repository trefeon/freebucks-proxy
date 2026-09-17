// acquire_route.go - pooled acquire route: Acquire (plain index order via
// acquireOrder) plus the leaseFromOrder failover loop (per-token skip
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

// Acquire resolves the model's agent and fails over linearly in plain index
// order until a token yields both a run and a session. Returns a lease on
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

	// Model-allowlist fail-fast (MODEL_LOCKS, issue #325): when every slot
	// is locked away from the requested model, no admission can succeed —
	// surface the routing error without touching upstream at all.
	if allLockedOut(toks, cfg, p.reg, model) {
		// The ordering/filter stages are bypassed entirely here, so count
		// the skip decision per slot (the counter tracks decisions, not
		// requests — a slot can count twice across filter + failover).
		for _, tok := range *toks {
			tok.allowlistSkips.Add(1)
		}
		return nil, lockFailFastError(model, len(*toks))
	}

	// Plain index order: concurrent requests share the per-entry
	// single-flight in the session manager, so no leader gate is needed
	// to prevent duplicate session creates.
	order, quotaLimited := p.acquireOrder(toks, 0, model)
	return p.leaseFromOrder(ctx, model, agentID, cfg, toks, order, quotaLimited)
}

// leaseFromOrder runs the token failover loop against the given order.
// Extracted from Acquire so the leader-election follower path can call it
// with a reordered token list without duplicating the loop.
func (p *Pool) leaseFromOrder(ctx context.Context, model string, agentID string, cfg *config.Config, toks *[]*tokenEntry, order []int, quotaLimited []rateLimitEntry) (*Lease, error) {
	var errs []string
	var waiting []*session.WaitingRoomError
	var rateLimited []rateLimitEntry
	var banned []*upstream.BanError
	for _, idx := range order {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Defensive bounds check: acquireOrder builds its order against the
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
		// Model-allowlist routing (MODEL_LOCKS, issue #325): a slot locked
		// to other models is skipped before any session/run contact, so a
		// request never burns the wrong account's quota or churns its
		// session (upstream model_locked 409). Sits after the quarantine
		// gate so terminal states keep their error-bucket precedence, and
		// mirrors the eligible() filter for custom-order callers.
		if lockedOutByModel(cfg, p.reg, idx, model) {
			tok.allowlistSkips.Add(1)
			errs = append(errs, fmt.Sprintf("%s: model %q not in token allowlist", name, model))
			p.logger.Debug("pool: token skipped (model allowlist)", "token", idx+1, "model", model)
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
		// Live-turn slot (SLOTS_PER_ACCOUNT per account-model lane,
		// slot_ledger.go): a lease is granted only while the token holds
		// fewer live turns for this model than the cap; otherwise the
		// caller parks FIFO until QUEUE_WAIT elapses. The slot is taken
		// BEFORE any upstream admission so a queued request never burns a
		// session slot or run START while it waits. Queue-full and
		// wait-timeout map to the existing 429 rate-limit shape and fail
		// over to the next token; the caller's own ctx expiry returns as-is.
		// Skipped entirely when ROUTING_SMART is off (legacy path untouched).
		var routeSlot *slotPermit
		// queueWait is this attempt's park duration: set only when the
		// request actually parked AND the slot was granted. A waiter that
		// timed out or was cancelled held no slot and reports nothing.
		var queueWait time.Duration
		if cfg.RoutingSmart {
			slotCap, slotDepth, slotWait := slotParams(cfg)
			// SLOTS_PER_ACCOUNT=0 skips slot gating entirely: no
			// counter, no queue — the upstream quota/429 is the brake.
			if slotCap > 0 {
				parkStart := time.Now()
				permit, parked, slotErr := p.slotAcquire(ctx, slotKey{entry: tok, model: model}, idx+1, slotCap, slotDepth, slotWait)
				if slotErr != nil {
					if slotIsQueueExhausted(slotErr) {
						live := p.slotLive(slotKey{entry: tok, model: model})
						rateLimited = appendRateLimitEntry(rateLimited, slotQueueRateLimit(slotErr.(*slotQueueExhaustedError), model, slotCap, live), idx)
						errs = append(errs, fmt.Sprintf("%s: %v", name, slotErr))
						p.logger.Debug("pool: token skipped (live-turn queue exhausted)", "token", idx+1, "err", slotErr)
						continue
					}
					return nil, slotErr
				}
				// Queue-wait telemetry: the park duration rides the
				// request's phase accumulator (the server puts it on the
				// chat trace, inside the console's request card) and the
				// lease. Never recorded for a request that did not park.
				if parked {
					queueWait = time.Since(parkStart)
					phasetiming.FromContext(ctx).Since(phasetiming.QueueWaitMS, parkStart)
				}
				routeSlot = permit
			}
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
			if rle := c.rateLimited; rle != nil {
				// Issue #178: tag the refusal with the requested model when
				// the upstream body omits it, so the remembered cooldown can
				// be isolated per model — a quota cap on one model (glm-5.2,
				// gpt-5.6-luna) must not block the same token's other models.
				if rle.Model == "" {
					rle.Model = model
				}
				rateLimited = appendRateLimitEntry(rateLimited, rle, idx)
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
				banned = appendBan(banned, be)
			}
			if cbe := c.countryBlocked; cbe != nil {
				// Correlative refusal like ip_capped: surface directly.
				routeSlot.Release()
				return nil, cbe
			}
			if lie := c.limitedIp; lie != nil {
				// The egress IP cannot serve this model. The session row is
				// fine — it stays bound to its admitted model — so nothing
				// is invalidated; stamping Model makes the surfaced refusal
				// self-describing. Surface directly, no failover walk.
				lie.Model = model
				errs = append(errs, fmt.Sprintf("%s: %v", name, err))
				routeSlot.Release()
				return nil, err
			}
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
			routeSlot.Release()
			continue
		}
		tok.runs.ClearCooldowns()

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
			if rle := c.rateLimited; rle != nil {
				// Issue #178: tag the refusal with the requested model when
				// the upstream body omits it, so the remembered cooldown can
				// be isolated per model — a quota cap on one model (glm-5.2,
				// gpt-5.6-luna) must not block the same token's other models.
				if rle.Model == "" {
					rle.Model = model
				}
				rateLimited = appendRateLimitEntry(rateLimited, rle, idx)
				// Issue #122: count run-start spend_limited refusals on the
				// ledger (same counter as the chat-path refusal).
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
				banned = appendBan(banned, be)
			}
			if cbe := c.countryBlocked; cbe != nil {
				// Correlative refusal like ip_capped: surface directly.
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
		// Track the activity and end any idle-maintenance pause: the next
		// maintain tick resumes rotation/refresh work.
		p.lastActiveMu.Lock()
		p.lastActive = time.Now()
		p.idleFinished = false
		p.lastActiveMu.Unlock()
		return lease, nil
	}

	// Failover precedence: when buckets are mixed the highest-precedence
	// non-empty bucket wins — ban > rate-limit > waiting-room. Correlative
	// refusals (ip_capped, country-blocked, limited_ip) surface directly at
	// the failing token and never reach these buckets. Each bucket
	// contributes its best error (first ban, shortest rate window, lowest
	// queue position). Only when every bucket is empty — all tokens failed
	// with errors outside the matrix — is the generic error surfaced.
	// Freebucks-capped tokens were excluded in acquireOrder (never
	// attempted); their rate-limit reasons land here so a fully-capped pool
	// surfaces a real 429 with the earliest window reset instead of a
	// generic combined error.
	rateLimited = append(rateLimited, quotaLimited...)
	// Every slot locked away from the model (direct-order callers reach
	// here via the loop gates): the dedicated routing error beats the
	// generic combined one.
	if allLockedOut(toks, cfg, p.reg, model) {
		return nil, lockFailFastError(model, len(*toks))
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
	return nil, fmt.Errorf("unable to acquire run from any token: %s", strings.Join(errs, "; "))
}
