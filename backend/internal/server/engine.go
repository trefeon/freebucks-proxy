package server

import (
	"context"
	"errors"
	"freebucks-proxy/backend/internal/convert"
	"freebucks-proxy/backend/internal/phasetiming"
	"freebucks-proxy/backend/internal/pool"
	"freebucks-proxy/backend/internal/upstream"
	"io"
	"net/http"
	"time"
)

// --- Shared completion engine (protocol-neutral) ---
//
// The acquire→upstream→relay core every completion surface runs on:
// chatCore (lease acquisition, bridge routing, the ErrRunInvalid
// rotate-and-retry-once, phase timing, endpoint log lines), chatAttempt
// (one acquire→chat attempt with session/run invalidation and token
// cooldowns on refusal), and the plain SSE plumbing every relay shares
// (relayReadLoop, lineChunk, keepaliveInterval). Protocol policy does NOT
// live here — each surface's handler, wire translation, stream relay and
// error envelope live in its own file:
//
//	openai.go            /v1/chat/completions handler (+ openai_stream.go relays)
//	responses.go         /v1/responses handler (+ responses_stream.go relays)
//	anthropic.go         /v1/messages handler (+ anthropic_stream.go,
//	                     anthropic_json.go, anthropic_count.go, anthropic_errors.go,
//	                     streamxml_anthropic.go)
//	models.go            /v1/models catalog
//
// The relay plumbing shared by every surface lives in engine_sse.go
// (relayReadLoop, lineChunk, keepaliveInterval, relayStats).
//
// Kindly keep it that way: a protocol-specific branch added here is a
// debugging regression — the entire point of the split is that an Anthropic
// bug never requires reading OpenAI relay code and vice versa.

// relayFunc relays the upstream SSE reader to the client in the endpoint's
// wire format (chat.completion chunks, Responses events, or Anthropic
// events). Implementations set their own headers, flush, and write terminal
// frames. chatStart is when the upstream chat call returned; the first
// relayed chunk records the upstream TTFB phase.
type relayFunc func(ctx context.Context, w http.ResponseWriter, up io.Reader, stats *relayStats, chatStart time.Time)

// timedBackend wraps a chatBackend so the token-acquisition phase is
// recorded into the request's phasetiming accumulator, mirroring the
// acquireTimed closure chatCore used to thread into chatAttempt.
type timedBackend struct {
	chatBackend
	phases *phasetiming.Phases
}

func (b *timedBackend) Acquire(ctx context.Context, model string) (*pool.Lease, error) {
	start := time.Now()
	l, err := b.chatBackend.Acquire(ctx, model)
	b.phases.Since(phasetiming.AcquireMS, start)
	return l, err
}

// handleChat is the OpenAI chat-completions entry point: sanitize the
// chatCore is the shared acquire→relay core for every completion-style
// endpoint (chat completions, Responses, Anthropic messages): acquire a
// token lease (bridge routing included), call upstream with
// retry-once recovery, then relay the forced stream to the client through
// relay. kind names the endpoint in request/done log lines.
func (s *Server) chatCore(w http.ResponseWriter, r *http.Request, model string, stream bool, normalized []byte, reasoningEffort, kind string, relay relayFunc) {
	// Issue #140: the tool-name tolerance map. The handlers normalize
	// with NormalizeRequestMapped, which renames mapped client tools to
	// official signature names IN the normalized body; the mapper that maps
	// them BACK is rebuilt here from the client's ORIGINAL body so response
	// relays can restore names the client dispatched on.
	toolMap := convert.NewToolMapper(originalBodyFromContext(r.Context()))
	// D1: the access wrapper minted the request's correlation id; direct
	// handler calls (tests) mint here so it is never empty. The value is
	// threaded into the request context AND into ChatOptions.RequestID so
	// the upstream client's do()/retry lines share it.
	reqID := reqIDFrom(r.Context())
	if reqID == "" {
		reqID = newReqID()
	}
	st := &chatTraceState{reqID: reqID, clientRequestID: clientRequestID(r)}
	ctx, phases := phasetiming.WithContext(context.WithValue(r.Context(), reqIDKey{}, reqID))
	start := time.Now()

	agentID, _ := s.reg.AgentForModel(model)
	reqAttrs := []any{
		"req_id", reqID,
		"model", model,
		"agent", agentID,
		"stream", stream,
		"remote", remoteHost(r),
		"msgs", toolMap.MsgCount(),
		"tools", toolMap.ToolCount(),
	}
	if reasoningEffort != "" {
		reqAttrs = append(reqAttrs, "reasoning_effort", reasoningEffort)
	}
	s.logger.Info(kind+" request", reqAttrs...)
	// Client-side per-IP rate limiting runs in the OUTERMOST request wrapper
	// (Handler): it must cover every /v1/* surface with a single bucket, so
	// this core deliberately does not re-limit — direct handler calls (unit
	// tests) skip the limiter on purpose.
	// Bridge routing: bridge mode relays the client's Authorization header
	// as the upstream token.  No token in bridge → 401 before touching
	// the pool.
	var up io.ReadCloser
	var lease *pool.Lease
	// One request, one snapshot: requireAuth pinned the config it made its
	// pass-through decision with into the request context; chatCore and
	// authorized route from that same view, so a config swap mid-request
	// cannot split the pooled-vs-bridge decision across two configs.
	cfg := cfgSnapshotFrom(r.Context())
	if cfg == nil {
		// No stamped snapshot (direct handler calls in tests): load live.
		cfg = s.cfg.Load()
	}
	tok := bearerToken(r)
	bridge := false
	// Hybrid (default when AUTH_TOKENS set): the pool and the bridge share
	// one instance. A credential matching API_KEYS uses the pool; any other
	// credential is relayed upstream as a bridge token. With no API_KEYS
	// configured every request uses the pool (the historic open behavior),
	// and a missing credential is rejected exactly like pure pooled mode.
	switch {
	case cfg.BridgeMode():
		// Bridge: the client token is the only upstream credential.
		bridge = true
		tok = clientToken(r)
	case cfg.HybridBridgeMode():
		provided := clientToken(r)
		if provided == "" && len(cfg.APIKeys) > 0 {
			s.writeClientError(w, r, http.StatusUnauthorized,
				"Authentication required: send a valid API key for pooled access, or your FreeBuff token for bridge mode",
				"missing_bearer_token", 0)
			return
		}
		if provided != "" && len(cfg.APIKeys) > 0 && !s.authorized(cfg, r) {
			bridge = true
			tok = provided
		}
	}
	// Key identity for usage tracking: pooled requests attribute the
	// caller's API-key hash; bridge requests (the credential IS the
	// upstream token, never a pooled identity) attribute "". Re-stamp the
	// derived context so recordUsage below reads the routing decision,
	// not requireAuth's pre-routing best effort; the trace state carries
	// the same value to the request-record persist path.
	clientKeyHash := ""
	if !bridge {
		if ok, hash := s.authorizedWithIdentity(cfg, r); ok {
			clientKeyHash = hash
		}
	}
	ctx = withClientKeyHash(ctx, clientKeyHash)
	st.clientKeyHash = clientKeyHash
	// The chatBackend abstracts the pooled-vs-bridge acquire/chat/invalidate/
	// cooldown/lease hooks (issue #255); the timing wrapper records the
	// acquire phase.
	var err error
	var be chatBackend
	if bridge {
		if tok == "" {
			s.writeClientError(w, r, http.StatusUnauthorized,
				"bridge mode: send your FreeBuff token in Authorization: Bearer <token>, x-api-key, or anthropic-api-key",
				"missing_bearer_token", 0)
			return
		}
		be = bridgeBackend{p: s.pool, token: tok}
	} else {
		be = pooledBackend{p: s.pool}
	}
	be = &timedBackend{chatBackend: be, phases: phases}
	up, lease, err = s.chatAttempt(ctx, model, normalized, st, be)
	if err != nil && ctx.Err() == nil && errors.Is(err, upstream.ErrRunInvalid) {
		// The lease's agent run is gone upstream (e.g. a resumed run whose FINISH
		// raced this request: live 2026-09-21T07:05:05Z, upstream 400 runId Not
		// Running surfaced as a bare 502). chatAttempt already Invalidated the run
		// (in-memory + persisted record removed), so the re-acquire below cannot
		// re-adopt it: rotate-and-retry-once, the sentinel's documented contract
		// (upstream/errors.go).
		st.retried = true
		up, lease, err = s.chatAttempt(ctx, model, normalized, st, be)
	}
	if err != nil {
		// Acquire-time rate limit (pool returned nil lease): attribute the
		// binding token + limited set onto the trace line. Post-acquire
		// errors already carry the failed lease via chatAttempt; egress
		// refusals carry neither and keep TOKEN —.
		if lease == nil {
			setRateAttribution(st, err)
		}
		// Surface the attributable token (serving/failed lease, else
		// rate-limit binding) on the access line; unattributable
		// refusals stash "" and stay ring-only.
		stashAccessToken(r.Context(), accessTokenLabel(lease, st))
		phases.Since(phasetiming.TotalMS, start)
		s.traceChat(lease, model, time.Since(start).Milliseconds(), "error", chatErrClass(err), phases.All(), st)
		// Issue #114: a chat that died on a terminal upstream error must
		// not leave its run FINISHing as completed — report it honestly
		// (nil-safe: an acquire failure leaves no lease).
		be.MarkRunFailed(lease)
		s.writeError(w, r, err, model, lease)
		return
	}
	defer func() { _ = up.Close() }()
	// Issue #53: when the downstream client disconnects mid-stream, abandon
	// the lease instead of a plain release — the run is FINISHed through the
	// bounded queue (last-in-flight only) so upstream does not keep an
	// abandoned agent run alive until the 6h rotation. A normal completion
	// releases the lease as before.
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		if r.Context().Err() != nil {
			be.LeaseAbandon(lease)
			return
		}
		be.LeaseRelease(lease)
	}
	defer release()
	// Token identity for the access line (abusive-key triage without
	// joining the routing log): the serving lease's label, stashed before
	// the relay so an early client disconnect still attributes.
	stashAccessToken(r.Context(), accessTokenLabel(lease, st))

	routingAttrs := []any{
		"req_id", reqID,
		"token", tokenLabel(lease),
		"model", model,
		"agent", lease.AgentID,
		"instance_id", lease.SessionInstanceID,
	}
	if reasoningEffort != "" {
		routingAttrs = append(routingAttrs, "reasoning_effort", reasoningEffort)
	}
	// Served-model transparency (issue #164, narrowed): x-freebuff-served-model
	// names the model this lease's session/run is actually bound to —
	// requested model when served directly — and is set on every successful
	// response so clients can always tell what served them.
	servedModel := lease.Model
	if servedModel == "" {
		servedModel = model
	}
	w.Header().Set("X-FreeBuff-Served-Model", servedModel)
	if servedModel != model {
		// Issue #230: upstream coercion transparency. When upstream binds the
		// session to a different model (e.g. limited-tier token coerced to mimo),
		// surface served_model in the INFO routing log so operators immediately
		// see the requested->served redirection.
		routingAttrs = append(routingAttrs, "served_model", servedModel)
	}
	s.logger.Info(kind+" routing", routingAttrs...)

	chatStart := time.Now()
	stats := &relayStats{servedModel: servedModel, toolMap: toolMap}
	relay(ctx, w, up, stats, chatStart)
	// Issue #114: record the completed chat as a run step — steps are
	// batched in memory and sent WITH FINISH (the CLI has no /steps
	// endpoint). The response message id is not extracted from the stream;
	// the CLI step schema allows a null messageId.
	be.RecordRunStep(lease, "")
	// Issue #122: feed the per-token spend ledger once per successful chat
	// completion with the usage total observed by the relay (0 when the
	// upstream stream carried none — RecordSpend ignores non-positive).
	be.RecordSpend(lease, stats.usageTokens)
	// Usage log: persist the same completion's token split into the
	// dashboard ring (unconditional — zero-usage completions still count
	// as requests with OK=false; nil-dash safe).
	s.recordUsage(ctx, stats, model)
	// Traces enrichment: carry the split onto the "chat trace" line below
	// (traceChat omits the keys when no usage block was observed).
	st.usageInput, st.usageOutput, st.usageCached, st.usageReasoning, st.usageTotal = stats.usageInput, stats.usageOutput, stats.usageCached, stats.usageReasoning, stats.usageTokens
	phases.Since(phasetiming.TotalMS, start)
	ms := time.Since(start).Milliseconds()
	s.logger.Info(kind+" done", chatDoneAttrs(reqID, model, lease.AgentID, stream, ms, stats.chunks, stats.bytes, reasoningEffort)...)
	s.traceChat(lease, model, ms, "ok", "", phases.All(), st)
}
