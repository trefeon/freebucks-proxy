# ACCOUNT-REQUEST — credential→slot/seat→EnsureSession→run START/FINISH→chat POST→Release

> Version: live CLI 0.0.194 vs static pin 0.0.193. No tokens/hosts. Proxy paths are repo backend (current), CLI legs are pin-0.0.193 vendor tree.

## Flow
Credential: AUTH_TOKENS pool or per-request bridge token (backend/internal/server/engine.go routing). Session: EnsureSessionForModel fast-reuse (active to expiresAt-5s else grace 30m) else POST /api/v1/freebuff/session/admission with x-freebuff-model/wallet-0/tz/first-tab-0 (backend/internal/upstream/session.go:87-100); GET poll with instance+compact (:116-131); DELETE release (:302-346). Single-flight refresh, re-admit lead + seat gate, instance-guarded invalidate, never auto-takeover on superseded (backend/internal/session/session_manager.go, session_admission.go, session.go, grace.go). Run: RunManager per-agent acquire/rotate (6h), START/FINISH /api/v1/agent-runs (session.go:362-486), per-run RunID/TraceSessionID/ClientID, llm_step_number, envelope codebuff_metadata + deny + stream + Buffy marker (chat.go:409-517). Chat POST /api/v1/chat/completions Bearer + ai-sdk UA only (chat.go:110,130); in-place same-session retry for capacity_deferred/waiting-room under TRANSIENT_RETRIES (chat.go:91-208); transport retry fresh-conn (client_chat.go, client.go).

## Pooled vs bridge
- Pooled: spill walk, slot park, admitOnLane session+run grant, cooldown/quarantine (backend/internal/pool/acquire_route.go, cooldown.go, slot_ledger.go). Slots SLOTS_PER_ACCOUNT 2/(account,model) + FIFO QUEUE_WAIT 30s/DEPTH 16 + MAX_SPILL; seat gates re-admit. PIN_MODEL fail-fast allPinnedOut (acquire_route.go:139-150).
- Bridge: per-client-token entry, single-flight admission, slot+seat (backend/internal/pool/bridge.go, bridge_cache.go). Single-token relay shares chatAttempt/chatCore with pooled (backend/internal/server/engine_attempt.go:120).
- Request path: reg.ResolveModel alias/suffix strip (registry_resolve.go:14) + AgentForModel (:57) → modelAllowed=IsServed gate (server/models.go:55-64; refusal WithdrawnModelMessage vs supported dump, :20-48) → pool.Acquire(model) (pool/acquire_route.go:121) → session ensure + StartRun → chatAttempt (engine_attempt.go:120): effectiveModel=lease.Model rewrite + NormalizeRequest, opts{RunID,SessionInstanceID,TraceSessionID,ClientID per-run,AgentID,StepNumber=NextStepNumber(),RequestID} (:142-163) → upstream.ChatCompletions same-session transient retry (chat.go:91-208); run-invalid retried once with fresh run (engine_attempt.go:255-258) → relayReadLoop SSE verbatim (server/engine_sse.go:105) + usage-chunk spend ledger RecordSpend/RecordRunStep (pool/lifecycle.go:84,118; server/openai_chunk_pipeline.go:162) → LeaseRelease (or LeaseAbandon→FINISH cancelled on client cancel, :165-184).

## Error table (classifyError → chatAttempt invalidate → writeError HTTP mirror)
- Source: backend/internal/upstream/classify.go, errors.go; engine_attempt.go, errors.go, error_taxonomy.go.
- 401 → ErrAuthRejected; 403 banned/country_blocked terminal (ErrBanned/ErrCountryBlocked, probe maps session.go:150-152); 409 session_superseded → InvalidateSessionSuperseded terminal; 428 waiting-room-required invalidate; session-invalid invalidate; turn_spend_limit terminal no failover; 429/quota surface verbatim Retry-After, no local cooldown on chat path, no cooldown memory under MASQ (admission owns quota authority).

Sample (redacted): `POST {API_ORIGIN}/api/v1/chat/completions` H `Authorization: Bearer <redacted>`, `User-Agent: ai-sdk/openai-compatible/1.0.0/codebuff`; body `{"model":"<id>","stream":true,"codebuff_metadata":{"run_id":"<redacted>","client_id":"<redacted>","trace_session_id":"<redacted>","freebuff_instance_id":"<redacted>","llm_step_number":"1","cost_mode":"free"}}`.

## UNVERIFIED
- Server gate/envelope behavior (no web/ dir in vendor clone — CLI+SDK call sites only).
- Live 0.0.194 error-copy deltas; upstream quota authoritative values (capture-gated).
