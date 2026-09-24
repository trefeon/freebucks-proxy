package server

import (
	"encoding/json"
	"fmt"
	"freebucks-proxy/backend/internal/telemetry"
	"net/http"
	"strings"
	"time"
)

// handleHealthz reports uptime, model count, the per-token snapshot, the
// cached bridge entries (bridge mode), the effective routing mode, and the
// resolved session-locality zone + source + detected egress region.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	snaps := s.pool.Snapshot()
	cfg := s.cfg.Load()
	w.Header().Set("Content-Type", "application/json")
	tokens := make([]map[string]any, 0, len(snaps))
	for _, snap := range snaps {
		tok := map[string]any{
			"Token":                snap.Token,
			"CooldownUntil":        snap.CooldownUntil,
			"SessionStatus":        snap.SessionStatus,
			"SessionInstanceID":    snap.SessionInstanceID,
			"SessionQueuePosition": snap.SessionQueuePosition,
			"SessionQueueDepth":    snap.SessionQueueDepth,
			"ActiveRuns":           snap.ActiveRuns,
			"Requests":             snap.Requests,
			"Messages24h":          snap.Messages24h,
			// Spend ledger: Pacific-day/week/month buckets plus the
			// spend_limited refusal counter. The upstream $ ceilings are
			// server-enforced; the ledger only records events.
			"Spend24h":                  snap.Spend24h,
			"SpendDay":                  snap.SpendDay,
			"SpendWeek":                 snap.SpendWeek,
			"SpendMonth":                snap.SpendMonth,
			"SpendDayStart":             snap.SpendDayStart,
			"SpendLimited":              snap.SpendLimited,
			"country":                   snap.CountryCode,
			"session_model":             snap.SessionModel,
			"session_remaining_seconds": snap.SessionRemainingSeconds,
			"quarantined":               snap.Quarantined,
			"quarantine_reason":         snap.QuarantineReason,
		}
		// Cooldown reason (additive): a distinguishable window refusal (the
		// vendor's freebucks ceiling) names WHY the token is cooling down and
		// WHEN upstream lifts it, so the operator never has to explain a
		// ~20h cooldown from the raw body. Absent for every other cooldown.
		if snap.CooldownKind != "" {
			tok["cooldown_kind"] = snap.CooldownKind
		}
		if snap.CooldownWindowHours > 0 {
			tok["cooldown_window_hours"] = snap.CooldownWindowHours
		}
		if !snap.CooldownResetsAt.IsZero() {
			tok["cooldown_resets_at"] = snap.CooldownResetsAt.UTC().Format(time.RFC3339)
		}
		if len(snap.QuotaByModel) > 0 {
			quota := make(map[string]any, len(snap.QuotaByModel))
			for model, q := range snap.QuotaByModel {
				entry := map[string]any{
					"limit":        q.Limit,
					"recent_count": q.RecentCount,
					"period":       q.Period,
				}
				if !q.ResetAt.IsZero() {
					entry["reset_at"] = q.ResetAt
				}
				if len(q.Entitlement) > 0 {
					entry["entitlement"] = q.Entitlement
				}
				quota[model] = entry
			}
			tok["quota"] = quota
		}
		if len(snap.Entitlement) > 0 {
			tok["entitlement"] = snap.Entitlement
		}
		tokens = append(tokens, tok)
	}
	// Bridge token snapshots (#187): per-entry data when in bridge mode.
	bridgeSnaps := s.pool.BridgeSnapshot()
	bridgeEntries := make([]map[string]any, 0, len(bridgeSnaps))
	for _, bs := range bridgeSnaps {
		entry := map[string]any{
			"key":            bs.Key[:min(8, len(bs.Key))] + "…",
			"locked":         bs.Locked,
			"session_active": bs.SessionActive,
			"active_runs":    bs.ActiveRuns,
			"requests":       bs.Requests,
			"model":          bs.Model,
			"spend_day":      bs.SpendDay,
		}
		if bs.CooldownUntil.After(time.Now()) {
			entry["cooldown_until"] = bs.CooldownUntil
		}
		if len(bs.QuotaByModel) > 0 {
			quota := make(map[string]any, len(bs.QuotaByModel))
			for model, q := range bs.QuotaByModel {
				qEntry := map[string]any{
					"limit":        q.Limit,
					"recent_count": q.RecentCount,
					"period":       q.Period,
				}
				if !q.ResetAt.IsZero() {
					qEntry["reset_at"] = q.ResetAt
				}
				if len(q.Entitlement) > 0 {
					qEntry["entitlement"] = q.Entitlement
				}
				quota[model] = qEntry
			}
			entry["quota"] = quota
		}
		bridgeEntries = append(bridgeEntries, entry)
	}
	// Session locality (feat/session-locality-region): which IANA zone the
	// gateway declares on session reads, which rule picked it, and the
	// detected egress country. Additive fields; the egress IP is deliberately
	// NOT exposed here — /healthz is unauthenticated and the doctor keeps the
	// IP readout.
	sessionTimezone, sessionTimezoneSource, egressRegion := s.sessionLocality()
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":                  "ok",
		"mode":                    cfg.EffectiveMode(),
		"uptime_seconds":          time.Since(s.started).Seconds(),
		"models":                  s.servedModelCount(),
		"tokens":                  tokens,
		"bridge_tokens":           s.pool.BridgeCount(),
		"bridge_entries":          bridgeEntries,
		"session_timezone":        sessionTimezone,
		"session_timezone_source": sessionTimezoneSource,
		"egress_region":           egressRegion,
	})
}

// escapeLabelValue escapes a Prometheus label value per the text exposition
// format: backslash, double quote, and newline are escaped; everything else
// passes through unchanged.
func escapeLabelValue(v string) string {
	if !strings.ContainsAny(v, `\"\n`) {
		return v
	}
	var sb strings.Builder
	for _, r := range v {
		switch r {
		case '\\':
			sb.WriteString(`\\`)
		case '"':
			sb.WriteString(`\"`)
		case '\n':
			sb.WriteString(`\n`)
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// legacyMetricNamespace is the pre-rename Prometheus namespace, re-emitted
// only by the deprecated alias section at the end of handleMetrics. It is
// derived from the live namespace rather than duplicated, so the two cannot
// drift apart.
// TODO(remove after one release)
var legacyMetricNamespace = strings.Replace("freebucks_proxy_", "freebucks", "freebuff", 1)

// handleMetrics exports Prometheus metrics (#24).
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var sb strings.Builder
	uptime := time.Since(s.started).Seconds()
	ps := s.pool.PoolSnapshot()
	snaps := ps.Tokens

	sb.WriteString("# HELP freebucks_proxy_uptime_seconds Process uptime in seconds\n")
	sb.WriteString("# TYPE freebucks_proxy_uptime_seconds gauge\n")
	fmt.Fprintf(&sb, "freebucks_proxy_uptime_seconds %.2f\n\n", uptime)

	sb.WriteString("# HELP freebucks_proxy_models_total Count of models available in registry\n")
	sb.WriteString("# TYPE freebucks_proxy_models_total gauge\n")
	fmt.Fprintf(&sb, "freebucks_proxy_models_total %d\n\n", s.servedModelCount())

	sb.WriteString("# HELP freebucks_proxy_tokens_total Count of configured tokens in pool\n")
	sb.WriteString("# TYPE freebucks_proxy_tokens_total gauge\n")
	fmt.Fprintf(&sb, "freebucks_proxy_tokens_total %d\n\n", len(snaps))

	sb.WriteString("# HELP freebucks_proxy_rate_limit_rejected_total Total client requests rejected by local rate limiter\n")
	sb.WriteString("# TYPE freebucks_proxy_rate_limit_rejected_total counter\n")
	fmt.Fprintf(&sb, "freebucks_proxy_rate_limit_rejected_total %d\n\n", s.rateLimitRejections.Load())
	sb.WriteString("# HELP freebucks_proxy_model_unavailable_skips_total Session admissions skipped via the model_unavailable window cache (issue #158)\n")
	sb.WriteString("# TYPE freebucks_proxy_model_unavailable_skips_total counter\n")
	fmt.Fprintf(&sb, "freebucks_proxy_model_unavailable_skips_total %d\n\n", telemetry.ModelUnavailableSkips.Load())
	sb.WriteString("# HELP freebucks_proxy_token_messages_24h Rolling 24h message count per token\n")
	sb.WriteString("# TYPE freebucks_proxy_token_messages_24h gauge\n")
	for _, snap := range snaps {
		fmt.Fprintf(&sb, "freebucks_proxy_token_messages_24h{token=\"%d\"} %d\n", snap.Token+1, snap.Messages24h)
	}
	sb.WriteString("\n")

	sb.WriteString("# HELP freebucks_proxy_token_requests_total Total requests served per token\n")
	sb.WriteString("# TYPE freebucks_proxy_token_requests_total counter\n")
	for _, snap := range snaps {
		fmt.Fprintf(&sb, "freebucks_proxy_token_requests_total{token=\"%d\"} %d\n", snap.Token+1, snap.Requests)
	}
	sb.WriteString("\n")

	sb.WriteString("# HELP freebucks_proxy_token_active_runs Active agent runs per token\n")
	sb.WriteString("# TYPE freebucks_proxy_token_active_runs gauge\n")
	for _, snap := range snaps {
		fmt.Fprintf(&sb, "freebucks_proxy_token_active_runs{token=\"%d\"} %d\n", snap.Token+1, snap.ActiveRuns)
	}
	sb.WriteString("\n")

	sb.WriteString("# HELP freebucks_proxy_token_cooldown_active Is token currently cooling down (1=yes, 0=no)\n")
	sb.WriteString("# TYPE freebucks_proxy_token_cooldown_active gauge\n")
	now := time.Now()
	for _, snap := range snaps {
		cd := 0
		if !snap.CooldownUntil.IsZero() && now.Before(snap.CooldownUntil) {
			cd = 1
		}
		fmt.Fprintf(&sb, "freebucks_proxy_token_cooldown_active{token=\"%d\"} %d\n", snap.Token+1, cd)
	}
	sb.WriteString("\n")

	sb.WriteString("# HELP freebucks_proxy_quota_recent Current usage toward the per-model quota window\n")
	sb.WriteString("# TYPE freebucks_proxy_quota_recent gauge\n")
	for _, snap := range snaps {
		for model, q := range snap.QuotaByModel {
			fmt.Fprintf(&sb, "freebucks_proxy_quota_recent{token=\"%d\",model=\"%s\",period=\"%s\"} %g\n",
				snap.Token+1, escapeLabelValue(model), escapeLabelValue(q.Period), q.RecentCount)
		}
	}
	sb.WriteString("\n")

	sb.WriteString("# HELP freebucks_proxy_quota_limit Per-model quota limit for the window\n")
	sb.WriteString("# TYPE freebucks_proxy_quota_limit gauge\n")
	for _, snap := range snaps {
		for model, q := range snap.QuotaByModel {
			fmt.Fprintf(&sb, "freebucks_proxy_quota_limit{token=\"%d\",model=\"%s\",period=\"%s\"} %g\n",
				snap.Token+1, escapeLabelValue(model), escapeLabelValue(q.Period), q.Limit)
		}
	}

	sb.WriteString("# HELP freebucks_proxy_quota_remaining Remaining allowance toward the per-model quota window\n")
	sb.WriteString("# TYPE freebucks_proxy_quota_remaining gauge\n")
	for _, snap := range snaps {
		for model, q := range snap.QuotaByModel {
			rem := float64(0)
			if q.Limit > 0 {
				rem = q.Limit - q.RecentCount
				if rem < 0 {
					rem = 0
				}
			}
			fmt.Fprintf(&sb, "freebucks_proxy_quota_remaining{token=\"%d\",model=\"%s\",period=\"%s\"} %g\n",
				snap.Token+1, escapeLabelValue(model), escapeLabelValue(q.Period), rem)
		}
	}
	sb.WriteString("\n")

	sb.WriteString("# HELP freebucks_proxy_session_remaining_seconds Remaining time in seconds for the active session\n")
	sb.WriteString("# TYPE freebucks_proxy_session_remaining_seconds gauge\n")
	for _, snap := range snaps {
		if snap.SessionModel != "" && snap.SessionRemainingSeconds > 0 {
			fmt.Fprintf(&sb, "freebucks_proxy_session_remaining_seconds{token=\"%d\",model=\"%s\"} %d\n",
				snap.Token+1, escapeLabelValue(snap.SessionModel), snap.SessionRemainingSeconds)
		}
	}
	sb.WriteString("\n")

	sb.WriteString("# HELP freebucks_proxy_transient_retries_total Transient transport failures retried per token\n")
	sb.WriteString("# TYPE freebucks_proxy_transient_retries_total counter\n")
	for _, snap := range snaps {
		if snap.TransientRetries > 0 {
			fmt.Fprintf(&sb, "freebucks_proxy_transient_retries_total{token=\"%d\"} %d\n", snap.Token+1, snap.TransientRetries)
		}
	}
	sb.WriteString("\n")
	sb.WriteString("# HELP freebucks_proxy_queue_retries_total Same-session transient-queue retries per token, split by queue\n")
	sb.WriteString("# TYPE freebucks_proxy_queue_retries_total counter\n")
	for _, snap := range snaps {
		if snap.CapacityDeferredRetries > 0 {
			fmt.Fprintf(&sb, "freebucks_proxy_queue_retries_total{token=\"%d\",queue=\"capacity_deferred\"} %d\n", snap.Token+1, snap.CapacityDeferredRetries)
		}
		if snap.WaitingRoomRetries > 0 {
			fmt.Fprintf(&sb, "freebucks_proxy_queue_retries_total{token=\"%d\",queue=\"waiting_room\"} %d\n", snap.Token+1, snap.WaitingRoomRetries)
		}
	}
	sb.WriteString("\n")

	sb.WriteString("# HELP freebucks_proxy_fingerprint_rotations_total TLS fingerprint rotations per token\n")
	sb.WriteString("# TYPE freebucks_proxy_fingerprint_rotations_total counter\n")
	for _, snap := range snaps {
		if snap.FingerprintRotations > 0 {
			fmt.Fprintf(&sb, "freebucks_proxy_fingerprint_rotations_total{token=\"%d\"} %d\n", snap.Token+1, snap.FingerprintRotations)
		}
	}
	sb.WriteString("\n")

	sb.WriteString("# HELP freebucks_proxy_rate_limit_events_total Upstream rate-limit classifications per token and code\n")
	sb.WriteString("# TYPE freebucks_proxy_rate_limit_events_total counter\n")
	for _, snap := range snaps {
		for code, n := range snap.RateLimitEvents {
			if n > 0 {
				fmt.Fprintf(&sb, "freebucks_proxy_rate_limit_events_total{token=\"%d\",code=\"%s\"} %d\n",
					snap.Token+1, escapeLabelValue(code), n)
			}
		}
	}
	sb.WriteString("\n")

	sb.WriteString("# HELP freebucks_proxy_model_locked_total Session releases on model lock, by model switch (from to)\n")
	sb.WriteString("# TYPE freebucks_proxy_model_locked_total counter\n")
	for _, snap := range snaps {
		for from, tos := range snap.ModelLocked {
			for to, n := range tos {
				if n > 0 {
					fmt.Fprintf(&sb, "freebucks_proxy_model_locked_total{token=\"%d\",from=\"%s\",to=\"%s\"} %d\n",
						snap.Token+1, escapeLabelValue(from), escapeLabelValue(to), n)
				}
			}
		}
	}
	sb.WriteString("\n")
	sb.WriteString("# HELP freebucks_proxy_pin_skips_total Acquire-time single-pin skips per token (PIN_MODEL)\n")
	sb.WriteString("# TYPE freebucks_proxy_pin_skips_total counter\n")
	for _, snap := range snaps {
		if snap.PinSkips > 0 {
			fmt.Fprintf(&sb, "freebucks_proxy_pin_skips_total{token=\"%d\"} %d\n",
				snap.Token+1, snap.PinSkips)
		}
	}
	sb.WriteString("\n")
	if s.logs != nil {
		// T20: handled-record counters from the dashboard log ring. The key
		// is logring's "level|msg" (level lowercased). msg is a free-form
		// operator message, so the label is escaped like every upstream-
		// derived label.
		sb.WriteString("# HELP freebucks_proxy_log_events_total Log records handled per level and message\n")
		sb.WriteString("# TYPE freebucks_proxy_log_events_total counter\n")
		for key, n := range s.logs.Counts() {
			level, msg, ok := strings.Cut(key, "|")
			if !ok {
				continue
			}
			fmt.Fprintf(&sb, "freebucks_proxy_log_events_total{level=\"%s\",msg=\"%s\"} %d\n",
				escapeLabelValue(level), escapeLabelValue(msg), n)
		}
		sb.WriteString("\n")
	}

	// Deprecated: every family above is re-emitted below under the pre-rename
	// product prefix, so existing scrapers and dashboards keep resolving
	// during the transition. Same bodies — one emit path, no second copy to
	// keep in sync.
	// TODO(remove after one release): delete this alias section.
	sb.WriteString("# Deprecated: aliases of the current namespace under the pre-rename product prefix; TODO(remove after one release)\n")
	sb.WriteString(strings.ReplaceAll(sb.String(), "freebucks_proxy_", legacyMetricNamespace))

	_, _ = w.Write([]byte(sb.String()))
}
