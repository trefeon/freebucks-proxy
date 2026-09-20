package server

import (
	"errors"
	"fmt"
	"freebucks-proxy/backend/internal/phasetiming"
	"freebucks-proxy/backend/internal/pool"
	"freebucks-proxy/backend/internal/store"
	"strconv"
	"strings"
	"time"
)

// traceChat records a structured "chat trace" entry for the dashboard
// traces page (the page filters the shared log ring by msg == "chat trace").
// phases carries the per-request latency phases (#89); the map is ordered
// deterministically for stable log output. st carries the retry-once
// attempt history (nil-safe: a refusal before any chat attempt passes a
// zero state).
func (s *Server) traceChat(lease *pool.Lease, model string, ms int64, status, errClass string, phases map[string]int64, st *chatTraceState) {
	attrs := []any{"model", model, "status", status, "ms", ms}
	if st != nil {
		if st.reqID != "" {
			attrs = append(attrs, "req_id", st.reqID)
		}
		if st.clientRequestID != "" {
			attrs = append(attrs, "client_request_id", st.clientRequestID)
		}
		if st.attempts > 0 {
			attrs = append(attrs, "attempts", st.attempts)
		}
		if seen := st.statusesSeen(); seen != "" {
			attrs = append(attrs, "statuses_seen", seen)
		}
		if st.retried {
			attrs = append(attrs, "retried", true, "backoff_ms", st.backoffMs)
		}
	}
	if lease != nil {
		attrs = append(attrs,
			"token", tokenLabel(lease),
			"agent", lease.AgentID,
			"trace_session_id", lease.Run.TraceSessionID,
		)
	} else if st != nil {
		// No lease was ever held (acquire failure or pre-attempt
		// refusal). Two nil-lease failures still carry token attribution:
		// a post-acquire upstream error reports the failed lease's token
		// (chatAttempt releases before returning), and an acquire-time
		// rate limit reports the binding token plus the limited set.
		// Egress refusals (model_ip_limited) and every other no-token
		// path leave both empty, so the row keeps TOKEN —.
		if st.failedToken != "" {
			attrs = append(attrs, "token", st.failedToken)
			if st.failedAgent != "" {
				attrs = append(attrs, "agent", st.failedAgent)
			}
		} else if st.rateToken != "" {
			attrs = append(attrs, "token", st.rateToken)
			if st.rateTokens != "" {
				attrs = append(attrs, "rate_tokens", st.rateTokens)
			}
		}
	}
	if errClass != "" {
		attrs = append(attrs, "error", errClass)
	}
	// Token split for the traces enrichment: integer attrs under the exact
	// UsageRecord key names, logged only when the relay observed a real
	// usage block (absent otherwise, so the trace renders no token line).
	if st != nil && st.usageTotal > 0 {
		attrs = append(attrs,
			"input", st.usageInput,
			"output", st.usageOutput,
			"cached", st.usageCached,
			"reasoning", st.usageReasoning,
			"total", st.usageTotal,
		)
	}
	for _, name := range []string{
		phasetiming.AcquireMS,
		phasetiming.SessionRefreshMS,
		phasetiming.RunAcquireMS,
		phasetiming.QueueWaitMS,
		phasetiming.UpstreamTTFBMS,
		phasetiming.TotalMS,
	} {
		if v, ok := phases[name]; ok {
			attrs = append(attrs, name, v)
		}
	}
	s.logger.Info("chat trace", attrs...)
	s.recordRequestOutcome(lease, model, status, errClass, phases, st)
}

// recordRequestOutcome persists one /v1 inference outcome to the history
// store for the Logs console view. It runs on the chat path but performs a
// single indexed upsert and never fails the request: a nil store skips the
// write (live-only), a missing req_id skips it (the PRIMARY KEY cannot
// distinguish pre-attempt refusals — the ring log still carries them), and
// insert errors only warn. Raw client tokens never reach the store: the
// lease's token index (bridge = -1) is the only token signal recorded.
func (s *Server) recordRequestOutcome(lease *pool.Lease, model string, status, errClass string, phases map[string]int64, st *chatTraceState) {
	if s.hist == nil || st == nil || st.reqID == "" {
		return
	}
	tokenIdx := -1
	if lease != nil {
		tokenIdx = lease.Token
	}
	var ttfb int64
	if phases != nil {
		ttfb = phases[phasetiming.UpstreamTTFBMS]
	}
	// clientKeyHash is the pooled API-key identity (hex(sha256)[:16]),
	// "" for bridge/no-key — finalized by chatCore after routing. The raw
	// key never reaches the store.
	if err := s.hist.RecordRequest(store.RequestRecord{
		ReqID:         st.reqID,
		TS:            store.Millis(time.Now()),
		Endpoint:      "/v1/chat/completions",
		Model:         model,
		TokenIdx:      tokenIdx,
		Status:        status,
		TTFBms:        ttfb,
		Err:           errClass,
		ClientKeyHash: st.clientKeyHash,
	}); err != nil {
		s.logger.Warn("request record failed", "err", err, "req_id", st.reqID)
	}
}

// chatDoneAttrs builds the structured log attributes for a completed chat,
// including reasoning effort when the client requested it.
func chatDoneAttrs(reqID, model, agent string, stream bool, ms int64, chunks, bytes int, reasoningEffort string) []any {
	attrs := []any{
		"req_id", reqID,
		"model", model,
		"agent", agent,
		"stream", stream,
		"ms", ms,
		"bytes", bytes,
	}
	if stream {
		attrs = append(attrs, "chunks", chunks)
	}
	if reasoningEffort != "" {
		attrs = append(attrs, "reasoning_effort", reasoningEffort)
	}
	return attrs
}

// chatTraceState accumulates the per-request attempt history for the chat
// trace line: how many upstream chat attempts fired, the HTTP statuses
// observed per attempt (success = 200), whether the retry-once recovery
// re-acquired a lease, and the measured re-acquire wait before the retry.
// Created in chatCore (which owns the req_id), filled by chatAttempt's
// retry loop.
type chatTraceState struct {
	reqID           string
	clientRequestID string
	attempts        int
	statuses        []int
	retried         bool
	backoffMs       int64
	// usageInput/usageOutput/usageCached/usageReasoning/usageTotal carry
	// the completed chat's token split onto the "chat trace" line for the
	// traces enrichment (dashboard parses the keys verbatim). Zero value
	// = no usage observed: the line omits the keys entirely.
	usageInput     int64
	usageOutput    int64
	usageCached    int64
	usageReasoning int64
	usageTotal     int64
	// failedToken/failedAgent carry the lease attribution of a
	// post-acquire chat error: chatAttempt releases the lease before
	// returning, so without these the trace would lose the token that
	// actually served (and failed) the attempt. Rendered 1-based via
	// tokenLabel ("bridge" for bridge leases). Empty = no lease was
	// held when the request failed (acquire-time failure).
	failedToken string
	failedAgent string
	// rateToken/rateTokens carry an acquire-time rate-limit's token
	// attribution (pool returned nil lease): the 1-based binding token
	// whose window bounds the wait, plus the comma-joined 1-based set
	// of all currently rate-limited tokens for the model. Empty =
	// unknown (egress refusals keep TOKEN —).
	rateToken  string
	rateTokens string
	// clientKeyHash is the pooled client API-key identity for this
	// request (hex(sha256(rawKey))[:16]), "" for bridge/no-key requests.
	// Finalized by chatCore after the pooled-vs-bridge decision; the
	// usage ring (via the request context) and the request_records row
	// (via recordRequestOutcome) both read this same value.
	clientKeyHash string
}

// statusesSeen renders the observed attempt statuses comma-joined
// ("409,200"), or "" when no attempt status was observed.
func (st *chatTraceState) statusesSeen() string {
	if len(st.statuses) == 0 {
		return ""
	}
	parts := make([]string, len(st.statuses))
	for i, s := range st.statuses {
		parts[i] = strconv.Itoa(s)
	}
	return strings.Join(parts, ",")
}

// tokenLabel renders the lease's token for logging: "bridge" for bridge
// leases, the 1-based fixed-token index otherwise.
func tokenLabel(lease *pool.Lease) string {
	if lease == nil || lease.Bridge != nil {
		return "bridge"
	}
	return fmt.Sprintf("%d", lease.Token+1)
}

// accessTokenLabel resolves the token identity for the access line from the
// chat outcome: the serving lease's label when held, else the failed
// attempt's lease attribution (post-acquire errors release before
// returning), else the acquire-time rate-limit binding token. "" when no
// token was ever attributable (auth 401s, egress refusals, missing bridge
// credential) — the caller then leaves the access "token" field absent and
// the detail stays ring-only. Labels are 1-based indices or "bridge";
// raw keys never appear here.
func accessTokenLabel(lease *pool.Lease, st *chatTraceState) string {
	if lease != nil {
		return tokenLabel(lease)
	}
	if st != nil {
		if st.failedToken != "" {
			return st.failedToken
		}
		if st.rateToken != "" {
			return st.rateToken
		}
	}
	return ""
}

// setRateAttribution records an acquire-time rate limit's token
// attribution onto the trace state from the pool's wrapped 429 (nil-lease
// path in chatCore). rateToken is the 1-based binding token; rateTokens
// the comma-joined 1-based limited set, or the binding token alone when
// enumeration was unavailable. Non-rate errors and unknown bindings leave
// the state untouched so those rows keep TOKEN —. Nil-safe.
func setRateAttribution(st *chatTraceState, err error) {
	if st == nil || err == nil {
		return
	}
	var are *pool.AcquireRateLimitedError
	if !errors.As(err, &are) || are == nil || are.Token < 0 {
		return
	}
	st.rateToken = strconv.Itoa(are.Token + 1)
	if len(are.LimitedTokens) == 0 {
		st.rateTokens = st.rateToken
		return
	}
	parts := make([]string, 0, len(are.LimitedTokens))
	for _, idx := range are.LimitedTokens {
		if idx < 0 {
			continue
		}
		parts = append(parts, strconv.Itoa(idx+1))
	}
	if len(parts) == 0 {
		st.rateTokens = st.rateToken
		return
	}
	st.rateTokens = strings.Join(parts, ",")
}
