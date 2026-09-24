package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"freebucks-proxy/backend/internal/pool"
	"freebucks-proxy/backend/internal/session"
	"freebucks-proxy/backend/internal/upstream"
)

// openAIErrorType maps an internal error code to the OpenAI error `type`
// field at the call sites that route through writeClientError. The shared
// handler needs a single OpenAI shape; the type is derived from the code
// so every site keeps its historical categorization.
func openAIErrorType(status int, code string) string {
	switch code {
	case "rate_limit_exceeded":
		return "rate_limit_exceeded"
	case "missing_bearer_token":
		return "invalid_request_error"
	case "strict_violation", "invalid_tool_arguments":
		// Strict tool-calling contract failures (strict_tools.go) are
		// client request errors, never upstream failures.
		return "invalid_request_error"
	default:
		return "upstream_error"
	}
}

func defaultHintForCode(code, message string) string {
	lowerMsg := strings.ToLower(message)
	switch {
	case code == "free_mode_cli_required" || strings.Contains(lowerMsg, "free_mode_cli_required"):
		return "Upstream free tier gate requires official CLI traffic envelope. See FAQ: https://github.com/trefeon/freebucks-proxy#faq"
	case code == "free_mode_invalid_agent_hierarchy" || strings.Contains(lowerMsg, "free_mode_invalid_agent_hierarchy"):
		return "Upstream hierarchy gate rejected the subagent (not in its root's allowlist). Retry with a root agent id from the registry."
	case code == "free_mode_cost_mode_required" || strings.Contains(lowerMsg, "free_mode_cost_mode_required"):
		return `A Freebuff agent id arrived with codebuff_metadata.cost_mode != "free". Send cost_mode = "free", or pick a non-Freebuff agent id.`
	case code == "free_mode_unavailable" || strings.Contains(lowerMsg, "free_mode_unavailable"):
		return "Free-tier region/egress gate (403, terminal). Anonymous-network blocks: disable VPN/proxy/Tor and retry; recent_limited_country: verify at freebuff.com/account?tab=country. Never a token problem — do not rotate keys."
	case code == "provider_usage_exhausted":
		return "Freebuff's shared provider account needs a refill — operator-side, not your credits. Do not buy credits; retry with backoff."
	case code == "consent_required":
		return "Wallet balance moved since the spend limit was confirmed — re-pick the model to confirm the wallet spend."
	case code == "first_tab_discount_changed":
		return "Stale first-tab quote, nothing charged — pick the model again from the menu."
	case code == "model_locked":
		return "The account holds an active session on another model — end it first, then pick again. The proxy never auto-switches models."
	case code == "free_mode_legacy_luna_agent" || strings.Contains(lowerMsg, "free_mode_legacy_luna_agent"):
		return "Retired Luna agent — new session required, retry immediately."
	case code == "free_mode_rate_limited" || strings.Contains(lowerMsg, "free_mode_rate_limited"):
		return "Free-tier sliding window rate limit (30m). Wait for Retry-After or retry with backoff."
	case code == "free_mode_run_fanout" || strings.Contains(lowerMsg, "free_mode_run_fanout"):
		return "Upstream refused the account's concurrent agent runs (proxy-fanout signal). Honor Retry-After; run fewer parallel requests per token, or add another token."
	case code == "free_mode_invalid_agent_model" || strings.Contains(lowerMsg, "free_mode_invalid_agent_model"):
		return "The model is not in upstream's free-mode allowlist (retired id or stale registry). Wait for the registry refresh; if it persists, remove the model from MODELS_ALLOW and update."
	case code == "free_mode_capacity_deferred" || strings.Contains(lowerMsg, "free_mode_capacity_deferred"):
		return "Free tier at capacity — request deferred. Honor Retry-After (approx 2s for 30m window, 10s default) before retrying."
	case code == "account_banned" || strings.Contains(lowerMsg, "banned"):
		return "Account suspended upstream. Token is dead; create a fresh account with an established GitHub login."
	case code == "country_blocked" || strings.Contains(lowerMsg, "country blocked") || strings.Contains(lowerMsg, "country_blocked"):
		return "Your egress IP is in an unsupported region. Route traffic through an allowed country (e.g. US/EU/ID/SG)."
	case code == "out_of_credits" || strings.Contains(lowerMsg, "out of credits"):
		return "Upstream free-tier credits exhausted. Check COST_MODE in .env — valid values are free or unset; any other value fails startup validation."
	case code == "upstream_timeout":
		return "The upstream request exceeded its deadline. Retry, or raise REQUEST_TIMEOUT/SESSION_CALL_TIMEOUT in .env."
	case code == "upstream_auth_rejected" || code == "invalid_api_key" || strings.Contains(lowerMsg, "invalid api key"):
		return "Token invalid or expired. Get a fresh token by running scripts/gen-token.cmd (Windows) or scripts/gen-token.sh (Linux/macOS)"
	case code == "rate_limited":
		return "Upstream refused the request (rate limit). Honor Retry-After before retrying; persistent refusals mean the account's upstream pool is spent."
	case code == "model_ip_limited":
		return "Model restricted on this egress IP/tier. Limited-tier accounts should switch to 'mimo/mimo-v2.5', or route traffic through a Tier-1 country (US/EU/SG)."
	case code == "ip_capped":
		return "Too many distinct users on this egress IP (admission-only). Retry after Retry-After or use a different egress."
	case code == "load_shedding":
		return "Upstream load shedding — transient minutes-scale saturation. Retry after ~90s."
	case code == "peak_hours":
		return "Premium peak-hours window — transient. Retry after ~30m."
	case code == "missing_bearer_token":
		return "Bridge mode active: pass your FreeBuff token in Authorization: Bearer <token>"
	case code == "strict_violation":
		return "A tool declared strict:true but its schema does not meet the strict contract: parameters must be type object with every property listed in required and additionalProperties false."
	case code == "invalid_tool_arguments":
		return "The model returned unusable arguments for a tool declared strict:true. Retry the turn; a strict tool's arguments must be a JSON object."
	case code == "model_not_found":
		return "Check available models via GET /v1/models"
	default:
		return ""
	}
}

// chatErrClass buckets an upstream error into the trace error column. A
// canceled downstream client gets its own bucket: the access line keeps a
// 200 default when nothing was (or could be) written, so a generic "error"
// would render a context-free "ERROR 200" on the dashboard.
func chatErrClass(err error) string {
	// The pool's acquire-time 429 wraps the binding refusal with token
	// attribution: classify the inner refusal so the trace error column
	// still reads rate_limited.
	err = unwrapAcquireRateLimit(err)
	if errors.Is(err, context.Canceled) {
		return "client_canceled"
	}
	switch err.(type) {
	case *upstream.RateLimitError:
		return "rate_limited"
	case *upstream.BanError:
		return "banned"
	case *upstream.IpCappedError:
		return "ip_capped"
	case *upstream.LimitedIpError:
		return "model_ip_limited"
	case *upstream.SessionLimitError:
		return "session_limit_reached"
	case *upstream.WaitingRoomError, *session.WaitingRoomError, *upstream.WaitingRoomRequiredError:
		return "waiting_room"
	case *upstream.SessionSupersededError:
		return "session_superseded"
	case *upstream.TurnSpendLimitError:
		return "turn_spend_limited"
	case *upstream.NoEndpointsError:
		return "model_no_endpoints"
	case *upstream.FreeModeUnavailableError:
		return "free_mode_unavailable"
	case *upstream.ProviderUsageError:
		return "provider_usage_exhausted"
	case *upstream.ConsentRequiredError:
		return "consent_required"
	case *upstream.FirstTabChangedError:
		return "first_tab_discount_changed"
	case *upstream.UpstreamError:
		return "upstream"
	default:
		return "error"
	}
}

// attemptStatus extracts the upstream HTTP status carried by a chat error,
// or 0 when the error carries none (wrapped sentinels such as
// ErrSessionInvalid/ErrRunInvalid, and transport-level failures). A 0 is
// skipped in statuses_seen — only observed statuses are listed.
func attemptStatus(err error) int {
	err = unwrapAcquireRateLimit(err)
	switch e := err.(type) {
	case *upstream.UpstreamError:
		return e.Status
	case *upstream.CreditsError:
		return e.Status
	case *upstream.CapacityDeferredError:
		return e.Status
	case *upstream.SessionSupersededError:
		return e.Status
	case *upstream.TurnSpendLimitError:
		return e.Status
	case *upstream.SessionLimitError:
		return e.Status
	case *upstream.NoEndpointsError:
		return e.Status
	case *upstream.WaitingRoomRequiredError:
		// The canonical 428 waiting_room_required (#94); the marker can
		// ride 428/429 alike, 428 is the documented gate. No named
		// net/http constant exists for 428, so spell it out.
		return 428
	case *upstream.FreeModeUnavailableError:
		return e.Status
	case *upstream.ProviderUsageError:
		return e.Status
	case *upstream.ConsentRequiredError:
		return e.Status
	case *upstream.FirstTabChangedError:
		return e.Status
	case *upstream.RateLimitError:
		// RateLimitError.Status is the upstream "429" string; parse when
		// numeric, else the 429 bucket is implicit.
		if n, perr := strconv.Atoi(e.Status); perr == nil {
			return n
		}
		return http.StatusTooManyRequests
	}
	return 0
}

// unwrapAcquireRateLimit strips the pool's acquire-time attribution wrapper
// so type-switch classifiers see the underlying refusal. Non-wrapped
// errors pass through untouched; a wrapper with no inner refusal yields
// the wrapper itself (still a rate-limit signal via errors.Is).
func unwrapAcquireRateLimit(err error) error {
	var are *pool.AcquireRateLimitedError
	if errors.As(err, &are) && are != nil && are.Err != nil {
		return are.Err
	}
	return err
}

// quotaSummary renders the live per-model session quota from a probe's
// isAnthropicRequest reports whether the incoming request is destined for the
// Anthropic Messages surface (/v1/messages) or carries Anthropic headers.
func isAnthropicRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	if strings.HasPrefix(r.URL.Path, "/v1/messages") {
		return true
	}
	if r.Header.Get("anthropic-version") != "" || r.Header.Get("anthropic-api-key") != "" {
		return true
	}
	return false
}

// anthropicErrorType maps HTTP status code and internal error code to standard
// Anthropic error types per reference/protocols/anthropic-sdk-typescript.
func anthropicErrorType(status int, code string) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == http.StatusServiceUnavailable && (code == "waiting_room_queued" || code == "waiting_room_required" || code == "capacity_deferred"):
		return "overloaded_error"
	case status >= 500:
		return "api_error"
	default:
		return "invalid_request_error"
	}
}
