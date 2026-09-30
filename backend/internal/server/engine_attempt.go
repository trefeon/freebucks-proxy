package server

import (
	"context"
	"errors"
	"freebuff-proxy/backend/internal/convert"
	"freebuff-proxy/backend/internal/pool"
	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/upstream"
	"io"
	"net/http"
	"strings"
	"time"
)

// chatAutoRetry bounds for CHAT_AUTO_RETRY (opt-in in-request retry so
// transient refusals resolve to 200 instead of interrupting clients):
// at most 3 retries within 120s total; a 429 is honored only when upstream
// names a window of 90s or less (30m sliding-window refusals surface, never
// waited out); waiting-room queue waits never exceed 30s; run-invalid
// retries immediately against a fresh run.
const (
	chatAutoRetryMax     = 3
	chatAutoRetryBudget  = 120 * time.Second
	chatAutoRetryRateCap = 90 * time.Second
	chatAutoRetryWaitCap = 30 * time.Second
	chatAutoRetryWaitDef = 10 * time.Second
)

// autoRetryWait reports how long to wait before re-attempting a refused
// chat, or false when the refusal is terminal and must surface. The
// transient shapes retry: short-window 429s, run-invalid (fresh run, no
// wait), and waiting-room queue holds. A refunded-purchase 409
// session_superseded ("purchase was refunded...", e.g. after a waiting-room
// 503 consumed the hold) also retries with no wait: no competing instance
// holds the seat, so the already-invalidated session re-admits fresh on the
// next attempt. A takeover 409 (no refund wording) stays terminal — stealing
// the seat back risks ping-pong with the live holder. Everything else —
// 400s, 401, bans, consent, credits, session-ending 428 — is terminal.
func autoRetryWait(err error) (time.Duration, bool) {
	var rle *upstream.RateLimitError
	if errors.As(err, &rle) {
		if rle.RetryAfter <= 0 || rle.RetryAfter > chatAutoRetryRateCap {
			return 0, false
		}
		return rle.RetryAfter, true
	}
	if errors.Is(err, upstream.ErrRunInvalid) {
		return 0, true
	}
	var wre *upstream.WaitingRoomError
	if errors.As(err, &wre) {
		wait := wre.RetryAfter
		if wait <= 0 {
			wait = chatAutoRetryWaitDef
		}
		if wait > chatAutoRetryWaitCap {
			return 0, false
		}
		return wait, true
	}
	var sse *upstream.SessionSupersededError
	if errors.As(err, &sse) {
		// Refund wording only: "This model purchase was refunded. Start a
		// new session to try again." No holder to fight, safe to rejoin.
		if refundedSuperseded(err) {
			return 0, true
		}
		return 0, false
	}
	return 0, false
}

// refundedSuperseded reports whether err is the refunded-purchase 409
// session_superseded ("This model purchase was refunded. Start a new session
// to try again."). A takeover 409 carries no refund marker and has a live
// holder, so it must stay terminal — stealing the seat back risks ping-pong.
// The two wordings share one wire code, so the refund marker is the only
// discriminator.
func refundedSuperseded(err error) bool {
	var sse *upstream.SessionSupersededError
	if !errors.As(err, &sse) {
		return false
	}
	return strings.Contains(strings.ToLower(sse.Body), "refund")
}

// chatBackend abstracts the acquire/chat/invalidate/cooldown/lease hooks the
// single-attempt chat path needs (issue #255).
type chatBackend interface {
	Acquire(ctx context.Context, model string) (*pool.Lease, error)
	Chat(ctx context.Context, lease *pool.Lease, opts upstream.ChatOptions, body []byte) (io.ReadCloser, error)
	InvalidateSession(lease *pool.Lease)
	InvalidateSessionSuperseded(lease *pool.Lease)
	InvalidateSessionStuckQueue(lease *pool.Lease)
	InvalidateRun(lease *pool.Lease, agentID string)
	CooldownBan(lease *pool.Lease, be *upstream.BanError)
	LeaseRelease(lease *pool.Lease)
	LeaseAbandon(lease *pool.Lease)
	MarkRunFailed(lease *pool.Lease)
	RecordRunStep(lease *pool.Lease, messageID string)
	RecordSpend(lease *pool.Lease, tokens int64)
	// FinishRun FINISHes the turn's run upstream at turn end (server-owned
	// turn lifecycle): success and terminal-failure turns FINISH explicitly.
	// The pool guards against the manager predecessor drain (stale leases
	// skip), so a turn is never FINISHed twice. Nil-safe.
	FinishRun(ctx context.Context, lease *pool.Lease)
}

// pooledBackend adapts the pool's fixed-token methods. The lease carries the
// token index, so token-indexed calls take it from the lease.
type pooledBackend struct{ p *pool.Pool }

func (b pooledBackend) Acquire(ctx context.Context, model string) (*pool.Lease, error) {
	return b.p.Acquire(ctx, model)
}

func (b pooledBackend) Chat(ctx context.Context, lease *pool.Lease, opts upstream.ChatOptions, body []byte) (io.ReadCloser, error) {
	return b.p.Chat(ctx, lease, opts, body)
}

func (b pooledBackend) InvalidateSession(lease *pool.Lease) {
	b.p.InvalidateLeaseSession(lease)
}

func (b pooledBackend) InvalidateSessionSuperseded(lease *pool.Lease) {
	b.p.InvalidateLeaseSessionWithReason(lease, session.ReasonSuperseded, http.StatusConflict)
}

func (b pooledBackend) InvalidateSessionStuckQueue(lease *pool.Lease) {
	b.p.InvalidateLeaseSessionStuckQueue(lease)
}

func (b pooledBackend) InvalidateRun(lease *pool.Lease, agentID string) {
	b.p.InvalidateLeaseRun(lease, agentID)
}

func (b pooledBackend) CooldownBan(lease *pool.Lease, be *upstream.BanError) {
	b.p.CooldownLeaseBan(lease, be)
}
func (b pooledBackend) LeaseRelease(lease *pool.Lease)  { b.p.LeaseRelease(lease) }
func (b pooledBackend) LeaseAbandon(lease *pool.Lease)  { b.p.LeaseAbandon(lease) }
func (b pooledBackend) MarkRunFailed(lease *pool.Lease) { b.p.MarkRunFailed(lease) }
func (b pooledBackend) RecordRunStep(lease *pool.Lease, mid string) {
	b.p.RecordRunStep(lease, mid)
}
func (b pooledBackend) RecordSpend(lease *pool.Lease, tokens int64) { b.p.RecordSpend(lease, tokens) }
func (b pooledBackend) FinishRun(ctx context.Context, lease *pool.Lease) {
	b.p.FinishLeaseRun(ctx, lease)
}

// chatAttempt runs one chat through the pool with opt-in auto-retry
// (CHAT_AUTO_RETRY): transient refusals re-attempt in-request until 200 so
// clients never see them, bounded to chatAutoRetryMax retries within
// chatAutoRetryBudget; terminal refusals surface on the first attempt.
// Retries happen before any response byte is written, so streaming and
// non-streaming callers share the shield. When the knob is off this is
// exactly one chatAttemptOnce pass (fail-fast, as pinned).
func (s *Server) chatAttempt(ctx context.Context, model string, normalized []byte, st *chatTraceState, backend chatBackend, autoRetry bool) (io.ReadCloser, *pool.Lease, error) {
	if !autoRetry {
		up, lease, err := s.chatAttemptOnce(ctx, model, normalized, st, backend)
		if err == nil || !refundedSuperseded(err) {
			return up, lease, err
		}
		// Refunded purchase (409 session_superseded, refund wording): the
		// seat is gone and NO competing instance holds it, so rejoin fresh
		// exactly once even with auto-retry off — chatAttemptOnce already
		// invalidated the dead session, so the retry re-admits on a new row.
		// Bounded: one extra upstream attempt per request, never a loop. A
		// takeover 409 (live holder) is excluded by refundedSuperseded and
		// still surfaces immediately.
		st.retried = true
		return s.chatAttemptOnce(ctx, model, normalized, st, backend)
	}
	deadline := time.Now().Add(chatAutoRetryBudget)
	var up io.ReadCloser
	var lease *pool.Lease
	var err error
	for n := 0; ; n++ {
		up, lease, err = s.chatAttemptOnce(ctx, model, normalized, st, backend)
		if err == nil {
			return up, lease, nil
		}
		wait, ok := autoRetryWait(err)
		if !ok || n >= chatAutoRetryMax || ctx.Err() != nil {
			return nil, nil, err
		}
		if wait > 0 {
			st.retried = true
			st.backoffMs += wait.Milliseconds()
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, nil, err
			case <-timer.C:
			}
		}
		if time.Now().After(deadline) {
			return nil, nil, err
		}
	}
}

// chatAttemptOnce runs one chat through the leased token and surfaces the
// result: on success the returned body reader and final lease belong to the
// caller (close the body, FINISH the turn via FinishRun, then release the
// lease via LeaseRelease). The lease's run is the turn's MintTurnRun run
// (TurnRun for the lease): the ChatOptions run_id, the llm_step_number
// stamp, and the recorded step all reuse that one run_id across the turn's
// tool steps — nothing re-STARTs mid-turn. Refusals never retry in-request
// here — the error returns for writeError after FINISHing the turn as failed
// (run-invalid excepted: the run is dead upstream, so Invalidate drops it
// without FINISH) and releasing the lease, with cache invalidation for dead
// sessions/runs and a ban cooldown+quarantine for terminal bans so the
// account stops serving. The auto-retry wrapper above re-invokes this for
// transient refusals; a dead run is invalidated here so the retry's acquire
// mints fresh. The acquire/chat/invalidate/cooldown hooks are behind the
// chatBackend interface. 429 quota, ip_capped and country blocks surface
// with no cooldown write: admission owns those refusals, not the chat path.
func (s *Server) chatAttemptOnce(ctx context.Context, model string, normalized []byte, st *chatTraceState, backend chatBackend) (io.ReadCloser, *pool.Lease, error) {
	lease, err := backend.Acquire(ctx, model)
	if err != nil {
		return nil, nil, err
	}

	// The lease is the authoritative source for the model its session/run
	// are bound to: after a #100 fallback the acquire returned a lease for
	// the FALLBACK model while the caller still holds the requested model.
	// opts.Model, the body model and x-freebuff-model must all agree with
	// the lease (previously the request went upstream labeled
	// with the requested model against the fallback session/run).
	effectiveModel := lease.Model
	if effectiveModel == "" {
		effectiveModel = model
	}
	if effectiveModel != model {
		// Snapshot the proxy-stamped system cache marker before the
		// fallback re-normalize: NormalizeRequest's client-marker strip
		// cannot tell proxy-originated markers from client echoes, so
		// re-apply afterwards — scoped to bodies that already carried it
		// (the Anthropic ingress stays marker-free by design).
		keepMarker := systemCacheMarker(normalized)
		if renormalized, nerr := convert.NormalizeRequest(normalized, effectiveModel); nerr == nil {
			normalized = renormalized
			if keepMarker {
				normalized = stampOpenAISystemCacheMarker(normalized)
			}
		}
	}

	opts := upstream.ChatOptions{
		Model:             effectiveModel,
		RunID:             lease.Run.RunID,
		SessionInstanceID: lease.SessionInstanceID,
		TraceSessionID:    lease.Run.TraceSessionID,
		// One client_id for the whole run: a fresh draw per call is the
		// free_mode_run_fanout shape (see injectEnvelope).
		ClientID: lease.Run.ClientID,
		// The run's root agent family selects the canonical system-prompt
		// opening: base3-free-* roots speak base3, others base2.
		AgentID: lease.Run.AgentID,
		// D1: the request's correlation id, threaded to the upstream
		// client so its do()/retry log lines share the server's req_id.
		RequestID: st.reqID,
		// Issue #113: stamp the run's 1-based per-chat step counter so
		// codebuff_metadata["llm_step_number"] matches the CLI (each chat
		// call is one agent step; run-agent-step.ts increments per step).
		// One increment per chatAttempt: the counter belongs to the turn's
		// MintTurnRun run (TurnRun for the lease), and the turn runs a single
		// attempt — the next turn mints (and stamps) its own run.
		StepNumber: int(lease.Run.NextStepNumber()),
	}

	released := false
	release := func() {
		if !released {
			released = true
			if ctx.Err() != nil {
				// Issue #157: the downstream client is gone (context
				// canceled — 72 hits/5k logs as 60s harness timeouts and
				// Ctrl-C on long runs). Abandon the lease instead of a
				// plain release: the run is dropped from the active set
				// and FINISHed as "cancelled" through the bounded queue
				// (CLI DELETE-on-exit parity, issue #53) so upstream does
				// not keep an abandoned agent run alive until the 6h
				// rotation. Plain releasing here left the run active for
				// the full duration, then wasted it.
				backend.LeaseAbandon(lease)
				return
			}
			backend.LeaseRelease(lease)
		}
	}
	defer release()

	up, err := backend.Chat(ctx, lease, opts, normalized)
	st.attempts++
	if err == nil {
		st.statuses = append(st.statuses, http.StatusOK)
		released = true // Disarm deferred release: ownership transferred to caller
		return up, lease, nil
	}
	if sc := attemptStatus(err); sc != 0 {
		st.statuses = append(st.statuses, sc)
	}
	// The lease is released before every error return below, so remember
	// its attribution for the trace line now: token/agent for the access
	// and trace lines, run/session ids so the error trace matches the ok
	// path field-for-field. lease.Run is always set on pool leases; the
	// guard is for synthetic test leases only.
	if lease != nil {
		st.failedToken = tokenLabel(lease)
		st.failedAgent = lease.AgentID
		if lease.Run != nil {
			st.failedRunID = lease.Run.RunID
			st.failedTraceSessionID = lease.Run.TraceSessionID
		}
		st.failedInstanceID = lease.SessionInstanceID
	}
	// failTurn records the terminal failure and FINISHes the turn's run
	// (server-owned turn lifecycle, CLI parity): the FINISH reports failed
	// instead of completed. Skipped when the client already went away (ctx
	// cancelled) — the release path's Abandon owns the async cancelled
	// FINISH there. Run-invalid is excluded by its branch (the run is dead
	// upstream; Invalidate drops it without FINISH).
	failTurn := func() {
		if ctx.Err() != nil {
			return
		}
		backend.MarkRunFailed(lease)
		backend.FinishRun(ctx, lease)
	}
	switch {
	case errors.Is(err, upstream.ErrModelIPLimited):
		// The egress IP is limited for the requested model. The session
		// stays bound to its admitted model — NOT invalidated. Surface
		// with no unfit mark and no retry.
		failTurn()
		release()
		return nil, nil, err
	case errors.Is(err, upstream.ErrSessionInvalid):
		failTurn()
		release()
		backend.InvalidateSession(lease)
		return nil, nil, err
	case errors.Is(err, upstream.ErrWaitingRoomRequired):
		// #116: 428 waiting_room_required is session-ENDING (the seat is
		// gone mid-chat). Drop the cached session so the NEXT request
		// re-admits fresh, and surface.
		failTurn()
		release()
		backend.InvalidateSession(lease)
		return nil, nil, err
	case errors.Is(err, upstream.ErrWaitingRoom):
		// waiting_room_queued is a transient admit race
		// (endsTheSession:false): the cached session is normally fine, so
		// FINISH the turn as failed and surface. EXCEPTION: a row that
		// strikes out (stuckQueueThreshold consecutive queues on one
		// instance) is wedged upstream — it polls active yet refuses every
		// chat — so drop the cached session for a fresh admission instead
		// of reusing the dead row forever. The 503 still surfaces for this
		// turn; the recovery lands on the retry (in-request under
		// CHAT_AUTO_RETRY, otherwise the client's next attempt).
		failTurn()
		release()
		if lease.NoteWaitingRoomQueue() {
			s.logger.Info("server: dropping stuck queued session for fresh admission",
				"token", lease.Token+1, "instance", lease.SessionInstanceID)
			backend.InvalidateSessionStuckQueue(lease)
		}
		return nil, nil, err
	case errors.Is(err, upstream.ErrSessionLimitReached):
		// 409 session_limit_reached (endsTheSession:false): the account is
		// over budget but this session's row is fine. FINISH the turn as
		// failed and surface.
		failTurn()
		release()
		return nil, nil, err
	case errors.Is(err, upstream.ErrSessionSuperseded):
		// #159: 409 session_superseded is TERMINAL for this request —
		// another instance took over the account. Drop the cached session
		// (reason "superseded") so the NEXT request re-admits fresh, and
		// surface. NEVER retry on the dead instance.
		failTurn()
		release()
		backend.InvalidateSessionSuperseded(lease)
		return nil, nil, err
	case errors.Is(err, upstream.ErrTurnSpendLimited):
		// turn_spend_limit killed THIS turn (per-turn spend ceiling).
		// TERMINAL for the current request: surface immediately with no
		// cooldown and no re-acquire.
		failTurn()
		release()
		return nil, nil, err
	case errors.Is(err, upstream.ErrRunInvalid):
		release()
		backend.InvalidateRun(lease, lease.AgentID)
		return nil, nil, err
	case errors.Is(err, upstream.ErrAuthRejected):
		// No cooldown write: the account simply failed to serve.
		failTurn()
		release()
		return nil, nil, err
	case errors.Is(err, upstream.ErrBanned):
		// Terminal ban: remember it (quarantines the account) and surface.
		var be *upstream.BanError
		if errors.As(err, &be) {
			backend.CooldownBan(lease, be)
		}
		failTurn()
		release()
		return nil, nil, err
	case errors.Is(err, upstream.ErrRateLimited):
		// Turn-time 429: surface with no cooldown write and no failover.
		failTurn()
		release()
		return nil, nil, err
	case errors.Is(err, upstream.ErrIpCapped):
		// Admission-only signal surfacing mid-chat: no cooldown write.
		failTurn()
		release()
		return nil, nil, err
	case errors.Is(err, upstream.ErrCountryBlocked):
		// No cooldown write: surface.
		failTurn()
		release()
		return nil, nil, err
	case errors.Is(err, upstream.ErrFreeModeUnavailable):
		// Terminal region/egress refusal: surface with no cooldown
		// write and no failover-spin.
		failTurn()
		release()
		return nil, nil, err
	case errors.Is(err, upstream.ErrProviderUsage):
		// Shared provider account needs a refill — the token itself is
		// healthy: no cooldown write, surface.
		failTurn()
		release()
		return nil, nil, err
	case errors.Is(err, upstream.ErrConsentRequired), errors.Is(err, upstream.ErrFirstTabChanged):
		// Terminal admission refusals surfacing mid-chat: surface with
		// no cooldown write and no retry.
		failTurn()
		release()
		return nil, nil, err
	case errors.Is(err, upstream.ErrCredits):
		// #117: 402 is NEVER retried.
		failTurn()
		release()
		return nil, nil, err
	default:
		failTurn()
		release()
		return nil, nil, err
	}
}
