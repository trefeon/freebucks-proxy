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
)

// chatBackend abstracts the acquire/chat/invalidate/cooldown/lease hooks the
// single-attempt chat path needs, so the pooled (fixed-token) and bridge
// paths share one chatAttempt implementation (issue #255). Each adapter
// maps the pool's token-indexed (pooled) or lease-based (bridge) methods onto
// this uniform surface.
type chatBackend interface {
	Acquire(ctx context.Context, model string) (*pool.Lease, error)
	Chat(ctx context.Context, lease *pool.Lease, opts upstream.ChatOptions, body []byte) (io.ReadCloser, error)
	InvalidateSession(lease *pool.Lease)
	InvalidateSessionSuperseded(lease *pool.Lease)
	InvalidateRun(lease *pool.Lease, agentID string)
	CooldownBan(lease *pool.Lease, be *upstream.BanError)
	LeaseRelease(lease *pool.Lease)
	LeaseAbandon(lease *pool.Lease)
	MarkRunFailed(lease *pool.Lease)
	RecordRunStep(lease *pool.Lease, messageID string)
	RecordSpend(lease *pool.Lease, tokens int64)
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

// bridgeBackend adapts the pool's lease-based bridge methods. It carries the
// client token used for the bridge Acquire.
type bridgeBackend struct {
	p     *pool.Pool
	token string
}

func (b bridgeBackend) Acquire(ctx context.Context, model string) (*pool.Lease, error) {
	return b.p.AcquireBridge(ctx, b.token, model)
}

func (b bridgeBackend) Chat(ctx context.Context, lease *pool.Lease, opts upstream.ChatOptions, body []byte) (io.ReadCloser, error) {
	return b.p.Chat(ctx, lease, opts, body)
}

func (b bridgeBackend) InvalidateSession(lease *pool.Lease) {
	b.p.InvalidateBridgeSession(lease)
}

func (b bridgeBackend) InvalidateSessionSuperseded(lease *pool.Lease) {
	b.p.InvalidateBridgeSessionWithReason(lease, session.ReasonSuperseded, http.StatusConflict)
}

func (b bridgeBackend) InvalidateRun(lease *pool.Lease, agentID string) {
	b.p.InvalidateBridgeRun(lease, agentID)
}

func (b bridgeBackend) CooldownBan(lease *pool.Lease, be *upstream.BanError) {
	b.p.CooldownBridgeBan(lease, be)
}
func (b bridgeBackend) LeaseRelease(lease *pool.Lease)  { b.p.LeaseRelease(lease) }
func (b bridgeBackend) LeaseAbandon(lease *pool.Lease)  { b.p.LeaseAbandon(lease) }
func (b bridgeBackend) MarkRunFailed(lease *pool.Lease) { b.p.MarkRunFailed(lease) }
func (b bridgeBackend) RecordRunStep(lease *pool.Lease, mid string) {
	b.p.RecordRunStep(lease, mid)
}
func (b bridgeBackend) RecordSpend(lease *pool.Lease, tokens int64) { b.p.RecordSpend(lease, tokens) }

// chatAttempt runs one chat through the leased token and surfaces the
// result: on success the returned body reader and final lease belong to the
// caller (close the body and release the lease via LeaseRelease). Refusals
// never retry in-request — the error returns for writeError after releasing
// the lease, with cache invalidation for dead sessions/runs (invalid,
// expired, superseded, 428-required) and a ban cooldown+quarantine for
// terminal bans so the account stops serving. The acquire/chat/invalidate/
// cooldown hooks are behind the chatBackend interface so the pooled
// (fixed-token) and bridge paths share one implementation. 429 quota,
// ip_capped and country blocks surface with no cooldown write: admission
// owns those refusals, not the chat path.
func (s *Server) chatAttempt(ctx context.Context, model string, normalized []byte, st *chatTraceState, backend chatBackend) (io.ReadCloser, *pool.Lease, error) {
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
		if renormalized, nerr := convert.NormalizeRequest(normalized, effectiveModel); nerr == nil {
			normalized = renormalized
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
		// Incremented once per chatAttempt — the retry-once loop below
		// retries the SAME step.
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
	st.attempts = 1
	if err == nil {
		st.statuses = append(st.statuses, http.StatusOK)
		released = true // Disarm deferred release: ownership transferred to caller
		return up, lease, nil
	}
	if sc := attemptStatus(err); sc != 0 {
		st.statuses = append(st.statuses, sc)
	}
	// The lease is released before every error return below, so remember
	// its attribution for the trace line now.
	if lease != nil {
		st.failedToken = tokenLabel(lease)
		st.failedAgent = lease.AgentID
	}
	switch {
	case errors.Is(err, upstream.ErrModelIPLimited):
		// The egress IP is limited for the requested model. The session
		// stays bound to its admitted model — NOT invalidated. Surface
		// with no unfit mark and no retry.
		release()
		return nil, nil, err
	case errors.Is(err, upstream.ErrSessionInvalid):
		release()
		backend.InvalidateSession(lease)
		return nil, nil, err
	case errors.Is(err, upstream.ErrWaitingRoomRequired):
		// #116: 428 waiting_room_required is session-ENDING (the seat is
		// gone mid-chat). Drop the cached session so the NEXT request
		// re-admits fresh, and surface.
		release()
		backend.InvalidateSession(lease)
		return nil, nil, err
	case errors.Is(err, upstream.ErrWaitingRoom):
		// waiting_room_queued is a transient admit race
		// (endsTheSession:false): the cached session is fine. Release
		// with no invalidation and surface.
		release()
		return nil, nil, err
	case errors.Is(err, upstream.ErrSessionLimitReached):
		// 409 session_limit_reached (endsTheSession:false): the account is
		// over budget but this session's row is fine. Release with no
		// invalidation and surface.
		release()
		return nil, nil, err
	case errors.Is(err, upstream.ErrSessionSuperseded):
		// #159: 409 session_superseded is TERMINAL for this request —
		// another instance took over the account. Drop the cached session
		// (reason "superseded") so the NEXT request re-admits fresh, and
		// surface. NEVER retry on the dead instance.
		release()
		backend.InvalidateSessionSuperseded(lease)
		return nil, nil, err
	case errors.Is(err, upstream.ErrTurnSpendLimited):
		// turn_spend_limit killed THIS turn (per-turn spend ceiling).
		// TERMINAL for the current request: surface immediately with no
		// cooldown and no re-acquire.
		release()
		return nil, nil, err
	case errors.Is(err, upstream.ErrRunInvalid):
		release()
		backend.InvalidateRun(lease, lease.AgentID)
		return nil, nil, err
	case errors.Is(err, upstream.ErrAuthRejected):
		// No cooldown write: the account simply failed to serve.
		release()
		return nil, nil, err
	case errors.Is(err, upstream.ErrBanned):
		// Terminal ban: remember it (quarantines the account) and surface.
		var be *upstream.BanError
		if errors.As(err, &be) {
			backend.CooldownBan(lease, be)
		}
		release()
		return nil, nil, err
	case errors.Is(err, upstream.ErrRateLimited):
		// Turn-time 429: surface with no cooldown write and no failover.
		release()
		return nil, nil, err
	case errors.Is(err, upstream.ErrIpCapped):
		// Admission-only signal surfacing mid-chat: no cooldown write.
		release()
		return nil, nil, err
	case errors.Is(err, upstream.ErrCountryBlocked):
		// No cooldown write: surface.
		release()
		return nil, nil, err
	case errors.Is(err, upstream.ErrCredits):
		// #117: 402 is NEVER retried.
		release()
		return nil, nil, err
	default:
		release()
		return nil, nil, err
	}
}
